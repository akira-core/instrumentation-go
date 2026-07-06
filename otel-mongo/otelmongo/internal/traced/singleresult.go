package traced

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
)

// SingleResult is the enabled-path impl of the otelmongo.SingleResult
// strategy. The FindOne span is held here and ended exactly once on the first
// of Decode / TraceContext / Raw.
type SingleResult struct {
	sr                 *mongo.SingleResult
	span               trace.Span
	ctx                context.Context
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	endOnce            sync.Once
}

// NewSingleResult wraps sr with the enabled-path SingleResult impl.
func NewSingleResult(sr *mongo.SingleResult, span trace.Span, ctx context.Context, propagator propagation.TextMapPropagator, propagationEnabled bool) *SingleResult {
	return &SingleResult{
		sr:                 sr,
		span:               span,
		ctx:                ctx,
		propagator:         propagator,
		propagationEnabled: propagationEnabled,
	}
}

func (r *SingleResult) endSpan() {
	r.endOnce.Do(func() { r.span.End() })
}

// Decode decodes the document and records any stored trace context as a span
// link before ending the FindOne span. The span is ended exactly once.
func (r *SingleResult) Decode(v any) error {
	defer r.endSpan()
	raw, err := r.sr.Raw()
	if err != nil {
		r.span.RecordError(err)
		r.span.SetStatus(codes.Error, err.Error())
		return err
	}
	if r.propagationEnabled {
		if meta, ok := shared.ExtractMetadataFromRaw(raw); ok {
			originCtx := shared.ContextFromTraceMetadata(context.Background(), meta, r.propagator)
			originSpanCtx := trace.SpanContextFromContext(originCtx)
			if originSpanCtx.IsValid() {
				r.span.AddLink(trace.Link{SpanContext: originSpanCtx})
			}
		}
	}
	return r.sr.Decode(v)
}

// TraceContext returns a context enriched with the trace context stored in
// the fetched document's "_oteltrace" field. Ends the FindOne span exactly once.
func (r *SingleResult) TraceContext() context.Context {
	defer r.endSpan()
	raw, err := r.sr.Raw()
	if err != nil {
		return r.ctx
	}
	if r.propagationEnabled {
		if meta, ok := shared.ExtractMetadataFromRaw(raw); ok {
			return shared.ContextFromTraceMetadata(r.ctx, meta, r.propagator)
		}
	}
	return r.ctx
}

// Raw returns the raw BSON document and ends the FindOne span exactly once.
func (r *SingleResult) Raw() (bson.Raw, error) {
	defer r.endSpan()
	return r.sr.Raw()
}
