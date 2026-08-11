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

	"github.com/akira-core/instrumentation-go/otel-nats/internal/spanname"
)

// tracedConn is the fully-instrumented connImpl: every Publish/PublishMsg/Request
// opens a producer span, every wrapMsgHandler closure extracts the incoming trace
// header and opens a consumer span.
type tracedConn struct {
	nc            *nats.Conn
	tracer        trace.Tracer
	propagator    propagation.TextMapPropagator
	serverAttrs   []attribute.KeyValue
	traceDest     string
	inboxPrefixes []string
}

// inboxAttrs marks a span whose destination is a request/reply inbox. Shared with
// oteljetstream, which needs the same markers for a stream that captures inboxes.
func inboxAttrs(subject string) []attribute.KeyValue { return spanname.InboxAttrs(subject) }

func (t *tracedConn) TracingEnabled() bool { return true }
func (t *tracedConn) TraceContext() (trace.Tracer, propagation.TextMapPropagator) {
	return t.tracer, t.propagator
}
func (t *tracedConn) ServerAttrs() []attribute.KeyValue { return t.serverAttrs }
func (t *tracedConn) TraceDest() string                 { return t.traceDest }
func (t *tracedConn) InboxPrefixes() []string           { return t.inboxPrefixes }

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
	reqCtx, reqSpan, destIsInbox := t.startRequestSpan(parent, msg)
	defer reqSpan.End()
	reply, err := t.nc.RequestMsg(msg, timeout)
	if err != nil {
		reqSpan.RecordError(err)
		reqSpan.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	t.recordReply(reqCtx, reqSpan, reply, destIsInbox)
	return reply, nil
}

// requestWithCtx is the ctx-driven request path used by RequestWithContext and RequestMsgWithContext.
// Delegates to nats.Conn.RequestMsgWithContext so ctx semantics match the origin driver.
func (t *tracedConn) requestWithCtx(parent context.Context, msg *nats.Msg) (*nats.Msg, error) {
	reqCtx, reqSpan, destIsInbox := t.startRequestSpan(parent, msg)
	defer reqSpan.End()
	reply, err := t.nc.RequestMsgWithContext(reqCtx, msg)
	if err != nil {
		reqSpan.RecordError(err)
		reqSpan.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	t.recordReply(reqCtx, reqSpan, reply, destIsInbox)
	return reply, nil
}

// startSendSpan opens the PRODUCER span used by Publish/PublishMsg, injects
// trace context, and returns the span-carrying context for the underlying
// driver call. Fire-and-forget semantics: caller does not block on a peer reply.
//
// Publishing to an inbox is the responder half of a manual request/reply exchange
// (nc.Publish(msg.Reply, data) rather than msg.Respond), so the inbox test applies
// here too: without it every reply sent that way names its span after a per-request
// nuid.
func (t *tracedConn) startSendSpan(parent context.Context, msg *nats.Msg) (context.Context, trace.Span) {
	// No filter subject on the publish path, so Resolve can never surface a
	// destination template here — only the name and the inbox verdict matter.
	name, _, inbox := spanname.Resolve("publish", msg.Subject, "", t.inboxPrefixes)
	attrs := publishAttrs(msg, t.serverAttrs)
	if inbox {
		attrs = append(attrs, inboxAttrs(msg.Subject)...)
	}
	ctx, span := t.tracer.Start(parent, name,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attrs...),
	)
	t.propagator.Inject(ctx, &HeaderCarrier{H: msg.Header})
	return ctx, span
}

