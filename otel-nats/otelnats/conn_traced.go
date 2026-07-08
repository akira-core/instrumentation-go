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
	_, span := t.startSendSpan(ctx, msg)
	defer span.End()
	if err := t.nc.PublishMsg(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// Request mirrors nats.Conn.Request. Producer span parent is context.Background()
// because the origin signature has no ctx; callers needing trace chaining should
// use RequestWithContext or RequestMsgWithContext instead.
func (t *tracedConn) Request(subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	msg := &nats.Msg{Subject: subject, Data: data, Header: make(nats.Header)}
	return t.requestWithTimeout(context.Background(), msg, timeout)
}

// RequestWithContext mirrors nats.Conn.RequestWithContext. The producer span is
// rooted at the supplied ctx; ctx also controls the underlying RPC timeout.
func (t *tracedConn) RequestWithContext(ctx context.Context, subject string, data []byte) (*nats.Msg, error) {
	msg := &nats.Msg{Subject: subject, Data: data, Header: make(nats.Header)}
	return t.requestWithCtx(ctx, msg)
}

// RequestMsg mirrors nats.Conn.RequestMsg. Producer span parent is context.Background().
func (t *tracedConn) RequestMsg(msg *nats.Msg, timeout time.Duration) (*nats.Msg, error) {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	return t.requestWithTimeout(context.Background(), msg, timeout)
}

// RequestMsgWithContext mirrors nats.Conn.RequestMsgWithContext. Producer span rooted at ctx.
func (t *tracedConn) RequestMsgWithContext(ctx context.Context, msg *nats.Msg) (*nats.Msg, error) {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	return t.requestWithCtx(ctx, msg)
}

// requestWithTimeout is the timeout-driven request path used by Request and RequestMsg.
// Delegates to nats.Conn.RequestMsg so timeout semantics match the origin driver.
func (t *tracedConn) requestWithTimeout(parent context.Context, msg *nats.Msg, timeout time.Duration) (*nats.Msg, error) {
	reqCtx, reqSpan := t.startRequestSpan(parent, msg)
	defer reqSpan.End()
	reply, err := t.nc.RequestMsg(msg, timeout)
	if err != nil {
		reqSpan.RecordError(err)
		reqSpan.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	t.recordReply(reqCtx, reqSpan, reply)
	return reply, nil
}

// requestWithCtx is the ctx-driven request path used by RequestWithContext and RequestMsgWithContext.
// Delegates to nats.Conn.RequestMsgWithContext so ctx semantics match the origin driver.
func (t *tracedConn) requestWithCtx(parent context.Context, msg *nats.Msg) (*nats.Msg, error) {
	reqCtx, reqSpan := t.startRequestSpan(parent, msg)
	defer reqSpan.End()
	reply, err := t.nc.RequestMsgWithContext(reqCtx, msg)
	if err != nil {
		reqSpan.RecordError(err)
		reqSpan.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	t.recordReply(reqCtx, reqSpan, reply)
	return reply, nil
}

// startSendSpan opens the PRODUCER span used by Publish/PublishMsg, injects
// trace context (through a deliver span when configured), and returns the
// span-carrying context for the underlying driver call. Fire-and-forget
// semantics: caller does not block on a peer reply.
func (t *tracedConn) startSendSpan(parent context.Context, msg *nats.Msg) (context.Context, trace.Span) {
	ctx, span := t.tracer.Start(parent, "send "+msg.Subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(publishAttrs(msg, t.serverAttrs)...),
	)
	injectCtx := ctx
	if t.deliverTracer != nil {
		injectCtx = t.StartDeliverSpan(ctx, msg.Subject)
	}
	t.propagator.Inject(injectCtx, &HeaderCarrier{H: msg.Header})
	return ctx, span
}

// startRequestSpan opens the CLIENT span used by Request/RequestMsg/
// RequestWithContext/RequestMsgWithContext. Request/reply is an RPC pattern
// (caller blocks on a peer Respond), so PRODUCER kind would mis-classify it.
// Span name follows OTel naming guidance for RPC client operations:
// "{destination} request". Propagation inject + deliver-span wrapping match
// startSendSpan so the responder side sees the same wire format.
func (t *tracedConn) startRequestSpan(parent context.Context, msg *nats.Msg) (context.Context, trace.Span) {
	ctx, span := t.tracer.Start(parent, msg.Subject+" request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(requestAttrs(msg, t.serverAttrs)...),
	)
	injectCtx := ctx
	if t.deliverTracer != nil {
		injectCtx = t.StartDeliverSpan(ctx, msg.Subject)
	}
	t.propagator.Inject(injectCtx, &HeaderCarrier{H: msg.Header})
	return ctx, span
}

// recordReply finalises the producer span with reply size and emits a CONSUMER
// span representing reply reception — aligned with the Subscribe consumer
// surface so every inbound message path produces a span + propagation extract.
// Extracts W3C trace context from reply.Header so any responder-side trace is
// linked into the receive span.
func (t *tracedConn) recordReply(parent context.Context, sendSpan trace.Span, reply *nats.Msg) {
	sendSpan.SetAttributes(attribute.Int(string(semconv.MessagingMessageBodySizeKey), len(reply.Data)))
	var originSC trace.SpanContext
	receiveCtx := parent
	if reply.Header != nil {
		extracted := t.propagator.Extract(parent, &HeaderCarrier{H: reply.Header})
		originSC = trace.SpanContextFromContext(extracted)
		if originSC.IsValid() {
			receiveCtx = extracted
		}
	}
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(receiveAttrs(reply, "", "receive", t.serverAttrs)...),
	}
	if originSC.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSC}))
	}
	_, span := t.tracer.Start(receiveCtx, "receive "+reply.Subject, opts...)
	span.End()
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
