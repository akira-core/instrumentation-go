package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// InsertOneResult wraps *mongo.InsertOneResult.
type InsertOneResult struct {
	*mongo.InsertOneResult
}

// InsertManyResult wraps *mongo.InsertManyResult.
type InsertManyResult struct {
	*mongo.InsertManyResult
}

// UpdateResult wraps *mongo.UpdateResult.
type UpdateResult struct {
	*mongo.UpdateResult
}

// DeleteResult wraps *mongo.DeleteResult.
type DeleteResult struct {
	*mongo.DeleteResult
}

// BulkWriteResult wraps *mongo.BulkWriteResult.
type BulkWriteResult struct {
	*mongo.BulkWriteResult
}

// ChangeStream wraps *mongo.ChangeStream. When tracingEnabled is false (gate
// off) DecodeWithContext is a passthrough — no spans, no _oteltrace extract.
type ChangeStream struct {
	*mongo.ChangeStream
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	tracingEnabled     bool
	propagationEnabled bool
	spanName           string
	baseSpanOpts       []trace.SpanStartOption
	deliverTracer      trace.Tracer
	deliverSpanName    string
	deliverAttrs       []attribute.KeyValue
}

// Next advances the change stream to the next change document.
func (cs *ChangeStream) Next(ctx context.Context) bool {
	return cs.ChangeStream.Next(ctx)
}

// Decode decodes the current change document into val.
func (cs *ChangeStream) Decode(val any) error {
	return cs.ChangeStream.Decode(val)
}

// DecodeWithContext decodes the current change document into val and returns
// a context enriched with trace context extracted from fullDocument's
// "_oteltrace" field. When the field is absent (e.g. delete events) or
// tracing is off, the returned context is unchanged.
func (cs *ChangeStream) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	if !cs.tracingEnabled {
		if err := cs.ChangeStream.Decode(val); err != nil {
			return ctx, err
		}
		return ctx, nil
	}
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

	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	consumerCtx := detachedCtx

	if cs.deliverTracer != nil && originSpanCtx.IsValid() {
		_, deliverSpan := cs.deliverTracer.Start(detachedCtx,
			cs.deliverSpanName,
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(cs.deliverAttrs...),
			trace.WithLinks(trace.Link{SpanContext: originSpanCtx}),
		)
		deliverSpan.End()
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

// Close closes the change stream.
func (cs *ChangeStream) Close(ctx context.Context) error {
	return cs.ChangeStream.Close(ctx)
}

// Err returns the last error.
func (cs *ChangeStream) Err() error {
	return cs.ChangeStream.Err()
}
