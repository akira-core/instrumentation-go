package oteljetstream

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
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
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: orderedConsumerNameFromConfig(cfg), c: cons}, nil
}

func (s *tracedStream) PushConsumer(ctx context.Context, consumer string) (PushConsumer, error) {
	cons, err := s.s.PushConsumer(ctx, consumer)
	return newTracedPushConsumer(s.conn, consumer, cons, err)
}

func (s *tracedStream) CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.CreatePushConsumer(ctx, cfg)
	return newTracedPushConsumer(s.conn, consumerNameFromConfig(cfg), cons, err)
}

func (s *tracedStream) CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.CreateOrUpdatePushConsumer(ctx, cfg)
	return newTracedPushConsumer(s.conn, consumerNameFromConfig(cfg), cons, err)
}

func (s *tracedStream) UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.UpdatePushConsumer(ctx, cfg)
	return newTracedPushConsumer(s.conn, consumerNameFromConfig(cfg), cons, err)
}

func (s *tracedStream) Unwrap() jetstream.Stream { return s.s }

func (s *tracedStream) ListConsumers(ctx context.Context) ConsumerInfoLister {
	return s.s.ListConsumers(ctx)
}

func (s *tracedStream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

func (s *tracedStream) ConsumerNames(ctx context.Context) ConsumerNameLister {
	return s.s.ConsumerNames(ctx)
}
