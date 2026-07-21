package traced

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
)

// Cursor is the enabled-path impl of the otelmongo.Cursor strategy. Created by
// tracedCollection.Find / .Aggregate.
type Cursor struct {
	cur                *mongo.Cursor
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
}

// NewCursor wraps cur with the enabled-path Cursor impl.
func NewCursor(cur *mongo.Cursor, tracer trace.Tracer, propagator propagation.TextMapPropagator, propagationEnabled bool) *Cursor {
	return &Cursor{
		cur:                cur,
		tracer:             tracer,
		propagator:         propagator,
		propagationEnabled: propagationEnabled,
	}
}

// DecodeAndTrace decodes the current document and returns a context
// enriched with the trace context extracted from the document's "_oteltrace"
// field. Always emits a "mongo.cursor.decode" span linked to the origin trace
// when present.
func (c *Cursor) DecodeAndTrace(ctx context.Context, val any) (context.Context, error) {
	if err := c.cur.Decode(val); err != nil {
		return ctx, err
	}
	raw := c.cur.Current
	var originSpanCtx trace.SpanContext
	if c.propagationEnabled {
		if meta, ok := shared.ExtractMetadataFromRaw(raw); ok {
			originCtx := shared.ContextFromTraceMetadata(context.Background(), meta, c.propagator)
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

// Decode delegates to *mongo.Cursor.Decode.
func (c *Cursor) Decode(val any) error { return c.cur.Decode(val) }
