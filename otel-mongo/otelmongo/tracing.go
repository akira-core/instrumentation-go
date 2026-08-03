// Package otelmongo provides a MongoDB driver v1 wrapper that propagates
// OpenTelemetry trace contexts to and from documents stored in MongoDB.
// Trace metadata is stored in a reserved field named "_oteltrace" in each
// document, enabling full lifecycle tracing of data across services.
package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
)

// TraceMetadataKey is the BSON field name used to store trace metadata in documents.
const TraceMetadataKey = shared.TraceMetadataKey

// TraceMetadata holds the W3C Trace Context fields stored alongside a MongoDB document.
type TraceMetadata = shared.TraceMetadata

// ContextFromRawDocument returns a context enriched with trace context stored in
// raw document "_oteltrace". When metadata is absent or invalid, the original
// ctx is returned unchanged.
//
// It carries NO feature-flag gate — not the global kill switch, not the module
// environment variables, not the relay verdicts. It starts no span, builds no
// attributes, initialises no part of the OTel SDK and writes nothing anywhere:
// it reads a field out of a value the caller already holds and returns what it
// encodes. The flags exist to stop the library doing work on the caller's behalf
// as a side effect of a business operation; this does only the thing the caller
// invoked it for, so gating it would leave no way to express what calling it
// already expressed.
//
// Cursor.DecodeAndTrace and ChangeStream.DecodeAndTrace look similar and ARE
// gated, because each starts and ends a real span on every call. A revocation
// therefore stops those but does NOT stop extraction here — this pair is the
// supported way to keep trace linking while the library is silenced.
func ContextFromRawDocument(ctx context.Context, raw bson.Raw) context.Context {
	meta, ok := shared.ExtractMetadataFromRaw(raw)
	if !ok {
		return ctx
	}
	return shared.ContextFromTraceMetadata(ctx, meta, otel.GetTextMapPropagator())
}

// ContextFromDocument extracts span context from fullDoc._oteltrace and injects
// it into the provided ctx before reading the resulting span context. It returns
// (zero, false) when the field is absent or malformed.
//
// Like ContextFromRawDocument it carries no feature-flag gate; see that
// function for why, and for what a revocation does and does not stop.
func ContextFromDocument(ctx context.Context, fullDoc any) (trace.SpanContext, bool) {
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
