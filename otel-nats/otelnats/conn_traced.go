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
// header and opens a consumer span.
type tracedConn struct {
	nc          *nats.Conn
	tracer      trace.Tracer
	propagator  propagation.TextMapPropagator
	serverAttrs []attribute.KeyValue
	traceDest   string
}

func (t *tracedConn) TracingEnabled() bool { return true }
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
// trace context, and returns the span-carrying context for the underlying
// driver call. Fire-and-forget semantics: caller does not block on a peer reply.
func (t *tracedConn) startSendSpan(parent context.Context, msg *nats.Msg) (context.Context, trace.Span) {
	ctx, span := t.tracer.Start(parent, "send "+msg.Subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(publishAttrs(msg, t.serverAttrs)...),
	)
	t.propagator.Inject(ctx, &HeaderCarrier{H: msg.Header})
	return ctx, span
}

// startRequestSpan opens the CLIENT span used by Request/RequestMsg/
// RequestWithContext/RequestMsgWithContext. Request/reply is an RPC pattern
// (caller blocks on a peer Respond), so PRODUCER kind would mis-classify it.
// Span name follows OTel naming guidance for RPC client operations:
// "{destination} request".
func (t *tracedConn) startRequestSpan(parent context.Context, msg *nats.Msg) (context.Context, trace.Span) {
	ctx, span := t.tracer.Start(parent, msg.Subject+" request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(requestAttrs(msg, t.serverAttrs)...),
	)
	t.propagator.Inject(ctx, &HeaderCarrier{H: msg.Header})
	return ctx, span
}

// recordReply emits a CLIENT span representing reply reception (a pull
// "receive" per the OTel messaging span-kind mapping). Extracts W3C trace
// context from reply.Header so any responder-side trace is linked into the
// receive span. The request span's body-size attribute is left untouched:
// reply body size belongs to the receive span (via receiveAttrs), not the
// send span.
//
// reply.Subject is the reply inbox — the natural NATS request/reply
// conversation ID. It becomes observable to the wrapper only once the reply
// arrives, so both the receive span (set at start, via an explicit attribute
// since a reply's own Reply field is empty and receiveAttrs' msg.Reply clause
// cannot see it) and the request span (a late SetAttributes call, valid any
// time before End()) receive it here. A request that times out or errors
// never calls recordReply, so its send span carries no conversation_id —
// conformant, since the semconv requirement level is Recommended, and expected
// since samplers only observe span-start attributes.
func (t *tracedConn) recordReply(parent context.Context, reqSpan trace.Span, reply *nats.Msg) {
	reqSpan.SetAttributes(semconv.MessagingMessageConversationID(reply.Subject))
	var originSC trace.SpanContext
	receiveCtx := parent
	if reply.Header != nil {
		extracted := t.propagator.Extract(parent, &HeaderCarrier{H: reply.Header})
		originSC = trace.SpanContextFromContext(extracted)
		if originSC.IsValid() {
			receiveCtx = extracted
		}
	}
	attrs := append(receiveAttrs(reply, "", "receive", t.serverAttrs), semconv.MessagingMessageConversationID(reply.Subject))
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
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
		spanName := "process " + subject
		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(receiveAttrs(msg, queue, "process", t.serverAttrs)...),
		}
		if originSpanCtx.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
		}
		ctx, span := t.tracer.Start(context.Background(), spanName, opts...)
		defer span.End()
		handler(Msg{Msg: msg, Ctx: ctx})
	}
}
