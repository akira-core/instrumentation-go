package traced

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
)

// ChangeStream is the enabled-path impl of the otelmongo.ChangeStream strategy.
type ChangeStream struct {
	cs                 *mongo.ChangeStream
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	spanName           string
	baseSpanOpts       []trace.SpanStartOption
}

// ChangeStreamConfig groups construction parameters for NewChangeStream.
type ChangeStreamConfig struct {
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
	SpanName           string
	BaseSpanOpts       []trace.SpanStartOption
}

// NewChangeStream wraps cs with the enabled-path ChangeStream impl.
func NewChangeStream(cs *mongo.ChangeStream, cfg ChangeStreamConfig) *ChangeStream {
	return &ChangeStream{
		cs:                 cs,
		tracer:             cfg.Tracer,
		propagator:         cfg.Propagator,
		propagationEnabled: cfg.PropagationEnabled,
		spanName:           cfg.SpanName,
		baseSpanOpts:       cfg.BaseSpanOpts,
	}
}

// DecodeAndTrace decodes the current change document and returns a
// context enriched with trace context from fullDocument's "_oteltrace".
func (c *ChangeStream) DecodeAndTrace(ctx context.Context, val any) (context.Context, error) {
	var originSpanCtx trace.SpanContext
	fullDoc, err := c.cs.Current.LookupErr("fullDocument")
	if err == nil && c.propagationEnabled {
		docRaw, ok := fullDoc.DocumentOK()
		if ok {
			if meta, ok := shared.ExtractMetadataFromRaw(docRaw); ok {
				originCtx := shared.ContextFromTraceMetadata(context.Background(), meta, c.propagator)
				originSpanCtx = trace.SpanContextFromContext(originCtx)
			}
		}
	}

	newCtx, span := buildLinkedSpanCtx(ctx, c.tracer, c.spanName, c.baseSpanOpts, originSpanCtx)

	if err := c.cs.Decode(val); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return ctx, err
	}

	span.End()
	return newCtx, nil
}

// Decode delegates to *mongo.ChangeStream.Decode.
func (c *ChangeStream) Decode(val any) error { return c.cs.Decode(val) }

// buildLinkedSpanCtx creates a detached context with a span linked to
// originSpanCtx (when valid).
func buildLinkedSpanCtx(ctx context.Context, tracer trace.Tracer, spanName string, baseSpanOpts []trace.SpanStartOption, originSpanCtx trace.SpanContext) (context.Context, trace.Span) {
	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})

	spanOpts := append([]trace.SpanStartOption{}, baseSpanOpts...)
	if originSpanCtx.IsValid() {
		spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}

	return tracer.Start(detachedCtx, spanName, spanOpts...)
}
