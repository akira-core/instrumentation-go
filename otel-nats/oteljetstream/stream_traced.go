package oteljetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

// tracedStream constructs traced consumer wrappers for every consumer-returning method.
type tracedStream struct {
	conn       *otelnats.Conn
	streamName string
	s          jetstream.Stream
}

func (s *tracedStream) Info(ctx context.Context, opts ...StreamInfoOpt) (*StreamInfo, error) {
	return s.s.Info(ctx, opts...)
}

func (s *tracedStream) CachedInfo() *StreamInfo { return s.s.CachedInfo() }

func (s *tracedStream) Consumer(ctx context.Context, name string) (Consumer, error) {
	cons, err := s.s.Consumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) PushConsumer(ctx context.Context, name string) (PushConsumer, error) {
	cons, err := s.s.PushConsumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &tracedPushConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) CreateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.s.CreateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) CreateOrUpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.s.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) UpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.s.UpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) OrderedConsumer(ctx context.Context, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := s.s.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := cfg.NamePrefix
	if name == "" {
		name = "ordered-consumer"
	}
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) PauseConsumer(ctx context.Context, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error) {
	return s.s.PauseConsumer(ctx, consumer, pauseUntil)
}

func (s *tracedStream) ResumeConsumer(ctx context.Context, consumer string) (*ConsumerPauseResponse, error) {
	return s.s.ResumeConsumer(ctx, consumer)
}

func (s *tracedStream) ListConsumers(ctx context.Context) ConsumerInfoLister {
	return s.s.ListConsumers(ctx)
}

func (s *tracedStream) CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.CreatePushConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedPushConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.CreateOrUpdatePushConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedPushConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.UpdatePushConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedPushConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

func (s *tracedStream) ConsumerNames(ctx context.Context) ConsumerNameLister {
	return s.s.ConsumerNames(ctx)
}

func (s *tracedStream) UnpinConsumer(ctx context.Context, consumer string, group string) error {
	return s.s.UnpinConsumer(ctx, consumer, group)
}
