package oteljetstream

import (
	"context"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/internal/spanname"
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

// filterDestination returns a consumer's single filter subject as a low-cardinality
// span-name destination, or "" when the consumer has no filter, multiple filters, or its
// filter configuration is not observable here — callers then fall back to the concrete
// delivered subject (see design.md decision D5 in the otel-nats-low-cardinality-span-names
// OpenSpec change).
// resolveMsgSpan computes the span name and the destination-derived attributes for one
// delivered message. All four consume paths go through it so the inbox test, the
// template attribute and the inbox markers cannot drift apart between them.
//
// inboxPrefixes is threaded from the connection rather than passed as nil: a stream can
// capture inbox subjects (archiving replies for durability is legal and deployed), and an
// unfiltered consumer over such a stream resolves its destination to the concrete
// per-request subject — an unbounded span name unless the inbox test runs.
func resolveMsgSpan(op string, msg jetstream.Msg, destination string, inboxPrefixes []string, baseAttrs []attribute.KeyValue) (string, []attribute.KeyValue) {
	name, template, inbox := spanname.Resolve(op, msg.Subject(), destination, inboxPrefixes)
	attrs := receiveMsgAttrs(baseAttrs, msg)
	if template != "" {
		attrs = append(attrs, semconv.MessagingDestinationTemplate(template))
	}
	if inbox {
		attrs = append(attrs, spanname.InboxAttrs(msg.Subject())...)
	}
	return name, attrs
}

func filterDestination(cfg jetstream.ConsumerConfig) string {
	if cfg.FilterSubject != "" {
		return cfg.FilterSubject
	}
	if len(cfg.FilterSubjects) == 1 {
		return cfg.FilterSubjects[0]
	}
	return ""
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

// messageBatchTrace is the dynamic MessageBatch: for every message it re-checks
// the connection's tracing gate and, when on, extracts trace headers and emits a
// receive span. Each span starts and ends before the message is sent to the
// wrapper channel, so consumers always observe an already-ended span
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

// forwardBatch is the MessageBatch forwarding loop shared by the passthrough and
// the dynamic wrapper: it drains raw, converts each message with wrap, and sends
// the result on ch. It selects on done both while waiting to receive from the
// native batch and while waiting to send to the wrapper channel, so Stop() takes
// effect promptly regardless of which side the goroutine is parked on.
//
// wrap is evaluated as the send-case operand, i.e. before the select blocks, so a
// span it creates is always started AND ended before the receiver can observe the
// message (IsRecording() == false at delivery). The trade-off is unchanged: a
// span may be emitted for one final message that Stop() prevents from being
// delivered.
func forwardBatch(raw jetstream.MessageBatch, ch chan<- Msg, done <-chan struct{}, wrap func(jetstream.Msg) Msg) {
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
		case ch <- wrap(msg):
		case <-done:
			return
		}
	}
}

// newDirectMessageBatch wraps a raw jetstream.MessageBatch with the passthrough variant.
func newDirectMessageBatch(raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	done := make(chan struct{})
	go forwardBatch(raw, ch, done, func(msg jetstream.Msg) Msg {
		return Msg{Msg: msg, Ctx: context.Background()}
	})
	return &directMessageBatch{ch: ch, raw: raw, done: done}
}

// newDynamicMessageBatch wraps a raw jetstream.MessageBatch with a forwarder
// that re-checks the connection tracing gate per message (design R2). Construction
// never freezes direct vs traced for the batch lifetime.
func newDynamicMessageBatch(conn *otelnats.Conn, consumerName, destination string, raw jetstream.MessageBatch) MessageBatch {
	ch := make(chan Msg)
	done := make(chan struct{})
	spanner := &receiveSpanner{conn: conn, consumerName: consumerName, destination: destination}
	go forwardBatch(raw, ch, done, spanner.wrap)
	return &messageBatchTrace{ch: ch, raw: raw, done: done}
}

// receiveSpanner turns a jetstream.Msg into a wrapper Msg, emitting a receive
// span whenever the connection's tracing gate is on at that moment.
//
// The tracer, propagator and base attribute set are resolved lazily on the first
// message that finds the gate ON, then reused: they answer through the Conn's
// impl(), so resolving them eagerly could freeze the noop tracer into the
// forwarder, while re-resolving them per message costs one attribute-slice
// allocation per delivered message on the JetStream hot path. Once the gate has
// been observed on, the traced impl behind them is immutable for the Conn's life.
type receiveSpanner struct {
	conn         *otelnats.Conn
	consumerName string
	destination  string

	resolved  bool
	tracer    trace.Tracer
	prop      propagation.TextMapPropagator
	baseAttrs []attribute.KeyValue
}

func (s *receiveSpanner) wrap(msg jetstream.Msg) Msg {
	if !s.conn.TracingEnabled() {
		return Msg{Msg: msg, Ctx: context.Background()}
	}
	if !s.resolved {
		s.tracer, s.prop = s.conn.TraceContext()
		s.baseAttrs = receiveBaseAttrs("receive", s.conn.ServerAttrs(), s.consumerName)
		s.resolved = true
	}
	msgCtx := context.Background()
	if h := msg.Headers(); h != nil {
		msgCtx = s.prop.Extract(msgCtx, &otelnats.HeaderCarrier{H: h})
	}
	name, attrs := resolveMsgSpan("receive", msg, s.destination, s.conn.InboxPrefixes(), s.baseAttrs)
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	}
	if originSpanCtx := trace.SpanContextFromContext(msgCtx); originSpanCtx.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}
	ctx, span := s.tracer.Start(context.Background(), name, opts...)
	span.End()
	return Msg{Msg: msg, Ctx: ctx}
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
