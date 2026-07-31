package oteljetstream

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// tracedConsumer instruments every fetch/iterate with a consumer span linked
// to the producer trace context embedded in the message headers.
type tracedConsumer struct {
	conn         *otelnats.Conn
	streamName   string
	consumerName string
	c            jetstream.Consumer
}

// on reports whether THIS call should be instrumented. A consumer often outlives
// many flag changes, so it is read per call rather than captured at construction.
func (c *tracedConsumer) on() bool { return c.conn.TracingEnabled() }

// direct returns the passthrough behaviour for this consumer, used when the flag
// resolves off. It wraps the same native consumer, so no extra server round trip.
func (c *tracedConsumer) direct() *directConsumer { return &directConsumer{c: c.c} }

func (c *tracedConsumer) Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	wrapped := dynamicConsumeHandler(c.conn, c.consumerName, handler)
	return wrapConsumeContext(c.c.Consume(wrapped, opts...))
}

func (c *tracedConsumer) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	iter, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	// Always the dynamic iterator: the flag is resolved per Next, never at
	// Messages time — this iterator is a canonically long-lived object and must
	// follow relay changes in both directions.
	return &tracedMessagesContext{conn: c.conn, iter: iter, consumerName: c.consumerName}, nil
}

func (c *tracedConsumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	if !c.on() {
		return c.direct().Next(ctx, opts...)
	}
	opts, err := applyCtxToFetchOpts(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	tracer, prop := c.conn.TraceContext()
	msgCtx := context.Background()
	if h := msg.Headers(); h != nil {
		msgCtx = prop.Extract(msgCtx, &otelnats.HeaderCarrier{H: h})
	}
	originSpanCtx := trace.SpanContextFromContext(msgCtx)
	spanName := "receive " + msg.Subject()
	attrs := receiveMsgAttrs(receiveBaseAttrs("receive", c.conn.ServerAttrs(), c.consumerName), msg)
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	}
	if originSpanCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}
	// Return the ctx bearing the local receive span (linked to the producer),
	// not the raw extracted producer ctx, so downstream spans nest under this
	// consumer's receive span — matching Messages().Next and the Consume handler.
	// The span is ended immediately: a single-shot fetch has no processing-scope
	// boundary to close it later. Child spans still parent correctly to an ended
	// span via its still-valid SpanContext.
	ctx, span := tracer.Start(context.Background(), spanName, startOpts...)
	span.End()
	return ctx, msg, nil
}

func (c *tracedConsumer) Fetch(batch int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	if !c.on() {
		return c.direct().Fetch(batch, opts...)
	}
	raw, err := c.c.Fetch(batch, opts...)
	if err != nil {
		return nil, err
	}
	return newTracedMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *tracedConsumer) FetchBytes(maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	if !c.on() {
		return c.direct().FetchBytes(maxBytes, opts...)
	}
	raw, err := c.c.FetchBytes(maxBytes, opts...)
	if err != nil {
		return nil, err
	}
	return newTracedMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *tracedConsumer) FetchNoWait(batch int) (MessageBatch, error) {
	if !c.on() {
		return c.direct().FetchNoWait(batch)
	}
	raw, err := c.c.FetchNoWait(batch)
	if err != nil {
		return nil, err
	}
	return newTracedMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *tracedConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *tracedConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

// tracedPushConsumer instruments the push-based consumer: Consume wraps the
// user handler with the same extract-and-span closure as the pull variant.
type tracedPushConsumer struct {
	conn         *otelnats.Conn
	consumerName string
	c            jetstream.PushConsumer
}

// newTracedPushConsumer wraps a raw jetstream.PushConsumer (and its constructor
// error) as the instrumented PushConsumer impl.
func newTracedPushConsumer(conn *otelnats.Conn, name string, cons jetstream.PushConsumer, err error) (PushConsumer, error) {
	if err != nil {
		return nil, err
	}
	return &tracedPushConsumer{conn: conn, consumerName: name, c: cons}, nil
}

func (c *tracedPushConsumer) Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error) {
	wrapped := dynamicConsumeHandler(c.conn, c.consumerName, handler)
	return wrapConsumeContext(c.c.Consume(wrapped, opts...))
}

