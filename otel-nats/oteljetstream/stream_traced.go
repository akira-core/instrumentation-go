package oteljetstream

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// tracedStream constructs traced consumer wrappers for every consumer-returning
// method. Everything else (Info, consumer-admin, message-management) is promoted
// verbatim from the embedded jetstream.Stream — these control-plane calls carry
// no message payload, so no trace context applies. The consumer-returning
// overrides shadow their promoted counterparts, so tracedStream does not
// satisfy jetstream.Stream and cannot leak the raw stream through a type
// assertion.
type tracedStream struct {
	conn       *otelnats.Conn
	streamName string
	jetstream.Stream
}

func (s *tracedStream) Consumer(ctx context.Context, name string) (Consumer, error) {
	cons, err := s.Stream.Consumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) CreateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.CreateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) CreateOrUpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) UpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.UpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: name, c: cons}, nil
}

func (s *tracedStream) OrderedConsumer(ctx context.Context, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &tracedConsumer{conn: s.conn, streamName: s.streamName, consumerName: orderedConsumerNameFromConfig(cfg), c: cons}, nil
}

func (s *tracedStream) PushConsumer(ctx context.Context, consumer string) (PushConsumer, error) {
	cons, err := s.Stream.PushConsumer(ctx, consumer)
	return newTracedPushConsumer(s.conn, consumer, cons, err)
}

func (s *tracedStream) CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.Stream.CreatePushConsumer(ctx, cfg)
	return newTracedPushConsumer(s.conn, consumerNameFromConfig(cfg), cons, err)
}

func (s *tracedStream) CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.Stream.CreateOrUpdatePushConsumer(ctx, cfg)
	return newTracedPushConsumer(s.conn, consumerNameFromConfig(cfg), cons, err)
}

func (s *tracedStream) UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.Stream.UpdatePushConsumer(ctx, cfg)
	return newTracedPushConsumer(s.conn, consumerNameFromConfig(cfg), cons, err)
}
