package oteljetstream

import (
	"context"

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
	opts, err := applyCtxToFetchOpts(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	msg, err := c.c.Next(opts...)
	if err != nil {
		return nil, nil, err
	}
	return context.Background(), msg, nil
}

// applyCtxToFetchOpts wires ctx into the fetch via jetstream.FetchContext, so
// Next honors both live cancellation and deadline expiry — FetchContext
// derives the server-side expiry from ctx's own deadline (with its own
// buffer logic), and its internal fetch goroutine selects on ctx.Done()
// alongside message arrival, so a canceled ctx unblocks Next promptly instead
// of waiting for the server round trip. An already-canceled or expired ctx
// returns its error immediately so Next fails fast without a round trip.
// A ctx that can never fire (ctx == nil, or ctx.Done() == nil as with
// context.Background()/context.TODO()) skips the wiring entirely — this
// matters because FetchContext cannot be combined with a caller-supplied
// FetchMaxWait opt (jetstream returns ErrInvalidOption), and callers commonly
// pass context.Background() alongside their own FetchMaxWait when they don't
// need cancellation; only a genuinely cancelable ctx (WithCancel/WithTimeout/
// WithDeadline) triggers FetchContext, so that combination still surfaces
// jetstream's native "cannot specify both FetchContext and FetchMaxWait"
// error — callers needing both a live ctx and a custom max wait use ctx's own
// deadline instead of a separate FetchMaxWait opt.
//
// The wrapper's FetchContext is appended AFTER the caller's opts: jetstream
// applies fetch options in order and FetchContext overwrites the request ctx,
// so appending last makes the method parameter ctx authoritative — a
// caller-supplied FetchContext(otherCtx) cannot silently disable Next(ctx)
// cancellation. (A caller FetchContext whose deadline already set the request
// expiry keeps that expiry as an effective max wait; cancellation authority
// still comes from the method ctx.)
func applyCtxToFetchOpts(ctx context.Context, opts []jetstream.FetchOpt) ([]jetstream.FetchOpt, error) {
	if ctx == nil || ctx.Done() == nil {
		return opts, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]jetstream.FetchOpt, 0, len(opts)+1)
	out = append(out, opts...)
	out = append(out, jetstream.FetchContext(ctx))
	return out, nil
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
