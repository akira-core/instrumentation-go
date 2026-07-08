package oteljetstream

import (
	"context"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// directJSImpl is the passthrough JetStream impl used when tracing is off.
// Publish/PublishMsg call the underlying driver directly with no
// instrumentation; Consumer/Stream constructors return direct variants so the
// entire chain stays branch-free.
type directJSImpl struct {
	js jetstream.JetStream
}

func (j *directJSImpl) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*PubAck, error) {
	return j.js.Publish(ctx, subject, data, opts...)
}

func (j *directJSImpl) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*PubAck, error) {
	return j.js.PublishMsg(ctx, msg, opts...)
}

func (j *directJSImpl) Stream(ctx context.Context, name string) (Stream, error) {
	s, err := j.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return &directStream{Stream: s}, nil
}

func (j *directJSImpl) Consumer(ctx context.Context, stream string, consumer string) (Consumer, error) {
	cons, err := j.js.Consumer(ctx, stream, consumer)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (j *directJSImpl) CreateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (j *directJSImpl) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (j *directJSImpl) UpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.UpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (j *directJSImpl) OrderedConsumer(ctx context.Context, stream string, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := j.js.OrderedConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	return &directConsumer{c: cons}, nil
}

func (j *directJSImpl) DeleteConsumer(ctx context.Context, stream string, consumer string) error {
	return j.js.DeleteConsumer(ctx, stream, consumer)
}

func (j *directJSImpl) PushConsumer(ctx context.Context, stream string, consumer string) (PushConsumer, error) {
	return wrapDirectPushConsumer(j.js.PushConsumer(ctx, stream, consumer))
}

func (j *directJSImpl) CreatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(j.js.CreatePushConsumer(ctx, stream, cfg))
}

func (j *directJSImpl) CreateOrUpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(j.js.CreateOrUpdatePushConsumer(ctx, stream, cfg))
}

func (j *directJSImpl) UpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	return wrapDirectPushConsumer(j.js.UpdatePushConsumer(ctx, stream, cfg))
}

func (j *directJSImpl) Unwrap() jetstream.JetStream { return j.js }

func (j *directJSImpl) CreateOrUpdateStream(ctx context.Context, cfg StreamConfig) (Stream, error) {
	s, err := j.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &directStream{Stream: s}, nil
}

func (j *directJSImpl) DeleteStream(ctx context.Context, name string) error {
	return j.js.DeleteStream(ctx, name)
}
