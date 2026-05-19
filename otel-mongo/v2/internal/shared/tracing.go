// Package shared holds helpers used by both the public otelmongo facade and
// the internal/traced impl subpackage. Kept under internal/ so it remains
// unimportable from outside the otelmongo v2 module.
package shared

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceMetadataKey is the BSON field name used to store trace metadata in documents.
const TraceMetadataKey = "_oteltrace"

// TraceMetadata holds the W3C Trace Context fields stored alongside a MongoDB document.
type TraceMetadata struct {
	Traceparent string `bson:"traceparent"`
	Tracestate  string `bson:"tracestate,omitempty"`
}

// traceMetadataFromContext extracts W3C trace context from ctx into TraceMetadata using prop.
// Returns ok=false when ctx carries no valid SpanContext, or when the parent
// SpanContext is not sampled — unsampled writes do not embed _oteltrace so
// downstream consumers see "no trace" rather than "trace present but inert",
// and the document avoids the ~100 byte propagation overhead.
//
// Uses a stack-allocated fixedCarrier so the hot path performs zero map
// allocations and the returned value need not escape to the heap.
func traceMetadataFromContext(ctx context.Context, prop propagation.TextMapPropagator) (TraceMetadata, bool) {
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if !spanCtx.IsValid() || !spanCtx.IsSampled() {
		return TraceMetadata{}, false
	}
	var c fixedCarrier
	prop.Inject(ctx, &c)
	return TraceMetadata{Traceparent: c.traceparent, Tracestate: c.tracestate}, true
}

// InjectTraceIntoDocument marshals document to bson.D and, when the span context in ctx is valid,
// appends an "_oteltrace" field.
//
// Fast path: for bson.D / bson.M / map[string]any inputs the document is
// shallow-cloned into a fresh bson.D and (optionally) appended with the trace
// metadata entry — no BSON round-trip, no reflection. Struct and other inputs
// fall back to the original Marshal/Unmarshal path so behaviour is unchanged
// for callers that rely on driver-side type normalisation.
//
// IMPORTANT invariant: callers' original document must NOT be mutated, and the
// returned bson.D must NOT share a backing array with the caller's slice. Both
// requirements are enforced by always allocating a new bson.D in the fast
// paths below.
func InjectTraceIntoDocument(ctx context.Context, document any, prop propagation.TextMapPropagator) (bson.D, error) {
	meta, hasMeta := traceMetadataFromContext(ctx, prop)

	switch d := document.(type) {
	case bson.D:
		out := make(bson.D, len(d), len(d)+1)
		copy(out, d)
		if hasMeta {
			out = append(out, bson.E{Key: TraceMetadataKey, Value: meta})
		}
		return out, nil

	case bson.M:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		if hasMeta {
			out = append(out, bson.E{Key: TraceMetadataKey, Value: meta})
		}
		return out, nil

	case map[string]any:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		if hasMeta {
			out = append(out, bson.E{Key: TraceMetadataKey, Value: meta})
		}
		return out, nil
	}

	// Fallback: struct / pointer / custom types — preserve previous behaviour
	// (driver-side normalisation via Marshal/Unmarshal round-trip).
	raw, err := bson.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: marshal document: %w", err)
	}
	doc := make(bson.D, 0, 1)
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("otelmongo: unmarshal document: %w", err)
	}
	if hasMeta {
		doc = append(doc, bson.E{Key: TraceMetadataKey, Value: meta})
	}
	return doc, nil
}

// ExtractMetadataFromRaw looks up the "_oteltrace" field in raw and reads its
// traceparent / tracestate string members directly via BSON byte lookups —
// avoids reflection-based bson.Unmarshal so the per-document overhead in heavy
// read paths (cursor decode, change stream) stays at zero allocations.
//
// Returns a value (16 bytes) so escape analysis can keep it on the caller's
// stack — the previous pointer-returning signature forced a heap allocation
// per event.
func ExtractMetadataFromRaw(raw bson.Raw) (TraceMetadata, bool) {
	val, err := raw.LookupErr(TraceMetadataKey)
	if err != nil {
		return TraceMetadata{}, false
	}
	sub, ok := val.DocumentOK()
	if !ok {
		return TraceMetadata{}, false
	}
	tp, ok := sub.Lookup("traceparent").StringValueOK()
	if !ok || tp == "" {
		return TraceMetadata{}, false
	}
	ts, _ := sub.Lookup("tracestate").StringValueOK()
	return TraceMetadata{Traceparent: tp, Tracestate: ts}, true
}

// ExtractMetadataFromMap reads "_oteltrace" out of a bson.M / map[string]any
// outer container and parses its inner shape via MetadataFromValue.
func ExtractMetadataFromMap(m bson.M) (TraceMetadata, bool) {
	v, ok := m[TraceMetadataKey]
	if !ok {
		return TraceMetadata{}, false
	}
	return MetadataFromValue(v)
}

