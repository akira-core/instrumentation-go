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
func InjectTraceIntoDocument(ctx context.Context, document any, prop propagation.TextMapPropagator) (bson.D, error) {
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

// ExtractMetadataFromRaw looks up the "_oteltrace" field in raw and, when found,
// unmarshals it into a TraceMetadata.
func ExtractMetadataFromRaw(raw bson.Raw) (*TraceMetadata, bool) {
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
func InjectTraceIntoUpdate(ctx context.Context, update any, prop propagation.TextMapPropagator) (any, error) {
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
		doc, err = upsertSetField(doc, *meta)
		if err != nil {
			return update, fmt.Errorf("otelmongo: upsert $set: %w", err)
		}
		return doc, nil
	}

	doc = append(doc, bson.E{Key: TraceMetadataKey, Value: *meta})
	return doc, nil
}

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
		doc = append(doc, bson.E{Key: "$set", Value: bson.D{{Key: TraceMetadataKey, Value: meta}}})
	}
	return doc, nil
}
