package oteljetstream

import (
	"context"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

// MsgHandler is the callback for Consume. Receives Msg (implements Msg; use m.Data(), m.Ack(), m.Context()).
// Type name matches nats.MsgHandler and otelnats.MsgHandler for unified naming.
type MsgHandler func(m Msg)

// ConsumeContext is returned by Consume. Same as jetstream.ConsumeContext; call Stop() when done.
type ConsumeContext interface {
	Stop()
}

// MessagesContext is the iterator from Messages(). Same as jetstream.MessagesContext but
// Next() returns (ctx, msg, error) with ctx carrying extracted trace.
type MessagesContext interface {
	Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error)
	Stop()
	Drain()
}

// Msg carries a message and the context with extracted trace. It embeds jetstream.Msg so it implements
// jetstream.Msg (use m.Data(), m.Ack(), m.Headers() etc.); use m.Context() or m.Ctx for the trace context.
type Msg struct {
	jetstream.Msg
	Ctx context.Context
}

// Context returns the context with extracted trace. Use for passing trace into downstream calls.
func (m Msg) Context() context.Context { return m.Ctx }

// MessageBatch is the result of Fetch/FetchBytes/FetchNoWait. Use Messages() for Msg + trace context.
// Call Error() after the channel is closed.
type MessageBatch interface {
	Messages() <-chan Msg
	Error() error
}

// ConsumerInfo mirrors jetstream.ConsumerInfo.
type ConsumerInfo = jetstream.ConsumerInfo

// Consumer mirrors jetstream.Consumer. Consume, Messages, Next; Fetch/FetchBytes/FetchNoWait
// return MessageBatch with Messages() for trace context per message.
type Consumer interface {
	Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error)
	Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error)
	Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error)
	Fetch(batch int, opts ...jetstream.FetchOpt) (MessageBatch, error)
	FetchBytes(maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error)
	FetchNoWait(batch int) (MessageBatch, error)
	Info(ctx context.Context) (*ConsumerInfo, error)
	CachedInfo() *ConsumerInfo
}

// PushConsumer mirrors jetstream.PushConsumer. Consume receives Msg with extracted trace.
type PushConsumer interface {
	Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error)
	Info(ctx context.Context) (*ConsumerInfo, error)
	CachedInfo() *ConsumerInfo
}

// Attribute for distinguishing which consumer handled the message (durable/consumer name).
const attrConsumerName = "messaging.consumer.name"

type consumerImpl struct {
	conn         *otelnats.Conn
	streamName   string
	consumerName string
	c            jetstream.Consumer
}

type pushConsumerImpl struct {
	conn         *otelnats.Conn
	streamName   string
	consumerName string
	c            jetstream.PushConsumer
}

func (c *consumerImpl) Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	wrapped := wrapConsumeHandler(c.conn, c.consumerName, handler)
	cc, err := c.c.Consume(wrapped, opts...)
	if err != nil {
		return nil, err
	}
	return &consumeContextImpl{cc: cc}, nil
}

func (c *pushConsumerImpl) Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error) {
	wrapped := wrapConsumeHandler(c.conn, c.consumerName, handler)
	cc, err := c.c.Consume(wrapped, opts...)
	if err != nil {
		return nil, err
	}
	return &consumeContextImpl{cc: cc}, nil
}

func (c *consumerImpl) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	iter, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &messagesContextImpl{conn: c.conn, consumerName: c.consumerName, iter: iter}, nil
}

func (c *consumerImpl) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	if ctx != nil {
		opts = append([]jetstream.FetchOpt{jetstream.FetchContext(ctx)}, opts...)
	}
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	if !c.conn.TracingEnabled() {
		return context.Background(), msg, nil
	}
	h := msg.Headers()
	if h == nil {
		h = make(nats.Header)
	}
	tracer, prop := c.conn.TraceContext()
	msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: h})
	originSpanCtx := trace.SpanContextFromContext(msgCtx)
	consumerParentCtx := c.conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
	spanName := "receive " + msg.Subject()
	attrs := append(receiveAttrs(msg, "receive", c.conn.ServerAttrs()), attribute.String(attrConsumerName, c.consumerName))
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	if originSpanCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}
	_, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
	span.End()
	return msgCtx, msg, nil
}

func (c *consumerImpl) Fetch(batch int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	raw, err := c.c.Fetch(batch, opts...)
	if err != nil {
		return nil, err
	}
	return wrapMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *consumerImpl) FetchBytes(maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	raw, err := c.c.FetchBytes(maxBytes, opts...)
	if err != nil {
		return nil, err
	}
	return wrapMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *consumerImpl) FetchNoWait(batch int) (MessageBatch, error) {
	raw, err := c.c.FetchNoWait(batch)
	if err != nil {
		return nil, err
	}
	return wrapMessageBatch(c.conn, c.consumerName, raw), nil
}

