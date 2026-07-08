package oteljetstream

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// directStream is the passthrough Stream impl. Consumer-returning methods
// construct direct variants so the entire chain stays branch-free; every other
// method (Info, consumer-admin, message-management) is promoted verbatim from
// the embedded jetstream.Stream — no trace propagation applies to those
// control-plane calls. The consumer-returning overrides shadow their promoted
// counterparts, so directStream does not satisfy jetstream.Stream and cannot
// leak the raw stream through a type assertion.
type directStream struct {
	jetstream.Stream
}

func (s *directStream) Consumer(ctx context.Context, name string) (Consumer, error) {
	cons, err := s.Stream.Consumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) CreateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.CreateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) CreateOrUpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) UpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.UpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) OrderedConsumer(ctx context.Context, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := s.Stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) PushConsumer(ctx context.Context, consumer string) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.Stream.PushConsumer(ctx, consumer))
}

func (s *directStream) CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.Stream.CreatePushConsumer(ctx, cfg))
}

func (s *directStream) CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.Stream.CreateOrUpdatePushConsumer(ctx, cfg))
}

func (s *directStream) UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.Stream.UpdatePushConsumer(ctx, cfg))
}
