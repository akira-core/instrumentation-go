package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"
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

// Decode decodes the current document into val. Delegates to *mongo.Cursor.Decode.
func (c *Cursor) Decode(val any) error {
	return c.Cursor.Decode(val)
}
