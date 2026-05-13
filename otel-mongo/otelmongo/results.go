package otelmongo

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
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

// SingleResult wraps *mongo.SingleResult with trace propagation. When the
// tracing gate is off (tracingEnabled=false) Decode/TraceContext/Raw act as
// passthroughs and the span field stays nil so endSpan is a no-op.
type SingleResult struct {
	*mongo.SingleResult
	span               trace.Span
	ctx                context.Context
	propagator         propagation.TextMapPropagator
	tracingEnabled     bool
	propagationEnabled bool
	endOnce            sync.Once
}

// endSpan ensures the associated span is ended exactly once. Nil-safe.
func (r *SingleResult) endSpan() {
	r.endOnce.Do(func() {
		if r.span != nil {
			r.span.End()
		}
	})
}

// Decode decodes the document and (when tracing is on) records any stored
// trace context as a span link on the FindOne span before ending it. The
// span is ended exactly once.
func (r *SingleResult) Decode(v any) error {
	defer r.endSpan()
	if !r.tracingEnabled {
		return r.SingleResult.Decode(v)
	}
	raw, err := r.SingleResult.Raw()
	if err != nil {
		recordSpanError(r.span, err)
		return err
	}
	if r.propagationEnabled {
		if meta, ok := extractMetadataFromRaw(raw); ok {
			originCtx := contextFromTraceMetadata(context.Background(), meta, r.propagator)
			originSpanCtx := trace.SpanContextFromContext(originCtx)
			if originSpanCtx.IsValid() {
				r.span.AddLink(trace.Link{SpanContext: originSpanCtx})
			}
		}
	}
	return r.SingleResult.Decode(v)
}

// TraceContext returns a context enriched with the trace context stored in
// the fetched document's "_oteltrace" field. Must be called after Decode or Raw.
func (r *SingleResult) TraceContext() context.Context {
	defer r.endSpan()
	if !r.tracingEnabled {
		return r.ctx
	}
	raw, err := r.SingleResult.Raw()
	if err != nil {
		return r.ctx
	}
	if r.propagationEnabled {
		if meta, ok := extractMetadataFromRaw(raw); ok {
			return contextFromTraceMetadata(r.ctx, meta, r.propagator)
		}
	}
	return r.ctx
}

// Raw returns the raw BSON document and ends the span exactly once.
func (r *SingleResult) Raw() (bson.Raw, error) {
	defer r.endSpan()
	return r.SingleResult.Raw()
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

// Close closes the change stream.
func (cs *ChangeStream) Close(ctx context.Context) error {
	return cs.ChangeStream.Close(ctx)
}

// Err returns the last error.
func (cs *ChangeStream) Err() error {
	return cs.ChangeStream.Err()
}

// buildConsumerCtx creates a detached context with a consumer span linked to
// originSpanCtx. When deliverTracer is non-nil and originSpanCtx is valid, a
// consumer-side deliver span (SpanKindProducer) is created first and the
// consumer span becomes its child. Extracted for testability.
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
