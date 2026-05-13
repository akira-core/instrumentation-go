package otelmongo

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Cursor wraps *mongo.Cursor with trace propagation. The tracing/propagation
// flags are populated by the Collection that produced this cursor — when the
// gate is off all fields except Cursor stay zero-valued and DecodeWithContext
// is a passthrough.
type Cursor struct {
	*mongo.Cursor
	parentCtx          context.Context
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	tracingEnabled     bool
	propagationEnabled bool
}

// DecodeWithContext decodes the current document into val and returns a
// context enriched with the trace context extracted from the document's
// "_oteltrace" field. When tracing is off (or the field is absent) the
// returned context is unchanged.
func (c *Cursor) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	if err := c.Cursor.Decode(val); err != nil {
		return ctx, err
	}
	if !c.tracingEnabled {
		return ctx, nil
	}
	raw := c.Current
	var originSpanCtx trace.SpanContext
	if c.propagationEnabled {
		if meta, ok := extractMetadataFromRaw(raw); ok {
			originCtx := contextFromTraceMetadata(context.Background(), meta, c.propagator)
			originSpanCtx = trace.SpanContextFromContext(originCtx)
		}
	}

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
func (c *Cursor) Decode(val any) error {
	return c.Cursor.Decode(val)
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
// trace context as a span link on the FindOne span before ending it.
func (r *SingleResult) Decode(v any) error {
	defer r.endSpan()
	if !r.tracingEnabled {
		return r.SingleResult.Decode(v)
	}
	raw, err := r.SingleResult.Raw()
	if err != nil {
		if r.span != nil {
			r.span.RecordError(err)
			r.span.SetStatus(codes.Error, err.Error())
		}
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
