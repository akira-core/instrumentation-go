// Package shared holds helpers used by both the public otelmongo facade and
// the internal/traced impl subpackage. Kept under internal/ so it remains
// unimportable from outside the otelmongo module.
package shared

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
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
func traceMetadataFromContext(ctx context.Context, prop propagation.TextMapPropagator) (*TraceMetadata, bool) {
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if !spanCtx.IsValid() {
		return nil, false
	}
	carrier := propagation.MapCarrier{}
	prop.Inject(ctx, carrier)
	return &TraceMetadata{
		Traceparent: carrier.Get("traceparent"),
		Tracestate:  carrier.Get("tracestate"),
	}, true
}

// InjectTraceIntoDocument marshals document to bson.D and, when the span context in ctx is valid,
// appends an "_oteltrace" field. The original document is not modified.
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
			out = append(out, bson.E{Key: TraceMetadataKey, Value: *meta})
		}
		return out, nil

	case bson.M:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		if hasMeta {
			out = append(out, bson.E{Key: TraceMetadataKey, Value: *meta})
		}
		return out, nil

	case map[string]any:
		out := make(bson.D, 0, len(d)+1)
		for k, v := range d {
			out = append(out, bson.E{Key: k, Value: v})
		}
		if hasMeta {
			out = append(out, bson.E{Key: TraceMetadataKey, Value: *meta})
		}
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
	if hasMeta {
		doc = append(doc, bson.E{Key: TraceMetadataKey, Value: *meta})
	}
	return doc, nil
}

// ExtractMetadataFromRaw looks up the "_oteltrace" field in raw and reads its
// traceparent / tracestate string members directly via BSON byte lookups —
// avoids reflection-based bson.Unmarshal so the per-document overhead in heavy
// read paths (cursor decode, change stream) stays at zero allocations.
func ExtractMetadataFromRaw(raw bson.Raw) (*TraceMetadata, bool) {
	val, err := raw.LookupErr(TraceMetadataKey)
	if err != nil {
		return nil, false
	}
	sub, ok := val.DocumentOK()
	if !ok {
		return nil, false
	}
	tp, ok := sub.Lookup("traceparent").StringValueOK()
	if !ok || tp == "" {
		return nil, false
	}
	ts, _ := sub.Lookup("tracestate").StringValueOK()
	return &TraceMetadata{Traceparent: tp, Tracestate: ts}, true
}

// ContextFromTraceMetadata injects the remote span context encoded in meta into ctx using prop.
func ContextFromTraceMetadata(ctx context.Context, meta *TraceMetadata, prop propagation.TextMapPropagator) context.Context {
	carrier := propagation.MapCarrier{
		"traceparent": meta.Traceparent,
	}
	if meta.Tracestate != "" {
		carrier["tracestate"] = meta.Tracestate
	}
	return prop.Extract(ctx, carrier)
}

// InjectTraceIntoUpdate inspects update, and when ctx carries a valid span context,
// embeds the trace metadata. For operator updates the metadata is added to "$set".
// For replacement documents the metadata is appended as a top-level field.
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

	if len(doc) > 0 && len(doc[0].Key) > 0 && doc[0].Key[0] == '$' {
		doc, err = upsertSetField(doc, *meta)
		if err != nil {
			return update, fmt.Errorf("otelmongo: upsert $set: %w", err)
		}
		return doc, nil
	}

	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: *meta})
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
		// Always allocate a fresh bson.D for the sub-doc so we never mutate the
		// caller's bson.D / bson.M sub-value (the fast path makes this load-
		// bearing: doc may be a shallow clone of caller's update and elem.Value
		// could still point at caller-owned storage).
		var subDoc bson.D
		switch v := elem.Value.(type) {
		case bson.D:
			subDoc = make(bson.D, len(v), len(v)+1)
			copy(subDoc, v)
		case bson.M:
			subDoc = make(bson.D, 0, len(v)+1)
			for k, vv := range v {
				subDoc = append(subDoc, bson.E{Key: k, Value: vv})
			}
		case map[string]any:
			subDoc = make(bson.D, 0, len(v)+1)
			for k, vv := range v {
				subDoc = append(subDoc, bson.E{Key: k, Value: vv})
			}
		default:
			raw, err := bson.Marshal(v)
			if err != nil {
				return doc, fmt.Errorf("marshal %s value: %w", elem.Key, err)
			}
			if err := bson.Unmarshal(raw, &subDoc); err != nil {
				return doc, fmt.Errorf("unmarshal %s value: %w", elem.Key, err)
			}
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