func (c *consumerImpl) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *consumerImpl) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

func (c *pushConsumerImpl) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *pushConsumerImpl) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

func wrapConsumeHandler(conn *otelnats.Conn, consumerName string, handler MsgHandler) func(jetstream.Msg) {
	tracer, prop := conn.TraceContext()
	return func(msg jetstream.Msg) {
		if !conn.TracingEnabled() {
			handler(Msg{Msg: msg, Ctx: context.Background()})
			return
		}
		h := msg.Headers()
		if h == nil {
			h = make(nats.Header)
		}
		msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: h})
		originSpanCtx := trace.SpanContextFromContext(msgCtx)
		consumerParentCtx := conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
		spanName := "process " + msg.Subject()
		attrs := append(receiveAttrs(msg, "process", conn.ServerAttrs()), attribute.String(attrConsumerName, consumerName))
		startOpts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attrs...),
		}
		if originSpanCtx.IsValid() {
			startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
		}
		ctx, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
		defer span.End()
		handler(Msg{Msg: msg, Ctx: ctx})
	}
}

// receiveAttrs builds consumer span attributes. opType is "process" (push) or "receive" (pull).
// Note: otelnats/conn.go has a parallel receiveAttrs for *nats.Msg — keep both in sync.
func receiveAttrs(msg jetstream.Msg, opType string, serverAttrs []attribute.KeyValue) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.MessagingSystemKey.String(messagingSystem),
		semconv.MessagingDestinationNameKey.String(msg.Subject()),
		attribute.String(string(semconv.MessagingOperationTypeKey), opType),
		semconv.MessagingOperationNameKey.String(opType),
	}
	if d := msg.Data(); len(d) > 0 {
		attrs = append(attrs, semconv.MessagingMessageBodySize(len(d)))
	}
	attrs = append(attrs, serverAttrs...)
	return attrs
}

type messageBatchTrace struct {
	ch  chan Msg
	raw jetstream.MessageBatch
}

// Messages returns a channel of messages with their extracted trace contexts.
// Each span is started when the message is dispatched and ended when the next message
// arrives or the batch is exhausted.
func (m *messageBatchTrace) Messages() <-chan Msg {
	return m.ch
}

func (m *messageBatchTrace) Error() error {
	return m.raw.Error()
}

func wrapMessageBatch(conn *otelnats.Conn, consumerName string, raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	go func() {
		defer close(ch)
		tracer, prop := conn.TraceContext()
		var lastSpan trace.Span
		for msg := range raw.Messages() {
			if lastSpan != nil {
				lastSpan.End()
				lastSpan = nil
			}
			if !conn.TracingEnabled() {
				ch <- Msg{Msg: msg, Ctx: context.Background()}
				continue
			}
			h := msg.Headers()
			if h == nil {
				h = make(nats.Header)
			}
			msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: h})
			originSpanCtx := trace.SpanContextFromContext(msgCtx)
			consumerParentCtx := conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
			attrs := append(receiveAttrs(msg, "receive", conn.ServerAttrs()), attribute.String(attrConsumerName, consumerName))
			opts := []trace.SpanStartOption{
				trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithAttributes(attrs...),
			}
			if originSpanCtx.IsValid() {
				opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
			}
			ctx, span := tracer.Start(consumerParentCtx, "receive "+msg.Subject(), opts...)
			lastSpan = span
			ch <- Msg{Msg: msg, Ctx: ctx}
		}
		if lastSpan != nil {
			lastSpan.End()
		}
	}()
	return &messageBatchTrace{ch: ch, raw: raw}
}

type consumeContextImpl struct {
	cc jetstream.ConsumeContext
}

func (c *consumeContextImpl) Stop() {
	if c.cc != nil {
		c.cc.Stop()
	}
}

type messagesContextImpl struct {
	conn         *otelnats.Conn
	consumerName string
	iter         jetstream.MessagesContext
	lastSpan     trace.Span
}

func (m *messagesContextImpl) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	msg, err := m.iter.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	if !m.conn.TracingEnabled() {
		return context.Background(), msg, nil
	}
	h := msg.Headers()
	if h == nil {
		h = make(nats.Header)
	}
	tracer, prop := m.conn.TraceContext()
	msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: h})
	originSpanCtx := trace.SpanContextFromContext(msgCtx)
	consumerParentCtx := m.conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
	spanName := "receive " + msg.Subject()
	attrs := append(receiveAttrs(msg, "receive", m.conn.ServerAttrs()), attribute.String(attrConsumerName, m.consumerName))
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	if originSpanCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}
	ctx, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
	m.lastSpan = span
	return ctx, msg, nil
}

func (m *messagesContextImpl) Stop() {
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	m.iter.Stop()
}

func (m *messagesContextImpl) Drain() {
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	m.iter.Drain()
}
