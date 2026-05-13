// Package otelmongo provides a MongoDB driver v2 wrapper that propagates
// OpenTelemetry trace contexts to and from documents stored in MongoDB.
// Trace metadata is stored in a reserved field named "_oteltrace" in each
// document, enabling full lifecycle tracing of data across services.
package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/shared"
)

// TraceMetadataKey is the BSON field name used to store trace metadata in documents.
const TraceMetadataKey = shared.TraceMetadataKey

// TraceMetadata holds the W3C Trace Context fields stored alongside a MongoDB document.
type TraceMetadata = shared.TraceMetadata

// ContextFromRawDocument returns a context enriched with trace context stored in
// raw document "_oteltrace". When document propagation is disabled, returns ctx unchanged.
func ContextFromRawDocument(ctx context.Context, raw bson.Raw) context.Context {
	if !cachedPropagationEnabled() {
		return ctx
	}
	meta, ok := shared.ExtractMetadataFromRaw(raw)
	if !ok {
		return ctx
	}
	return shared.ContextFromTraceMetadata(ctx, meta, otel.GetTextMapPropagator())
}

// ContextFromDocument extracts span context from fullDoc._oteltrace.
func ContextFromDocument(ctx context.Context, fullDoc any) (trace.SpanContext, bool) {
	if !cachedPropagationEnabled() {
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
