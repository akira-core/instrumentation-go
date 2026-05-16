package oteljetstream

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

// tracedConsumer instruments every fetch/iterate with a consumer span linked
// to the producer trace context embedded in the message headers.
type tracedConsumer struct {
	conn         *otelnats.Conn
	streamName   string
	consumerName string
	c            jetstream.Consumer
}

func (c *tracedConsumer) Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	wrapped := tracedConsumeHandler(c.conn, c.consumerName, handler)
	cc, err := c.c.Consume(wrapped, opts...)
	if err != nil {
		return nil, err
	}
	return &consumeContextImpl{cc: cc}, nil
}

func (c *tracedConsumer) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	iter, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &tracedMessagesContext{conn: c.conn, consumerName: c.consumerName, iter: iter}, nil
}

func (c *tracedConsumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	if ctx != nil {
		opts = append([]jetstream.FetchOpt{jetstream.FetchContext(ctx)}, opts...)
	}
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	tracer, prop := c.conn.TraceContext()
	spanName := "receive " + msg.Subject()
	attrs := append(receiveAttrs(msg, "receive", c.conn.ServerAttrs()), attribute.String(attrConsumerName, c.consumerName))
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	// Propagation closure: when off, no Extract / no deliver span / no link.
	msgCtx := context.Background()
	consumerParentCtx := msgCtx
	if c.conn.PropagationEnabled() {
		if hdr := msg.Headers(); hdr != nil && hdr.Get("traceparent") != "" {
			msgCtx = prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: hdr})
			originSpanCtx := trace.SpanContextFromContext(msgCtx)
			consumerParentCtx = c.conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
			// Only attach link when origin trace is sampled (avoid dangling link to dropped span).
			if originSpanCtx.IsValid() && originSpanCtx.IsSampled() {
				startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
			}
		}
	}
	_, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
	span.End()
	return msgCtx, msg, nil
}

func (c *tracedConsumer) Fetch(batch int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	raw, err := c.c.Fetch(batch, opts...)
	if err != nil {
		return nil, err
	}
	return newTracedMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *tracedConsumer) FetchBytes(maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	raw, err := c.c.FetchBytes(maxBytes, opts...)
	if err != nil {
		return nil, err
	}
	return newTracedMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *tracedConsumer) FetchNoWait(batch int) (MessageBatch, error) {
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

type tracedPushConsumer struct {
	conn         *otelnats.Conn
	streamName   string
	consumerName string
	c            jetstream.PushConsumer
}

func (c *tracedPushConsumer) Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error) {
	wrapped := tracedConsumeHandler(c.conn, c.consumerName, handler)
	cc, err := c.c.Consume(wrapped, opts...)
	if err != nil {
		return nil, err
	}
	return &consumeContextImpl{cc: cc}, nil
}

func (c *tracedPushConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *tracedPushConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

// tracedConsumeHandler returns the instrumented closure that extracts the message's
// trace context and starts a consumer span before invoking the user handler.
func tracedConsumeHandler(conn *otelnats.Conn, consumerName string, handler MsgHandler) func(jetstream.Msg) {
	tracer, prop := conn.TraceContext()
	propEnabled := conn.PropagationEnabled()
	return func(msg jetstream.Msg) {
		spanName := "process " + msg.Subject()
		attrs := append(receiveAttrs(msg, "process", conn.ServerAttrs()), attribute.String(attrConsumerName, consumerName))
		startOpts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attrs...),
		}
		// Propagation closure: when off, no Extract / no deliver span / no link.
		consumerParentCtx := context.Background()
		if propEnabled {
			if hdr := msg.Headers(); hdr != nil && hdr.Get("traceparent") != "" {
				msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: hdr})
				originSpanCtx := trace.SpanContextFromContext(msgCtx)
				consumerParentCtx = conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
				if originSpanCtx.IsValid() && originSpanCtx.IsSampled() {
					startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
				}
			}
		}
		ctx, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
		defer span.End()
		handler(Msg{Msg: msg, Ctx: ctx})
	}
}

// tracedMessagesContext is the instrumented MessagesContext iterator.
type tracedMessagesContext struct {
	conn         *otelnats.Conn
	consumerName string
	iter         jetstream.MessagesContext
	lastSpan     trace.Span
}

func (m *tracedMessagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	msg, err := m.iter.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	tracer, prop := m.conn.TraceContext()
	spanName := "receive " + msg.Subject()
	attrs := append(receiveAttrs(msg, "receive", m.conn.ServerAttrs()), attribute.String(attrConsumerName, m.consumerName))
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	// Propagation closure: when off, no Extract / no deliver span / no link.
	consumerParentCtx := context.Background()
	if m.conn.PropagationEnabled() {
		if hdr := msg.Headers(); hdr != nil && hdr.Get("traceparent") != "" {
			msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: hdr})
			originSpanCtx := trace.SpanContextFromContext(msgCtx)
			consumerParentCtx = m.conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
			if originSpanCtx.IsValid() && originSpanCtx.IsSampled() {
				startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
			}
		}
	}
	ctx, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
	m.lastSpan = span
	return ctx, msg, nil
}

func (m *tracedMessagesContext) Stop() {
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	m.iter.Stop()
}

func (m *tracedMessagesContext) Drain() {
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	m.iter.Drain()
}
