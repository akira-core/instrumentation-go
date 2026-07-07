package oteljetstream

import (
	"context"
	"time"

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

// Consumer-admin and message-management methods are pure passthroughs: these
// control-plane calls carry no message payload, so no trace context applies.

func (s *tracedStream) PauseConsumer(ctx context.Context, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error) {
	return s.s.PauseConsumer(ctx, consumer, pauseUntil)
}

func (s *tracedStream) ResumeConsumer(ctx context.Context, consumer string) (*ConsumerPauseResponse, error) {
	return s.s.ResumeConsumer(ctx, consumer)
}

func (s *tracedStream) UnpinConsumer(ctx context.Context, consumer string, group string) error {
	return s.s.UnpinConsumer(ctx, consumer, group)
}

func (s *tracedStream) ResetConsumer(ctx context.Context, consumer string) (*ConsumerResetResponse, error) {
	return s.s.ResetConsumer(ctx, consumer)
}

func (s *tracedStream) ResetConsumerToSequence(ctx context.Context, consumer string, seq uint64) (*ConsumerResetResponse, error) {
	return s.s.ResetConsumerToSequence(ctx, consumer, seq)
}

func (s *tracedStream) GetMsg(ctx context.Context, seq uint64, opts ...GetMsgOpt) (*RawStreamMsg, error) {
	return s.s.GetMsg(ctx, seq, opts...)
}

func (s *tracedStream) GetLastMsgForSubject(ctx context.Context, subject string) (*RawStreamMsg, error) {
	return s.s.GetLastMsgForSubject(ctx, subject)
}

func (s *tracedStream) DeleteMsg(ctx context.Context, seq uint64) error {
	return s.s.DeleteMsg(ctx, seq)
}

func (s *tracedStream) SecureDeleteMsg(ctx context.Context, seq uint64) error {
	return s.s.SecureDeleteMsg(ctx, seq)
}

func (s *tracedStream) Purge(ctx context.Context, opts ...StreamPurgeOpt) error {
	return s.s.Purge(ctx, opts...)
}

func (s *tracedStream) ListConsumers(ctx context.Context) ConsumerInfoLister {
	return s.s.ListConsumers(ctx)
}

func (s *tracedStream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

func (s *tracedStream) ConsumerNames(ctx context.Context) ConsumerNameLister {
	return s.s.ConsumerNames(ctx)
}