// startRequestSpan opens the CLIENT span used by Request/RequestMsg/
// RequestWithContext/RequestMsgWithContext. Request/reply is an RPC pattern
// (caller blocks on a peer Respond), so PRODUCER kind would mis-classify it.
// Span name is operation-first, "request {destination}", matching semconv
// v1.39.0's "{messaging.operation.name} {destination}" shape and the
// messaging.operation.name=request attribute requestAttrs already sets —
// rather than relabeling the attribute to fit the older RPC-style
// "{destination} request" (design.md D1).
//
// Returns whether the destination is an inbox, which decides who owns the span's
// conversation_id — see recordReply.
func (t *tracedConn) startRequestSpan(parent context.Context, msg *nats.Msg) (context.Context, trace.Span, bool) {
	// No filter subject on the request path either — see startSendSpan.
	name, _, inbox := spanname.Resolve("request", msg.Subject, "", t.inboxPrefixes)
	attrs := requestAttrs(msg, t.serverAttrs)
	if inbox {
		attrs = append(attrs, inboxAttrs(msg.Subject)...)
	}
	ctx, span := t.tracer.Start(parent, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	t.propagator.Inject(ctx, &HeaderCarrier{H: msg.Header})
	return ctx, span, inbox
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
//
// destIsInbox suppresses that late write. A request addressed AT an inbox is the
// callback half of a manual exchange, and its request span was already given the
// conversation the message belongs to — the peer's inbox — at span start. Writing
// this request's own reply inbox over it would make one attribute hold two values
// during the span's life and export the one nothing had observed at start. The
// nested conversation stays recorded on the receive span, which is the span the
// reply message belongs to.
//
// The reply inbox is structurally always anonymous and temporary (an
// auto-generated per-request subject that stops existing once the exchange
// completes), so inboxAttrs is applied unconditionally here rather than via a
// prefix test — correct even under nats.CustomInboxPrefix, and correct even
// when the peer's prefix is one this connection would not recognise. Per
// design.md D2 the span name is the bare literal "receive": no
// spanname.Resolve call and no {destination} segment at all (semconv v1.39.0
// omits the segment rather than using the old "(anonymous)" literal). The
// inbox subject itself stays observable via messaging.destination.name and
// messaging.message.conversation_id.
func (t *tracedConn) recordReply(parent context.Context, reqSpan trace.Span, reply *nats.Msg, destIsInbox bool) {
	if !destIsInbox {
		reqSpan.SetAttributes(semconv.MessagingMessageConversationID(reply.Subject))
	}
	var originSC trace.SpanContext
	receiveCtx := parent
	if reply.Header != nil {
		extracted := t.propagator.Extract(parent, &HeaderCarrier{H: reply.Header})
		originSC = trace.SpanContextFromContext(extracted)
		if originSC.IsValid() {
			receiveCtx = extracted
		}
	}
	attrs := append(receiveAttrs(reply, "", "receive", t.serverAttrs), inboxAttrs(reply.Subject)...)
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	}
	if originSC.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSC}))
	}
	_, span := t.tracer.Start(receiveCtx, "receive", opts...)
	span.End()
}

func (t *tracedConn) traceEventHandler() nats.MsgHandler {
	return buildTraceEventHandler(t.tracer, t.propagator)
}

// wrapMsgHandler opens the CONSUMER span for each delivery. The subscription
// subject is the destination — a fact the subscriber declared, and already the
// low-cardinality form for a wildcard subscription. The inbox test still applies:
// a subscription to an inbox (or to "<inbox>.>", where the FILTER carries the nuid
// and the concrete subject alone would not reveal it) is the manual half of
// request/reply, and spanname.Resolve tests the resolved destination for exactly
// that reason.
func (t *tracedConn) wrapMsgHandler(subject, queue string, handler MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		msgCtx := t.propagator.Extract(context.Background(), &HeaderCarrier{H: msg.Header})
		originSpanCtx := trace.SpanContextFromContext(msgCtx)
		spanName, template, inbox := spanname.Resolve("process", msg.Subject, subject, t.inboxPrefixes)
		attrs := receiveAttrs(msg, queue, "process", t.serverAttrs)
		if template != "" {
			attrs = append(attrs, semconv.MessagingDestinationTemplate(template))
		}
		if inbox {
			attrs = append(attrs, inboxAttrs(msg.Subject)...)
		}
		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attrs...),
		}
		if originSpanCtx.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
		}
		ctx, span := t.tracer.Start(context.Background(), spanName, opts...)
		defer span.End()
		handler(Msg{Msg: msg, Ctx: ctx})
	}
}