// ExtractMetadataFromBsonD reads "_oteltrace" out of a bson.D outer container.
// InjectTraceIntoDocument always appends the metadata entry last, so scanning
// in reverse turns the typical lookup into an O(1) hit on round-tripped docs.
func ExtractMetadataFromBsonD(d bson.D) (TraceMetadata, bool) {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i].Key == TraceMetadataKey {
			return MetadataFromValue(d[i].Value)
		}
	}
	return TraceMetadata{}, false
}

// MetadataFromValue parses an already-located _oteltrace value. Accepts only
// the four contract shapes that InjectTraceIntoDocument can produce
// (TraceMetadata in-process, plus bson.D / bson.M / map[string]any after a
// BSON round-trip); any other shape returns false because the "_oteltrace"
// key is owned exclusively by otelmongo.
func MetadataFromValue(v any) (TraceMetadata, bool) {
	switch sub := v.(type) {
	case TraceMetadata:
		if sub.Traceparent == "" {
			return TraceMetadata{}, false
		}
		return sub, true
	case bson.M:
		return metadataFromInnerMap(sub)
	case map[string]any:
		return metadataFromInnerMap(sub)
	case bson.D:
		return metadataFromInnerD(sub)
	}
	return TraceMetadata{}, false
}

func metadataFromInnerMap(m map[string]any) (TraceMetadata, bool) {
	tp, _ := m["traceparent"].(string)
	if tp == "" {
		return TraceMetadata{}, false
	}
	ts, _ := m["tracestate"].(string)
	return TraceMetadata{Traceparent: tp, Tracestate: ts}, true
}

func metadataFromInnerD(d bson.D) (TraceMetadata, bool) {
	var tp, ts string
	for _, e := range d {
		switch e.Key {
		case "traceparent":
			tp, _ = e.Value.(string)
		case "tracestate":
			ts, _ = e.Value.(string)
		}
	}
	if tp == "" {
		return TraceMetadata{}, false
	}
	return TraceMetadata{Traceparent: tp, Tracestate: ts}, true
}

// ContextFromTraceMetadata injects the remote span context encoded in meta into ctx using prop.
// Uses a stack-allocated fixedCarrier to avoid the map allocation that
// propagation.MapCarrier would require on every read-path event.
func ContextFromTraceMetadata(ctx context.Context, meta TraceMetadata, prop propagation.TextMapPropagator) context.Context {
	c := fixedCarrier{traceparent: meta.Traceparent, tracestate: meta.Tracestate}
	return prop.Extract(ctx, &c)
}

// SpanContextFromMetadata builds a trace.SpanContext directly from meta without
// constructing a discardable context.Context chain. Read-path callers
// (Cursor/ChangeStream/SingleResult) only need the SpanContext for span links;
// the previous ContextFromTraceMetadata(context.Background(), ...) path forced
// allocation of a one-shot context.WithValue chain that was immediately
// discarded.
func SpanContextFromMetadata(meta TraceMetadata, prop propagation.TextMapPropagator) trace.SpanContext {
	c := fixedCarrier{traceparent: meta.Traceparent, tracestate: meta.Tracestate}
	return trace.SpanContextFromContext(prop.Extract(context.Background(), &c))
}

// SpanContextFromMetadataCtx is the ctx-aware variant of SpanContextFromMetadata.
// It threads the caller's ctx through prop.Extract so static analysers
// (e.g. contextcheck) see the parameter being consumed; functionally
// equivalent to SpanContextFromMetadata because only the embedded
// SpanContext is returned and any other ctx values are discarded.
func SpanContextFromMetadataCtx(ctx context.Context, meta TraceMetadata, prop propagation.TextMapPropagator) trace.SpanContext {
	c := fixedCarrier{traceparent: meta.Traceparent, tracestate: meta.Tracestate}
	return trace.SpanContextFromContext(prop.Extract(ctx, &c))
}

// InjectTraceIntoUpdate inspects update and embeds trace metadata when ctx carries a valid span context.
//
// Fast path: same shape as InjectTraceIntoDocument — bson.D / bson.M /
// map[string]any inputs are cloned before mutation so caller's update is never
// touched and no backing array is shared with the returned slice.
func InjectTraceIntoUpdate(ctx context.Context, update any, prop propagation.TextMapPropagator) (any, error) {
	meta, ok := traceMetadataFromContext(ctx, prop)
	if !ok {
		return update, nil
	}

	doc, err := updateToClonedBsonD(update)
	if err != nil {
		return update, err
	}

	if len(doc) > 0 && doc[0].Key != "" && doc[0].Key[0] == '$' {
		doc, err = upsertSetField(doc, meta)
		if err != nil {
			return update, fmt.Errorf("otelmongo: upsert $set: %w", err)
		}
		return doc, nil
	}

	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: meta})
	return doc, nil
}

