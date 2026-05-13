package oteljetstream

import (
	"context"
	"time"
)

// Stream mirrors jetstream.Stream for managing consumers with tracing. Two
// impls exist: tracedStream constructs traced consumer/pushConsumer; directStream
// constructs passthrough variants.
type Stream interface {
	Info(ctx context.Context, opts ...StreamInfoOpt) (*StreamInfo, error)
	CachedInfo() *StreamInfo
	Consumer(ctx context.Context, name string) (Consumer, error)
	PushConsumer(ctx context.Context, name string) (PushConsumer, error)
	CreateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	CreateOrUpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	UpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	OrderedConsumer(ctx context.Context, cfg OrderedConsumerConfig) (Consumer, error)
	PauseConsumer(ctx context.Context, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error)
	ResumeConsumer(ctx context.Context, consumer string) (*ConsumerPauseResponse, error)
	ListConsumers(ctx context.Context) ConsumerInfoLister
	CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error)
	CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error)
	UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error)
	DeleteConsumer(ctx context.Context, name string) error
	ConsumerNames(ctx context.Context) ConsumerNameLister
	UnpinConsumer(ctx context.Context, consumer string, group string) error
}