func (c *tracedPushConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *tracedPushConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

// dynamicConsumeHandler returns the per-message closure bound into a Consume
// callback. A ConsumeContext is created once and typically runs for the process
// lifetime, so the tracing flag is re-resolved on EVERY message rather than at
// Consume time — a relay change reaches a running consume loop.
//
// Returns nil for a nil handler so the underlying Consume call surfaces
// jetstream's ErrHandlerRequired instead of panicking in the delivery goroutine.
func dynamicConsumeHandler(conn *otelnats.Conn, consumerName string, handler MsgHandler) func(jetstream.Msg) {
	if handler == nil {
		return nil
	}
	th := tracedConsumeHandler(conn, consumerName, handler)
	dh := directHandler(handler)
	return func(msg jetstream.Msg) {
		if conn.TracingEnabled() {
			th(msg)
		} else {
			dh(msg)
		}
	}
}

// tracedConsumeHandler returns the instrumented closure that extracts the message's
// trace context and starts a consumer span before invoking the user handler.
//
// conn.TraceContext() and conn.ServerAttrs() are resolved INSIDE the per-message
// closure, not captured here: on a dynamic Conn they answer through impl(), so
// capturing them while the flag happened to be off would freeze the noop tracer
// into a handler that dynamicConsumeHandler will later invoke with the flag on.
func tracedConsumeHandler(conn *otelnats.Conn, consumerName string, handler MsgHandler) func(jetstream.Msg) {
	if handler == nil {
		return nil
	}
	return func(msg jetstream.Msg) {
		tracer, prop := conn.TraceContext()
		baseAttrs := receiveBaseAttrs("process", conn.ServerAttrs(), consumerName)
		msgCtx := context.Background()
		if h := msg.Headers(); h != nil {
			msgCtx = prop.Extract(msgCtx, &otelnats.HeaderCarrier{H: h})
		}
		originSpanCtx := trace.SpanContextFromContext(msgCtx)
		spanName := "process " + msg.Subject()
		attrs := receiveMsgAttrs(baseAttrs, msg)
		startOpts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attrs...),
		}
		if originSpanCtx.IsValid() {
			startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
		}
		ctx, span := tracer.Start(context.Background(), spanName, startOpts...)
		defer span.End()
		handler(Msg{Msg: msg, Ctx: ctx})
	}
}

// tracedMessagesContext is the instrumented MessagesContext iterator. Each
// call to Next ends its own receive span before returning (handover), so
// there is no cross-call span state and nothing for Stop/Drain to race
// against or clean up.
//
// The tracing flag is resolved per Next — never at Messages() time — because
// this iterator is a canonically long-lived object; tracer/propagator are
// likewise resolved per call so a dynamic Conn's current impl answers.
type tracedMessagesContext struct {
	conn         *otelnats.Conn
	iter         jetstream.MessagesContext
	consumerName string
}

func (m *tracedMessagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	msg, err := m.iter.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	if !m.conn.TracingEnabled() {
		return context.Background(), msg, nil
	}
	tracer, prop := m.conn.TraceContext()
	msgCtx := context.Background()
	if h := msg.Headers(); h != nil {
		msgCtx = prop.Extract(msgCtx, &otelnats.HeaderCarrier{H: h})
	}
	originSpanCtx := trace.SpanContextFromContext(msgCtx)
	spanName := "receive " + msg.Subject()
	attrs := receiveMsgAttrs(receiveBaseAttrs("receive", m.conn.ServerAttrs(), m.consumerName), msg)
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	}
	if originSpanCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}
	// Return the ctx bearing the local receive span (linked to the producer),
	// ended immediately at handover — matching single-shot Consumer.Next.
	// Child spans still parent correctly to an ended span via its still-valid
	// SpanContext.
	ctx, span := tracer.Start(context.Background(), spanName, startOpts...)
	span.End()
	return ctx, msg, nil
}

func (m *tracedMessagesContext) Stop() {
	m.iter.Stop()
}

func (m *tracedMessagesContext) Drain() {
	m.iter.Drain()
}
