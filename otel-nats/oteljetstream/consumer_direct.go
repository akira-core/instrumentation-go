package oteljetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// directConsumer is the passthrough Consumer impl used when tracing is off.
// No spans, no carriers, no attributes.
type directConsumer struct {
	c jetstream.Consumer
}

func (c *directConsumer) Consume(handler MsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	wrapped := func(msg jetstream.Msg) {
		handler(Msg{Msg: msg, Ctx: context.Background()})
	}
	cc, err := c.c.Consume(wrapped, opts...)
	if err != nil {
		return nil, err
	}
	return &consumeContextImpl{cc: cc}, nil
}

func (c *directConsumer) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	iter, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &directMessagesContext{iter: iter}, nil
}

func (c *directConsumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	opts = applyCtxDeadlineToFetchOpts(ctx, opts)
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	return context.Background(), msg, nil
}

// applyCtxDeadlineToFetchOpts converts a ctx deadline to a FetchMaxWait so callers
// retain timeout behavior even though nats.go v1.38.0 lacks jetstream.FetchContext.
func applyCtxDeadlineToFetchOpts(ctx context.Context, opts []jetstream.FetchOpt) []jetstream.FetchOpt {
	if ctx == nil {
		return opts
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return opts
	}
	d := time.Until(dl)
	if d <= 0 {
		return opts
	}
	return append([]jetstream.FetchOpt{jetstream.FetchMaxWait(d)}, opts...)
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

// directPushConsumer is the passthrough PushConsumer impl used when tracing is off.
type directPushConsumer struct {
	c jetstream.PushConsumer
}

func (c *directPushConsumer) Consume(handler MsgHandler, opts ...jetstream.PushConsumeOpt) (ConsumeContext, error) {
	wrapped := func(msg jetstream.Msg) {
		handler(Msg{Msg: msg, Ctx: context.Background()})
	}
	cc, err := c.c.Consume(wrapped, opts...)
	if err != nil {
		return nil, err
	}
	return &consumeContextImpl{cc: cc}, nil
}

func (c *directPushConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	return c.c.Info(ctx)
}

func (c *directPushConsumer) CachedInfo() *ConsumerInfo {
	return c.c.CachedInfo()
}

func (m *directMessagesContext) Stop()  { m.iter.Stop() }
func (m *directMessagesContext) Drain() { m.iter.Drain() }