// updateToClonedBsonD converts update into a fresh bson.D, never reusing
// caller-owned slice/map backing storage. Fast paths cover bson.D / bson.M /
// map[string]any; everything else falls back to Marshal/Unmarshal so behaviour
// is unchanged for struct callers.
func updateToClonedBsonD(update any) (bson.D, error) {
	switch d := update.(type) {
	case bson.D:
		out := make(bson.D, len(d), len(d)+1)
		copy(out, d)
		return out, nil
	case bson.M:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		return out, nil
	case map[string]any:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		return out, nil
	}
	raw, err := bson.Marshal(update)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: marshal update: %w", err)
	}
	doc := make(bson.D, 0, 1)
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("otelmongo: unmarshal update: %w", err)
	}
	return doc, nil
}

func upsertSetField(doc bson.D, meta TraceMetadata) (bson.D, error) {
	foundSet := false
	for i, elem := range doc {
		if elem.Key != "$set" && elem.Key != "$setOnInsert" {
			continue
		}
		subDoc, err := cloneSetValueAsBsonD(elem.Key, elem.Value)
		if err != nil {
			return doc, err
		}
		subDoc = append(subDoc, bson.E{Key: TraceMetadataKey, Value: meta})
		doc[i].Value = subDoc
		if elem.Key == "$set" {
			foundSet = true
		}
	}
	if !foundSet {
		doc = append(doc, bson.E{Key: "$set", Value: bson.D{{Key: TraceMetadataKey, Value: meta}}})
	}
	return doc, nil
}

// cloneSetValueAsBsonD copies the value of a $set / $setOnInsert operator into
// a fresh bson.D with one slot of headroom for the trace metadata key. Always
// allocates new backing storage so the returned bson.D never aliases caller-
// owned slices, maps, or BSON documents — required because the caller may pass
// a shallow clone whose elem.Value still points at user storage. The marshal /
// unmarshal fallback handles arbitrary struct / map[string]string / pointer
// shapes that the fast-path type switch cannot cover.
func cloneSetValueAsBsonD(key string, value any) (bson.D, error) {
	switch v := value.(type) {
	case bson.D:
		out := make(bson.D, len(v), len(v)+1)
		copy(out, v)
		return out, nil
	case bson.M:
		out := make(bson.D, 0, len(v)+1)
		for k, vv := range v {
			out = append(out, bson.E{Key: k, Value: vv})
		}
		return out, nil
	case map[string]any:
		out := make(bson.D, 0, len(v)+1)
		for k, vv := range v {
			out = append(out, bson.E{Key: k, Value: vv})
		}
		return out, nil
	default:
		raw, err := bson.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal %s value: %w", key, err)
		}
		var out bson.D
		if err := bson.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("unmarshal %s value: %w", key, err)
		}
		return out, nil
	}
}

// injectMetadataIntoDocument is the inner half of InjectTraceIntoDocument that
// reuses an already-prepared TraceMetadata. Used by BulkWrite so a single
// propagator.Inject suffices for all N models.
func injectMetadataIntoDocument(document any, meta TraceMetadata) (bson.D, error) {
	switch d := document.(type) {
	case bson.D:
		out := make(bson.D, len(d), len(d)+1)
		copy(out, d)
		out = append(out, bson.E{Key: TraceMetadataKey, Value: meta})
		return out, nil
	case bson.M:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		out = append(out, bson.E{Key: TraceMetadataKey, Value: meta})
		return out, nil
	case map[string]any:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		out = append(out, bson.E{Key: TraceMetadataKey, Value: meta})
		return out, nil
	}
	raw, err := bson.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: marshal document: %w", err)
	}
	doc := make(bson.D, 0, 1)
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("otelmongo: unmarshal document: %w", err)
	}
	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: meta})
	return doc, nil
}

// injectMetadataIntoUpdate is the inner half of InjectTraceIntoUpdate that
// reuses an already-prepared TraceMetadata. Used by BulkWrite so a single
// propagator.Inject suffices for all N models.
func injectMetadataIntoUpdate(update any, meta TraceMetadata) (any, error) {
	doc, err := updateToClonedBsonD(update)
	if err != nil {
		return update, err
	}
	if len(doc) > 0 && doc[0].Key != "" && doc[0].Key[0] == '$' {
		doc, err = upsertSetField(doc, meta)
		if err != nil {
			return update, fmt.Errorf("otelmongo: upsert $set: %w", err)
		}
		return doc, nil
	}
	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: meta})
	return doc, nil
}
