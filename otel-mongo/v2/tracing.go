// Package otelmongo provides a MongoDB driver v2 wrapper that propagates
// OpenTelemetry trace contexts to and from documents stored in MongoDB.
// Trace metadata is stored in a reserved field named "_oteltrace" in each
// document, enabling full lifecycle tracing of data across services.
package otelmongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceMetadataKey is the BSON field name used to store trace metadata in documents.
const TraceMetadataKey = "_oteltrace"

// TraceMetadata holds the W3C Trace Context fields stored alongside a MongoDB document.
type TraceMetadata struct {
	// Traceparent holds the W3C traceparent header value.
	Traceparent string `bson:"traceparent"`
	// Tracestate holds the W3C tracestate header value (optional).
	Tracestate string `bson:"tracestate,omitempty"`
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

// injectTraceIntoDocument marshals document to bson.D and, when the span context in ctx is valid,
// appends an "_oteltrace" field. The original document is not modified.
func injectTraceIntoDocument(ctx context.Context, document any, prop propagation.TextMapPropagator) (bson.D, error) {
	raw, err := bson.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: marshal document: %w", err)
	}

	doc := make(bson.D, 0, 1)
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("otelmongo: unmarshal document: %w", err)
	}

	meta, ok := traceMetadataFromContext(ctx, prop)
	if !ok {
		return doc, nil
	}
	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: *meta})
	return doc, nil
}

// extractMetadataFromRaw looks up the "_oteltrace" field in raw and, when found,
// unmarshals it into a TraceMetadata. Returns (nil, false) when the field is absent
// or cannot be decoded.
func extractMetadataFromRaw(raw bson.Raw) (*TraceMetadata, bool) {
	val, err := raw.LookupErr(TraceMetadataKey)
	if err != nil {
		return nil, false
	}

	rawDoc, ok := val.DocumentOK()
	if !ok {
		return nil, false
	}

	var meta TraceMetadata
	if err := bson.Unmarshal(rawDoc, &meta); err != nil {
		return nil, false
	}
	if meta.Traceparent == "" {
		return nil, false
	}
	return &meta, true
}

// ContextFromRawDocument returns a context enriched with trace context stored in
// raw document "_oteltrace". When metadata is absent/invalid, the original ctx
// is returned unchanged.
// Uses otel.GetTextMapPropagator() (global, read-only). For isolated propagator use,
// pre-enrich ctx with the desired propagator before calling this function.
// When document propagation is disabled (same env gates as Collection write/read paths:
// OTEL_INSTRUMENTATION_GO_TRACING_ENABLED and OTEL_MONGO_PROPAGATION_ENABLED), returns ctx unchanged.
func ContextFromRawDocument(ctx context.Context, raw bson.Raw) context.Context {
	if !mongoPropagationEnabled() {
		return ctx
	}
	meta, ok := extractMetadataFromRaw(raw)
	if !ok {
		return ctx
	}
	return contextFromTraceMetadata(ctx, meta, otel.GetTextMapPropagator())
}

// ContextFromDocument extracts span context from fullDoc._oteltrace and injects
// it into the provided ctx before reading the resulting span context.
// Returns (zero, false) when metadata is absent/invalid or marshal fails.
// When document propagation is disabled (same env gates as Collection), returns (zero, false).
func ContextFromDocument(ctx context.Context, fullDoc any) (trace.SpanContext, bool) {
	if !mongoPropagationEnabled() {
		return trace.SpanContext{}, false
	}
	raw, err := bson.Marshal(fullDoc)
	if err != nil {
		return trace.SpanContext{}, false
	}
	originCtx := ContextFromRawDocument(ctx, raw)
	sc := trace.SpanContextFromContext(originCtx)
	if !sc.IsValid() {
		return trace.SpanContext{}, false
	}
	return sc, true
}

// contextFromTraceMetadata injects the remote span context encoded in meta into ctx using prop.
func contextFromTraceMetadata(ctx context.Context, meta *TraceMetadata, prop propagation.TextMapPropagator) context.Context {
	carrier := propagation.MapCarrier{
		"traceparent": meta.Traceparent,
	}
	if meta.Tracestate != "" {
		carrier["tracestate"] = meta.Tracestate
	}
	return prop.Extract(ctx, carrier)
}

// injectTraceIntoUpdate inspects update, and when ctx carries a valid span context,
// embeds the trace metadata.
//   - For operator updates (first key starts with "$") the metadata is added to "$set".
//   - For replacement documents the metadata is appended as a top-level field.
func injectTraceIntoUpdate(ctx context.Context, update any, prop propagation.TextMapPropagator) (any, error) {
	meta, ok := traceMetadataFromContext(ctx, prop)
	if !ok {
		return update, nil
	}

	raw, err := bson.Marshal(update)
	if err != nil {
		return update, fmt.Errorf("otelmongo: marshal update: %w", err)
	}

	doc := make(bson.D, 0, 1)
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return update, fmt.Errorf("otelmongo: unmarshal update: %w", err)
	}

	if len(doc) > 0 && len(doc[0].Key) > 0 && doc[0].Key[0] == '$' {
		// Operator update: inject into $set.
		doc, err = upsertSetField(doc, *meta)
		if err != nil {
			return update, fmt.Errorf("otelmongo: upsert $set: %w", err)
		}
		return doc, nil
	}

	// Replacement document: append as top-level field (same as injectTraceIntoDocument).
	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: *meta})
	return doc, nil
}

// upsertSetField finds or creates the "$set" element in an operator update document
// and appends the trace metadata key to it. When "$setOnInsert" is present it is also
// annotated so that documents created via upsert carry the trace context.
func upsertSetField(doc bson.D, meta TraceMetadata) (bson.D, error) {
	foundSet := false
	for i, elem := range doc {
		if elem.Key != "$set" && elem.Key != "$setOnInsert" {
			continue
		}
		var subDoc bson.D
		switch v := elem.Value.(type) {
		case bson.D:
			subDoc = v
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
		// No existing $set — create one so existing documents are also annotated.
		doc = append(doc, bson.E{Key: "$set", Value: bson.D{{Key: TraceMetadataKey, Value: meta}}})
	}
	return doc, nil
}
