package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"
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

// buildConsumerCtx creates a detached context with a consumer span linked to originSpanCtx.
// When deliverTracer is non-nil and originSpanCtx is valid, a consumer-side deliver span
// (SpanKindProducer) is created first and the consumer span becomes its child.
// This helper is extracted for testability.
func buildConsumerCtx(ctx context.Context, tracer trace.Tracer, deliverTracer trace.Tracer, deliverSpanName string, deliverAttrs []attribute.KeyValue, spanName string, baseSpanOpts []trace.SpanStartOption, originSpanCtx trace.SpanContext) (context.Context, trace.Span) {
	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	consumerCtx := detachedCtx

	if deliverTracer != nil && originSpanCtx.IsValid() {
		_, deliverSpan := deliverTracer.Start(detachedCtx,
			deliverSpanName,
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(deliverAttrs...),
			trace.WithLinks(trace.Link{SpanContext: originSpanCtx}),
		)
		deliverSpan.End()
		consumerCtx = trace.ContextWithRemoteSpanContext(detachedCtx, deliverSpan.SpanContext())
	}

	spanOpts := append([]trace.SpanStartOption{}, baseSpanOpts...)
	if originSpanCtx.IsValid() {
		spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}

	return tracer.Start(consumerCtx, spanName, spanOpts...)
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
	var originSpanCtx trace.SpanContext

	fullDoc, err := cs.Current.LookupErr("fullDocument")
	if err == nil && cs.propagationEnabled {
		docRaw, ok := fullDoc.DocumentOK()
		if ok {
			if meta, ok := extractMetadataFromRaw(docRaw); ok {
				originCtx := contextFromTraceMetadata(context.Background(), meta, cs.propagator)
				originSpanCtx = trace.SpanContextFromContext(originCtx)
			}
		}
	}

	newCtx, span := buildConsumerCtx(ctx, cs.tracer, cs.deliverTracer, cs.deliverSpanName, cs.deliverAttrs, cs.spanName, cs.baseSpanOpts, originSpanCtx)

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
