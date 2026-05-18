package oteljetstream

import (
	"context"
)

// Stream mirrors jetstream.Stream for managing consumers with tracing. Two
// impls exist: tracedStream wraps every consumer-returning method; directStream
// constructs passthrough variants.
type Stream interface {
	Info(ctx context.Context, opts ...StreamInfoOpt) (*StreamInfo, error)
	CachedInfo() *StreamInfo
	Consumer(ctx context.Context, name string) (Consumer, error)
	CreateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	CreateOrUpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	UpdateConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	OrderedConsumer(ctx context.Context, cfg OrderedConsumerConfig) (Consumer, error)
	ListConsumers(ctx context.Context) ConsumerInfoLister
	DeleteConsumer(ctx context.Context, name string) error
	ConsumerNames(ctx context.Context) ConsumerNameLister
}
