package otelnats

import (
	"context"
	"time"

	nats "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// tracedConn is the fully-instrumented connImpl: every Publish/PublishMsg/Request
// opens a producer span, every wrapMsgHandler closure extracts the incoming trace
// header and opens a consumer span. deliverTracer is non-nil only when
// OTEL_EXPORTER_OTLP_ENDPOINT is set; per the construction invariant, tracing
// is on whenever this impl exists.
type tracedConn struct {
	nc            *nats.Conn
	tracer        trace.Tracer
	propagator    propagation.TextMapPropagator
	serverAttrs   []attribute.KeyValue
	traceDest     string
	deliverTracer trace.Tracer // may be nil when no exporter endpoint
}

func (t *tracedConn) TracingEnabled() bool     { return true }
func (t *tracedConn) DeliverSpanEnabled() bool { return t.deliverTracer != nil }
func (t *tracedConn) TraceContext() (trace.Tracer, propagation.TextMapPropagator) {
	return t.tracer, t.propagator
}
func (t *tracedConn) ServerAttrs() []attribute.KeyValue { return t.serverAttrs }
func (t *tracedConn) TraceDest() string                 { return t.traceDest }

func (t *tracedConn) Publish(ctx context.Context, subject string, data []byte) error {
	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  make(nats.Header),
	}
	return t.PublishMsg(ctx, msg)
}

func (t *tracedConn) PublishMsg(ctx context.Context, msg *nats.Msg) error {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	if t.traceDest != "" {
		msg.Header.Set("Nats-Trace-Dest", t.traceDest)
	}
	spanName := "send " + msg.Subject
	ctx, span := t.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(publishAttrs(msg, t.serverAttrs)...),
	)
	defer span.End()
	injectCtx := ctx
	if t.deliverTracer != nil {
		injectCtx = t.StartDeliverSpan(ctx, msg.Subject)
	}
	t.propagator.Inject(injectCtx, &HeaderCarrier{H: msg.Header})
	if err := t.nc.PublishMsg(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

func (t *tracedConn) Request(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  make(nats.Header),
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	spanName := "send " + subject
	reqCtx, span := t.tracer.Start(reqCtx, spanName,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(publishAttrs(msg, t.serverAttrs)...),
	)
	defer span.End()
	injectCtx := reqCtx
	if t.deliverTracer != nil {
		injectCtx = t.StartDeliverSpan(reqCtx, msg.Subject)
	}
	t.propagator.Inject(injectCtx, &HeaderCarrier{H: msg.Header})
	reply, err := t.nc.RequestMsgWithContext(reqCtx, msg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int(string(semconv.MessagingMessageBodySizeKey), len(reply.Data)))
	return reply, nil
}

func (t *tracedConn) traceEventHandler() nats.MsgHandler {
	return buildTraceEventHandler(t.tracer, t.propagator)
}

func (t *tracedConn) wrapMsgHandler(subject, queue string, handler MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		msgCtx := t.propagator.Extract(context.Background(), &HeaderCarrier{H: msg.Header})
		originSpanCtx := trace.SpanContextFromContext(msgCtx)
		consumerParentCtx := t.ConsumerContextWithDeliver(context.Background(), subject, originSpanCtx)
		spanName := "process " + subject
		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(receiveAttrs(msg, queue, "process", t.serverAttrs)...),
		}
		if originSpanCtx.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
		}
		ctx, span := t.tracer.Start(consumerParentCtx, spanName, opts...)
		defer span.End()
		handler(Msg{Msg: msg, Ctx: ctx})
	}
}

// StartDeliverSpan creates a synthetic CONSUMER span representing NATS broker delivery.
// When deliverTracer is nil (no OTEL_EXPORTER_OTLP_ENDPOINT) returns ctx unchanged —
// this is the only remaining runtime check; the !tracingEnabled disjunct is gone
// because directConn provides its own passthrough implementation.
func (t *tracedConn) StartDeliverSpan(ctx context.Context, subject string) context.Context {
	if t.deliverTracer == nil {
		return ctx
	}
	deliverCtx, span := t.deliverTracer.Start(ctx, subject+" deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(t.deliverAttrs(subject)...),
	)
	span.End()
	return deliverCtx
}

// ConsumerContextWithDeliver creates a consumer-side deliver span (SpanKindProducer)
// linked to origin and returns a context carrying that deliver span as remote
// parent for consumer spans. The deliverTracer-nil and origin-invalid checks
// remain per-call concerns.
func (t *tracedConn) ConsumerContextWithDeliver(ctx context.Context, subject string, origin trace.SpanContext) context.Context {
	if t.deliverTracer == nil || !origin.IsValid() {
		return ctx
	}
	detachedCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	_, deliverSpan := t.deliverTracer.Start(detachedCtx,
		subject+" deliver",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(t.deliverAttrs(subject)...),
		trace.WithLinks(trace.Link{SpanContext: origin}),
	)
	deliverSpan.End()
	return trace.ContextWithRemoteSpanContext(detachedCtx, deliverSpan.SpanContext())
}

func (t *tracedConn) deliverAttrs(subject string) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.MessagingSystemKey.String(messagingSystem),
		semconv.MessagingDestinationNameKey.String(subject),
	}
}
