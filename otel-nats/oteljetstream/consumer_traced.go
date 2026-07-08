package oteljetstream

import (
	"context"
	"sync"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
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

func (c *tracedConsumer) Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	wrapped := tracedConsumeHandler(c.conn, c.consumerName, handler)
	return wrapConsumeContext(c.c.Consume(wrapped, opts...))
}

func (c *tracedConsumer) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	iter, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	tracer, prop := c.conn.TraceContext()
	return &tracedMessagesContext{
		conn:      c.conn,
		iter:      iter,
		tracer:    tracer,
		prop:      prop,
		baseAttrs: receiveBaseAttrs("receive", c.conn.ServerAttrs(), c.consumerName),
	}, nil
}

func (c *tracedConsumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	opts, err := applyCtxDeadlineToFetchOpts(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
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
	attrs := receiveMsgAttrs(receiveBaseAttrs("receive", c.conn.ServerAttrs(), c.consumerName), msg)
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
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
	ctx, span := tracer.Start(consumerParentCtx, spanName, startOpts...)
	span.End()
	return ctx, msg, nil
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
	wrapped := tracedConsumeHandler(c.conn, c.consumerName, handler)
	return wrapConsumeContext(c.c.Consume(wrapped, opts...))
}

func (c *tracedPushConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *tracedPushConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

// tracedConsumeHandler returns the instrumented closure that extracts the message's
// trace context and starts a consumer span before invoking the user handler.
// Returns nil for a nil handler so the underlying Consume call surfaces
// jetstream's ErrHandlerRequired instead of panicking in the delivery goroutine.
func tracedConsumeHandler(conn *otelnats.Conn, consumerName string, handler MsgHandler) func(jetstream.Msg) {
	if handler == nil {
		return nil
	}
	tracer, prop := conn.TraceContext()
	baseAttrs := receiveBaseAttrs("process", conn.ServerAttrs(), consumerName)
	return func(msg jetstream.Msg) {
		h := msg.Headers()
		if h == nil {
			h = make(nats.Header)
		}
		msgCtx := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: h})
		originSpanCtx := trace.SpanContextFromContext(msgCtx)
		consumerParentCtx := conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
		spanName := "process " + msg.Subject()
		attrs := receiveMsgAttrs(baseAttrs, msg)
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

// tracedMessagesContext is the instrumented MessagesContext iterator.
//
// lastSpan is guarded by mu: jetstream.MessagesContext explicitly supports
// calling Stop/Drain from another goroutine to unblock a pending Next, so
// Next's span bookkeeping races Stop/Drain without synchronization. The mutex
// is never held across the blocking m.iter.Next call, so Stop can still
// interrupt a waiting Next.
type tracedMessagesContext struct {
	conn      *otelnats.Conn
	iter      jetstream.MessagesContext
	tracer    trace.Tracer
	prop      propagation.TextMapPropagator
	baseAttrs []attribute.KeyValue
	mu        sync.Mutex
	lastSpan  trace.Span
	stopped   bool
}

// endLastSpan ends and clears any in-flight span. trace.Span.End is idempotent,
// so a rare double call on the Stop/Next boundary is harmless. stopping marks
// the iterator as stopped so a span started concurrently with Stop/Drain (in
// the window between iter.Next returning and the lastSpan store) is ended
// immediately instead of leaking.
func (m *tracedMessagesContext) endLastSpan(stopping bool) {
	m.mu.Lock()
	if stopping {
		m.stopped = true
	}
	if m.lastSpan != nil {
		m.lastSpan.End()
		m.lastSpan = nil
	}
	m.mu.Unlock()
}

func (m *tracedMessagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	m.endLastSpan(false)
	msg, err := m.iter.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	h := msg.Headers()
	if h == nil {
		h = make(nats.Header)
	}
	msgCtx := m.prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: h})
	originSpanCtx := trace.SpanContextFromContext(msgCtx)
	consumerParentCtx := m.conn.ConsumerContextWithDeliver(context.Background(), msg.Subject(), originSpanCtx)
	spanName := "receive " + msg.Subject()
	attrs := receiveMsgAttrs(m.baseAttrs, msg)
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	if originSpanCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: originSpanCtx}))
	}
	ctx, span := m.tracer.Start(consumerParentCtx, spanName, startOpts...)
	m.mu.Lock()
	if m.stopped {
		// Stop/Drain already ran endLastSpan; nobody will end this span later.
		span.End()
	} else {
		m.lastSpan = span
	}
	m.mu.Unlock()
	return ctx, msg, nil
}

func (m *tracedMessagesContext) Stop() {
	m.endLastSpan(true)
	m.iter.Stop()
}

func (m *tracedMessagesContext) Drain() {
	m.endLastSpan(true)
	m.iter.Drain()
}
