package oteljetstream

import (
	"context"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// MsgHandler is the callback for Consume. Receives Msg (implements Msg; use m.Data(), m.Ack(), m.Context()).
// Type name matches nats.MsgHandler and otelnats.MsgHandler for unified naming.
type MsgHandler func(m Msg)

// ConsumeContext is returned by Consume. It mirrors jetstream.ConsumeContext in
// full — every upstream method is re-exposed, so no escape hatch is needed.
type ConsumeContext interface {
	// Stop unsubscribes and cancels the subscription; buffered messages are discarded.
	Stop()
	// Drain unsubscribes and cancels the subscription; buffered messages are still
	// processed by the handler before shutdown completes.
	Drain()
	// Closed returns a channel closed once consuming is fully stopped/drained and
	// no more messages will be delivered.
	Closed() <-chan struct{}
}

// MessagesContext is the iterator from Messages(). Same as jetstream.MessagesContext but
// Next() returns (ctx, msg, error) with ctx carrying extracted trace. NextOpt options
// (jetstream.NextContext, jetstream.NextMaxWait) are passed through to the underlying iterator.
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
// Call Error() after the channel is closed. Stop releases the internal forwarding goroutine —
// each message's receive span has already ended before the message is delivered, so Stop has no
// span bookkeeping to finish; callers that abandon Messages() before it closes must call Stop to
// avoid leaks.
type MessageBatch interface {
	Messages() <-chan Msg
	Error() error
	Stop()
}

// ConsumerInfo mirrors jetstream.ConsumerInfo.
type ConsumerInfo = jetstream.ConsumerInfo

// PushConsumer mirrors jetstream.PushConsumer (added upstream in the nats.go
// v1.38.0→v1.50.0 range): a push-based consumer that delivers messages via
// Consume only — no Fetch/Messages/Next pull paths. Two impls exist:
// tracedPushConsumer applies the full instrumentation; directPushConsumer is a
// passthrough. Requires ConsumerConfig.DeliverSubject to be set.
type PushConsumer interface {
	Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error)
	Info(ctx context.Context) (*ConsumerInfo, error)
	CachedInfo() *ConsumerInfo
}

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

// receiveBaseAttrs builds the consumer-constant span attributes — everything
// except the per-message subject and body size — so hot loops can compute them
// once. opType is "process" (push) or "receive" (pull). The returned slice has
// its capacity clamped so per-message appends never alias the shared base.
// The consumer/durable name is attached under the semconv v1.39.0 consumer-
// group key (semconv.MessagingConsumerGroupNameKey): a JetStream durable
// consumer is, semantically, a consumer group (multiple instances can pull
// from the same durable). The non-semconv literal "messaging.consumer.name"
// was used before 0.7.0; see the CHANGELOG for the migration.
// Note: otelnats/conn.go has a parallel receiveAttrs for *nats.Msg — keep both in sync.
func receiveBaseAttrs(opType string, serverAttrs []attribute.KeyValue, consumerName string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 4+len(serverAttrs))
	attrs = append(attrs,
		semconv.MessagingSystemKey.String(messagingSystem),
		attribute.String(string(semconv.MessagingOperationTypeKey), opType),
		semconv.MessagingOperationNameKey.String(opType),
		semconv.MessagingConsumerGroupNameKey.String(consumerName),
	)
	attrs = append(attrs, serverAttrs...)
	return attrs[:len(attrs):len(attrs)]
}

// receiveMsgAttrs appends the per-message attributes (subject, body size) to a
// base built by receiveBaseAttrs.
func receiveMsgAttrs(base []attribute.KeyValue, msg jetstream.Msg) []attribute.KeyValue {
	attrs := append(base, semconv.MessagingDestinationNameKey.String(msg.Subject()))
	if d := msg.Data(); len(d) > 0 {
		attrs = append(attrs, semconv.MessagingMessageBodySize(len(d)))
	}
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
// a receive span per message. Each span starts and ends before the message is sent to
// the wrapper channel, so consumers always observe an already-ended span
// (IsRecording() == false at delivery).
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
// The forwarding loop selects on done both while waiting to receive from the
// native batch and while waiting to send to the wrapper channel, so Stop()
// takes effect promptly regardless of which side the goroutine is parked on.
func newDirectMessageBatch(raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	done := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			var msg jetstream.Msg
			var ok bool
			select {
			case msg, ok = <-raw.Messages():
				if !ok {
					return
				}
			case <-done:
				return
			}
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
// The forwarding loop selects on done both while waiting to receive from the
// native batch and while waiting to send to the wrapper channel, so Stop()
// takes effect promptly regardless of which side the goroutine is parked on.
// Each message's receive span starts and ends BEFORE the channel send — the
// receiver must observe an already-ended span (IsRecording() == false at
// delivery, the spec's observable contract, matching single-shot
// Consumer.Next and MessagesContext.Next); ending after a successful send
// would race the receiver's check across the channel rendezvous. As a
// consequence, a span may be emitted for one final message that Stop()
// prevents from being delivered. The ended span's SpanContext still parents
// caller-created child spans; callers measure processing time with their own
// child spans.
func newTracedMessageBatch(conn *otelnats.Conn, consumerName string, raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	done := make(chan struct{})
	go func() {
		defer close(ch)
		tracer, prop := conn.TraceContext()
		baseAttrs := receiveBaseAttrs("receive", conn.ServerAttrs(), consumerName)
		for {
			var msg jetstream.Msg
			var ok bool
			select {
			case msg, ok = <-raw.Messages():
				if !ok {
					return
				}
			case <-done:
				return
			}
			msgCtx := context.Background()
			if h := msg.Headers(); h != nil {
				msgCtx = prop.Extract(msgCtx, &otelnats.HeaderCarrier{H: h})
			}
			originSpanCtx := trace.SpanContextFromContext(msgCtx)
			attrs := receiveMsgAttrs(baseAttrs, msg)
			opts := []trace.SpanStartOption{
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(attrs...),
			}
			if originSpanCtx.IsValid() {
				opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
			}
			ctx, span := tracer.Start(context.Background(), "receive "+msg.Subject(), opts...)
			span.End()
			select {
			case ch <- Msg{Msg: msg, Ctx: ctx}:
			case <-done:
				return
			}
		}
	}()
	return &messageBatchTrace{ch: ch, raw: raw, done: done}
}

// wrapConsumeContext adapts the (jetstream.ConsumeContext, error) pair from an
// underlying Consume call to the local interface. The local ConsumeContext
// mirrors jetstream.ConsumeContext exactly, so the raw value is returned as-is.
func wrapConsumeContext(cc jetstream.ConsumeContext, err error) (ConsumeContext, error) {
	if err != nil {
		return nil, err
	}
	return cc, nil
}
