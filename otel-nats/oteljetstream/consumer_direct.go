package oteljetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// directHandler adapts a user MsgHandler to the raw jetstream handler with an
// empty context (tracing off). Returns nil for a nil handler so the underlying
// Consume surfaces jetstream's ErrHandlerRequired instead of panicking in the
// delivery goroutine.
func directHandler(handler MsgHandler) func(jetstream.Msg) {
	if handler == nil {
		return nil
	}
	return func(msg jetstream.Msg) {
		handler(Msg{Msg: msg, Ctx: context.Background()})
	}
}

// directConsumer is the passthrough Consumer impl used when tracing is off.
// No spans, no carriers, no attributes.
type directConsumer struct {
	c jetstream.Consumer
}

func (c *directConsumer) Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	return wrapConsumeContext(c.c.Consume(directHandler(handler), opts...))
}

func (c *directConsumer) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	iter, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &directMessagesContext{iter: iter}, nil
}

func (c *directConsumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	opts, err := applyCtxDeadlineToFetchOpts(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	return context.Background(), msg, nil
}

// applyCtxDeadlineToFetchOpts converts a ctx deadline to a FetchMaxWait so
// callers retain timeout behavior (the underlying pull Next takes no ctx; we
// avoid jetstream.FetchContext because it errors when combined with a
// caller-passed FetchMaxWait). An already-canceled or expired ctx returns its
// error so Next fails fast instead of blocking for jetstream's default max wait.
func applyCtxDeadlineToFetchOpts(ctx context.Context, opts []jetstream.FetchOpt) ([]jetstream.FetchOpt, error) {
	if ctx == nil {
		return opts, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return opts, nil
	}
	d := time.Until(dl)
	if d <= 0 {
		return nil, context.DeadlineExceeded
	}
	return append([]jetstream.FetchOpt{jetstream.FetchMaxWait(d)}, opts...), nil
}

func (c *directConsumer) Fetch(batch int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	raw, err := c.c.Fetch(batch, opts...)
	if err != nil {
		return nil, err
	}
	return newDirectMessageBatch(raw), nil
}

func (c *directConsumer) FetchBytes(maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	raw, err := c.c.FetchBytes(maxBytes, opts...)
	if err != nil {
		return nil, err
	}
	return newDirectMessageBatch(raw), nil
}

func (c *directConsumer) FetchNoWait(batch int) (MessageBatch, error) {
	raw, err := c.c.FetchNoWait(batch)
	if err != nil {
		return nil, err
	}
	return newDirectMessageBatch(raw), nil
}

func (c *directConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *directConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

// directMessagesContext is the passthrough MessagesContext iterator.
type directMessagesContext struct {
	iter jetstream.MessagesContext
}

func (m *directMessagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	msg, err := m.iter.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	return context.Background(), msg, nil
}

func (m *directMessagesContext) Stop()  { m.iter.Stop() }
func (m *directMessagesContext) Drain() { m.iter.Drain() }

// directPushConsumer is the passthrough PushConsumer impl used when tracing is off.
type directPushConsumer struct {
	c jetstream.PushConsumer
}

func (c *directPushConsumer) Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error) {
	return wrapConsumeContext(c.c.Consume(directHandler(handler), opts...))
}

func (c *directPushConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *directPushConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

// wrapDirectPushConsumer wraps a raw jetstream.PushConsumer (and its
// constructor error) as the passthrough PushConsumer impl.
func wrapDirectPushConsumer(cons jetstream.PushConsumer, err error) (PushConsumer, error) {
	if err != nil {
		return nil, err
	}
	return &directPushConsumer{c: cons}, nil
}
