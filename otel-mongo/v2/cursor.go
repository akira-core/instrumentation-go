package otelmongo

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Cursor wraps *mongo.Cursor with trace propagation.
type Cursor struct {
	*mongo.Cursor
	parentCtx          context.Context
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
}

// DecodeWithContext decodes the current document into val and returns a context
// enriched with the trace context extracted from the document's "_oteltrace"
// field. When the field is absent the returned context is unchanged.
//
// Use this instead of Decode when you need to propagate the document's origin
// trace context downstream.
func (c *Cursor) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	if err := c.Cursor.Decode(val); err != nil {
		return ctx, err
	}
	raw := c.Current
	var originSpanCtx trace.SpanContext
	if c.propagationEnabled {
		if meta, ok := extractMetadataFromRaw(raw); ok {
			originCtx := contextFromTraceMetadata(context.Background(), meta, c.propagator)
			originSpanCtx = trace.SpanContextFromContext(originCtx)
		}
	}

	// Detach any existing parent so tracer.Start creates a new TraceID.
	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	spanOpts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal)}
	if originSpanCtx.IsValid() {
		spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}

	newCtx, span := c.tracer.Start(detachedCtx, "mongo.cursor.decode", spanOpts...)
	span.End()
	return newCtx, nil
}

// Decode decodes the current document into val.
// It delegates directly to the underlying *mongo.Cursor.Decode.
func (c *Cursor) Decode(val any) error {
	return c.Cursor.Decode(val)
}

// SingleResult wraps *mongo.SingleResult with trace propagation.
type SingleResult struct {
	*mongo.SingleResult
	span               trace.Span
	ctx                context.Context
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	endOnce            sync.Once
}

// endSpan ensures the associated span is ended exactly once.
func (r *SingleResult) endSpan() {
	r.endOnce.Do(func() { r.span.End() })
}

// Decode decodes the document and records any stored trace context as a span
// link on the FindOne span before ending it.
// The span is ended exactly once regardless of how many times Decode is called.
func (r *SingleResult) Decode(v any) error {
	defer r.endSpan()

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

// TraceContext returns a context enriched with the trace context stored in the
// fetched document's "_oteltrace" field. It must be called after Decode or Raw.
// The span is ended exactly once when this method is called.
func (r *SingleResult) TraceContext() context.Context {
	defer r.endSpan()

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
