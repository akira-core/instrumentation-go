package oteljetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// directStream is the passthrough Stream impl. Consumer-returning methods
// construct direct variants so the entire chain stays branch-free.
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

func (s *directStream) ListConsumers(ctx context.Context) ConsumerInfoLister {
	return s.s.ListConsumers(ctx)
}

func (s *directStream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

func (s *directStream) ConsumerNames(ctx context.Context) ConsumerNameLister {
	return s.s.ConsumerNames(ctx)
}

func (s *directStream) PushConsumer(ctx context.Context, consumer string) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.s.PushConsumer(ctx, consumer))
}

func (s *directStream) CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.s.CreatePushConsumer(ctx, cfg))
}

func (s *directStream) CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.s.CreateOrUpdatePushConsumer(ctx, cfg))
}

func (s *directStream) UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(s.s.UpdatePushConsumer(ctx, cfg))
}

func (s *directStream) PauseConsumer(ctx context.Context, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error) {
	return s.s.PauseConsumer(ctx, consumer, pauseUntil)
}

func (s *directStream) ResumeConsumer(ctx context.Context, consumer string) (*ConsumerPauseResponse, error) {
	return s.s.ResumeConsumer(ctx, consumer)
}

func (s *directStream) UnpinConsumer(ctx context.Context, consumer string, group string) error {
	return s.s.UnpinConsumer(ctx, consumer, group)
}

func (s *directStream) ResetConsumer(ctx context.Context, consumer string) (*ConsumerResetResponse, error) {
	return s.s.ResetConsumer(ctx, consumer)
}

func (s *directStream) ResetConsumerToSequence(ctx context.Context, consumer string, seq uint64) (*ConsumerResetResponse, error) {
	return s.s.ResetConsumerToSequence(ctx, consumer, seq)
}

func (s *directStream) GetMsg(ctx context.Context, seq uint64, opts ...GetMsgOpt) (*RawStreamMsg, error) {
	return s.s.GetMsg(ctx, seq, opts...)
}

func (s *directStream) GetLastMsgForSubject(ctx context.Context, subject string) (*RawStreamMsg, error) {
	return s.s.GetLastMsgForSubject(ctx, subject)
}

func (s *directStream) DeleteMsg(ctx context.Context, seq uint64) error {
	return s.s.DeleteMsg(ctx, seq)
}

func (s *directStream) SecureDeleteMsg(ctx context.Context, seq uint64) error {
	return s.s.SecureDeleteMsg(ctx, seq)
}

func (s *directStream) Purge(ctx context.Context, opts ...StreamPurgeOpt) error {
	return s.s.Purge(ctx, opts...)
}
