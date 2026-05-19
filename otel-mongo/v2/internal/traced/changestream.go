package traced

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/shared"
)

// ChangeStream is the enabled-path impl of the otelmongo.ChangeStream strategy.
//
// Performance: deliverStartOpts is pre-built once at construction (kind +
// attributes); per-event work in DecodeWithContext only appends an
// optional link, avoiding repeated variadic slice allocation on the hot
// read path.
type ChangeStream struct {
	cs                 *mongo.ChangeStream
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	spanName           string
	baseSpanOpts       []trace.SpanStartOption
	deliverTracer      trace.Tracer
	deliverSpanName    string
	deliverStartOpts   []trace.SpanStartOption
}

// ChangeStreamConfig groups construction parameters for NewChangeStream.
type ChangeStreamConfig struct {
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
	SpanName           string
	BaseSpanOpts       []trace.SpanStartOption
	DeliverTracer      trace.Tracer
	DeliverSpanName    string
	DeliverAttrs       []attribute.KeyValue
}

// NewChangeStream wraps cs with the enabled-path ChangeStream impl.
// Pre-builds the deliver-span start options once so DecodeWithContext does
// not re-construct the kind + attribute slice on every event.
func NewChangeStream(cs *mongo.ChangeStream, cfg ChangeStreamConfig) *ChangeStream {
	c := &ChangeStream{
		cs:                 cs,
		tracer:             cfg.Tracer,
		propagator:         cfg.Propagator,
		propagationEnabled: cfg.PropagationEnabled,
		spanName:           cfg.SpanName,
		baseSpanOpts:       cfg.BaseSpanOpts,
		deliverTracer:      cfg.DeliverTracer,
		deliverSpanName:    cfg.DeliverSpanName,
	}
	if cfg.DeliverTracer != nil {
		c.deliverStartOpts = []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(cfg.DeliverAttrs...),
		}
	}
	return c
}

// DecodeWithContext decodes the current change document and returns a
// context enriched with trace context from fullDocument's "_oteltrace".
func (c *ChangeStream) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	var originSpanCtx trace.SpanContext
	fullDoc, err := c.cs.Current.LookupErr("fullDocument")
	if err == nil && c.propagationEnabled {
		docRaw, ok := fullDoc.DocumentOK()
		if ok {
			if meta, ok := shared.ExtractMetadataFromRaw(docRaw); ok {
				originSpanCtx = shared.SpanContextFromMetadata(meta, c.propagator)
			}
		}
	}

	newCtx, span := c.buildConsumerCtx(ctx, originSpanCtx)

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

// buildConsumerCtx creates a detached context with a consumer span linked to
// originSpanCtx. Uses the pre-built deliverStartOpts when a deliver span is
// required, only appending the per-event link.
func (c *ChangeStream) buildConsumerCtx(ctx context.Context, originSpanCtx trace.SpanContext) (context.Context, trace.Span) {
	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	consumerCtx := detachedCtx

	if c.deliverTracer != nil && originSpanCtx.IsValid() {
		deliverOpts := append(c.deliverStartOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx})) //nolint:gocritic // intentional new slice to avoid mutating prebuilt opts
		_, deliverSpan := c.deliverTracer.Start(detachedCtx, c.deliverSpanName, deliverOpts...)
		deliverSpan.End()
		consumerCtx = trace.ContextWithRemoteSpanContext(detachedCtx, deliverSpan.SpanContext())
	}

	if !originSpanCtx.IsValid() {
		return c.tracer.Start(consumerCtx, c.spanName, c.baseSpanOpts...)
	}
	spanOpts := append(c.baseSpanOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx})) //nolint:gocritic // intentional new slice to avoid mutating prebuilt opts
	return c.tracer.Start(consumerCtx, c.spanName, spanOpts...)
}
