package oteljetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// directStream is the passthrough Stream impl. Consumer/PushConsumer-returning
// methods construct direct variants so the entire chain stays branch-free.
type directStream struct {
	s jetstream.Stream
}

func (s *directStream) Info(ctx context.Context, opts ...StreamInfoOpt) (*StreamInfo, error) {
	return s.s.Info(ctx, opts...)
}

func (s *directStream) CachedInfo() *StreamInfo { return s.s.CachedInfo() }

func (s *directStream) Consumer(ctx context.Context, name string) (Consumer, error) {
	cons, err := s.s.Consumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) PushConsumer(ctx context.Context, name string) (PushConsumer, error) {
	cons, err := s.s.PushConsumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &directPushConsumer{c: cons}, nil
}

func (s *directStream) CreateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.s.CreateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) CreateOrUpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.s.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) UpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	cons, err := s.s.UpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) OrderedConsumer(ctx context.Context, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := s.s.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (s *directStream) PauseConsumer(ctx context.Context, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error) {
	return s.s.PauseConsumer(ctx, consumer, pauseUntil)
}

func (s *directStream) ResumeConsumer(ctx context.Context, consumer string) (*ConsumerPauseResponse, error) {
	return s.s.ResumeConsumer(ctx, consumer)
}

func (s *directStream) ListConsumers(ctx context.Context) ConsumerInfoLister {
	return s.s.ListConsumers(ctx)
}

func (s *directStream) CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.CreatePushConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directPushConsumer{c: cons}, nil
}

func (s *directStream) CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.CreateOrUpdatePushConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directPushConsumer{c: cons}, nil
}

func (s *directStream) UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := s.s.UpdatePushConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directPushConsumer{c: cons}, nil
}

func (s *directStream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

func (s *directStream) ConsumerNames(ctx context.Context) ConsumerNameLister {
	return s.s.ConsumerNames(ctx)
}

func (s *directStream) UnpinConsumer(ctx context.Context, consumer string, group string) error {
	return s.s.UnpinConsumer(ctx, consumer, group)
}
