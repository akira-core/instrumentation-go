package oteljetstream

import (
	"context"
	"sync"

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
// Call Error() after the channel is closed. Stop releases the internal goroutine and ends any
// in-flight span; callers that abandon Messages() before it closes must call Stop to avoid leaks.
type MessageBatch interface {
	Messages() <-chan Msg
	Error() error
	Stop()
}

// ConsumerInfo mirrors jetstream.ConsumerInfo.
type ConsumerInfo = jetstream.ConsumerInfo

// Consumer mirrors jetstream.Consumer. Two impls exist: tracedConsumer applies
// the full instrumentation; directConsumer is a passthrough.
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

// PushConsumer mirrors jetstream.PushConsumer.
type PushConsumer interface {
	Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error)
	Info(ctx context.Context) (*ConsumerInfo, error)
	CachedInfo() *ConsumerInfo
}

// Attribute for distinguishing which consumer handled the message (durable/consumer name).
const attrConsumerName = "messaging.consumer.name"

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

// directMessageBatch is the passthrough MessageBatch: forwards messages with empty context.
// No spans, no carriers, no attributes. Stop signals the background goroutine to exit.
type directMessageBatch struct {
	ch       chan Msg
	raw      jetstream.MessageBatch
	done     chan struct{}
	stopOnce sync.Once
}

func (m *directMessageBatch) Messages() <-chan Msg { return m.ch }
func (m *directMessageBatch) Error() error         { return m.raw.Error() }
func (m *directMessageBatch) Stop() {
	m.stopOnce.Do(func() { close(m.done) })
}

// messageBatchTrace is the instrumented MessageBatch: extracts trace headers and emits
// a consumer span per message. The span ends when the next message arrives or the
// batch is exhausted.
type messageBatchTrace struct {
	ch       chan Msg
	raw      jetstream.MessageBatch
	done     chan struct{}
	stopOnce sync.Once
}

func (m *messageBatchTrace) Messages() <-chan Msg { return m.ch }
func (m *messageBatchTrace) Error() error         { return m.raw.Error() }
func (m *messageBatchTrace) Stop() {
	m.stopOnce.Do(func() { close(m.done) })
}

// newDirectMessageBatch wraps a raw jetstream.MessageBatch with the passthrough variant.
func newDirectMessageBatch(raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	done := make(chan struct{})
	go func() {
		defer close(ch)
		for msg := range raw.Messages() {
			select {
			case ch <- Msg{Msg: msg, Ctx: context.Background()}:
			case <-done:
				return
			}
		}
	}()
	return &directMessageBatch{ch: ch, raw: raw, done: done}
}

// newTracedMessageBatch wraps a raw jetstream.MessageBatch with the instrumented variant.
func newTracedMessageBatch(conn *otelnats.Conn, consumerName string, raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	done := make(chan struct{})
	go func() {
		defer close(ch)
		var lastSpan trace.Span
		defer func() {
			if lastSpan != nil {
				lastSpan.End()
			}
		}()
		tracer, prop := conn.TraceContext()
		for msg := range raw.Messages() {
			if lastSpan != nil {
				lastSpan.End()
				lastSpan = nil
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
			select {
			case ch <- Msg{Msg: msg, Ctx: ctx}:
				lastSpan = span
			case <-done:
				span.End()
				return
			}
		}
	}()
	return &messageBatchTrace{ch: ch, raw: raw, done: done}
}

type consumeContextImpl struct {
	cc jetstream.ConsumeContext
}

func (c *consumeContextImpl) Stop() {
	if c.cc != nil {
		c.cc.Stop()
	}
}
