package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// InsertOneResult wraps *mongo.InsertOneResult. Use when calling Collection.InsertOne.
type InsertOneResult struct {
	*mongo.InsertOneResult
}

// InsertManyResult wraps *mongo.InsertManyResult. Use when calling Collection.InsertMany.
type InsertManyResult struct {
	*mongo.InsertManyResult
}

// UpdateResult wraps *mongo.UpdateResult. Use when calling UpdateOne, UpdateMany, ReplaceOne, UpdateByID.
type UpdateResult struct {
	*mongo.UpdateResult
}

// DeleteResult wraps *mongo.DeleteResult. Use when calling DeleteOne, DeleteMany.
type DeleteResult struct {
	*mongo.DeleteResult
}

// BulkWriteResult wraps *mongo.BulkWriteResult. Use when calling Collection.BulkWrite.
type BulkWriteResult struct {
	*mongo.BulkWriteResult
}

// ChangeStream wraps *mongo.ChangeStream. Use when calling Collection.Watch.
// Use DecodeWithContext to automatically restore trace context from fullDocument,
// or ContextFromDocument(ctx, event.FullDocument) for manual extraction.
type ChangeStream struct {
	*mongo.ChangeStream
	tracer             trace.Tracer                  // consumer-side app tracer
	propagator         propagation.TextMapPropagator // for extracting trace from documents
	propagationEnabled bool
	spanName           string
	baseSpanOpts       []trace.SpanStartOption
	deliverTracer      trace.Tracer         // nil when disabled
	deliverSpanName    string               // e.g. "messaging.messages deliver"
	deliverAttrs       []attribute.KeyValue // same attrs as producer-side deliver span
}

// Next advances the change stream to the next change document. See *mongo.ChangeStream.Next.
func (cs *ChangeStream) Next(ctx context.Context) bool {
	return cs.ChangeStream.Next(ctx)
}

// Decode decodes the current change document into val. See *mongo.ChangeStream.Decode.
func (cs *ChangeStream) Decode(val any) error {
	return cs.ChangeStream.Decode(val)
}

// DecodeWithContext decodes the current change document into val and returns a
// context enriched with trace context extracted from fullDocument's "_oteltrace"
// field. When the field is absent (e.g. delete events) or invalid, the returned
// context is unchanged. The val parameter can be any user-defined struct — it
// does not need a fullDocument field; extraction uses the raw BSON internally.
func (cs *ChangeStream) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	// Extract origin trace context from this change event's fullDocument._oteltrace.
	var originSpanCtx trace.SpanContext

	fullDoc, err := cs.Current.LookupErr("fullDocument")
	if err == nil && cs.propagationEnabled {
		docRaw, ok := fullDoc.DocumentOK()
		if ok {
			if meta, ok := extractMetadataFromRaw(docRaw); ok {
				// Keep it separate from `ctx` so the created span stays "link-only".
				originCtx := contextFromTraceMetadata(context.Background(), meta, cs.propagator)
				originSpanCtx = trace.SpanContextFromContext(originCtx)
			}
		}
	}

	// Detach any incoming parent span so consumer spans get a new TraceID.
	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	consumerCtx := detachedCtx

	// Consumer-side deliver span: SpanKindProducer (broker delivering to consumer), links to producer deliver A.
	// Only created when deliverTracer is set and there is a valid origin span context.
	if cs.deliverTracer != nil && originSpanCtx.IsValid() {
		_, deliverSpan := cs.deliverTracer.Start(detachedCtx,
			cs.deliverSpanName,
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(cs.deliverAttrs...),
			trace.WithLinks(trace.Link{SpanContext: originSpanCtx}),
		)
		deliverSpan.End()
		// Consumer span becomes child of deliver span (shared new TraceID).
		consumerCtx = trace.ContextWithRemoteSpanContext(detachedCtx, deliverSpan.SpanContext())
	}

	spanOpts := append([]trace.SpanStartOption{}, cs.baseSpanOpts...)
	if originSpanCtx.IsValid() {
		spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}

	newCtx, span := cs.tracer.Start(consumerCtx, cs.spanName, spanOpts...)

	if err := cs.ChangeStream.Decode(val); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return ctx, err
	}

	span.End()
	return newCtx, nil
}

// Close closes the change stream. See *mongo.ChangeStream.Close.
func (cs *ChangeStream) Close(ctx context.Context) error {
	return cs.ChangeStream.Close(ctx)
}

// Err returns the last error. See *mongo.ChangeStream.Err.
func (cs *ChangeStream) Err() error {
	return cs.ChangeStream.Err()
}
