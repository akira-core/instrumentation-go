package oteljetstream

import (
	"context"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// tracedJSImpl is the fully-instrumented JetStream impl: Publish/PublishMsg
// open producer spans and inject trace headers; all child wrappers
// (Consumer/Stream) returned are also traced variants.
type tracedJSImpl struct {
	conn *otelnats.Conn
	js   jetstream.JetStream
}

func (j *tracedJSImpl) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*PubAck, error) {
	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  make(nats.Header),
	}
	return j.PublishMsg(ctx, msg, opts...)
}

func (j *tracedJSImpl) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*PubAck, error) {
	tracer, prop := j.conn.TraceContext()
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	if dest := j.conn.TraceDest(); dest != "" {
		msg.Header.Set("Nats-Trace-Dest", dest)
	}
	ctx, span := tracer.Start(ctx, "send "+msg.Subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(publishAttrs(msg, j.conn.ServerAttrs())...),
	)
	defer span.End()
	injectCtx := ctx
	if j.conn.DeliverSpanEnabled() {
		injectCtx = j.conn.StartDeliverSpan(ctx, msg.Subject)
	}
	prop.Inject(injectCtx, &otelnats.HeaderCarrier{H: msg.Header})
	ack, err := j.js.PublishMsg(ctx, msg, opts...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return ack, nil
}

func (j *tracedJSImpl) Stream(ctx context.Context, name string) (Stream, error) {
	s, err := j.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return &tracedStream{conn: j.conn, streamName: name, s: s}, nil
}

func (j *tracedJSImpl) Consumer(ctx context.Context, stream string, consumer string) (Consumer, error) {
	cons, err := j.js.Consumer(ctx, stream, consumer)
	if err != nil {
		return nil, err
	}
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: consumer, c: cons}, nil
}

func (j *tracedJSImpl) CreateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *tracedJSImpl) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *tracedJSImpl) UpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.UpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *tracedJSImpl) OrderedConsumer(ctx context.Context, stream string, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := j.js.OrderedConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: orderedConsumerNameFromConfig(cfg), c: cons}, nil
}

func (j *tracedJSImpl) DeleteConsumer(ctx context.Context, stream string, consumer string) error {
	return j.js.DeleteConsumer(ctx, stream, consumer)
}

func (j *tracedJSImpl) PushConsumer(ctx context.Context, stream string, consumer string) (PushConsumer, error) {
	cons, err := j.js.PushConsumer(ctx, stream, consumer)
	return newTracedPushConsumer(j.conn, consumer, cons, err)
}

func (j *tracedJSImpl) CreatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.CreatePushConsumer(ctx, stream, cfg)
	return newTracedPushConsumer(j.conn, consumerNameFromConfig(cfg), cons, err)
}

func (j *tracedJSImpl) CreateOrUpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.CreateOrUpdatePushConsumer(ctx, stream, cfg)
	return newTracedPushConsumer(j.conn, consumerNameFromConfig(cfg), cons, err)
}

func (j *tracedJSImpl) UpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.UpdatePushConsumer(ctx, stream, cfg)
	return newTracedPushConsumer(j.conn, consumerNameFromConfig(cfg), cons, err)
}

func (j *tracedJSImpl) Unwrap() jetstream.JetStream { return j.js }

func (j *tracedJSImpl) CreateOrUpdateStream(ctx context.Context, cfg StreamConfig) (Stream, error) {
	s, err := j.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &tracedStream{conn: j.conn, streamName: cfg.Name, s: s}, nil
}

func (j *tracedJSImpl) DeleteStream(ctx context.Context, name string) error {
	return j.js.DeleteStream(ctx, name)
}
