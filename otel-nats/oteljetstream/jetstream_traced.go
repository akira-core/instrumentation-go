package oteljetstream

import (
	"context"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/internal/spanname"
	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// tracedJSImpl is the fully-instrumented JetStream impl: Publish/PublishMsg
// open producer spans and inject trace headers; all child wrappers
// (Consumer/Stream) returned are also traced variants.
type tracedJSImpl struct {
	conn *otelnats.Conn
	js   jetstream.JetStream
}

// on reports whether THIS call should be instrumented. Read per call so a relay
// flag change reaches a JetStream handle that an application typically creates
// once at startup and keeps for its lifetime.
//
// Methods that RETURN a wrapper (Consumer, Stream, PushConsumer) are deliberately
// NOT gated: they always hand back the instrumented wrapper, which gates its own
// methods. Returning a passthrough wrapper while the flag happened to be off
// would pin that consumer or stream forever, which is exactly what per-call
// resolution exists to avoid.
func (j *tracedJSImpl) on() bool { return j.conn.TracingEnabled() }

func (j *tracedJSImpl) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*PubAck, error) {
	if !j.on() {
		return j.js.Publish(ctx, subject, data, opts...)
	}
	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  make(nats.Header),
	}
	return j.PublishMsg(ctx, msg, opts...)
}

func (j *tracedJSImpl) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*PubAck, error) {
	if !j.on() {
		return j.js.PublishMsg(ctx, msg, opts...)
	}
	tracer, prop := j.conn.TraceContext()
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	if dest := j.conn.TraceDest(); dest != "" {
		msg.Header.Set("Nats-Trace-Dest", dest)
	}
	// A stream may capture inbox subjects — archiving replies for durability is a legal
	// and deployed configuration — so the inbox test applies here exactly as it does to
	// core NATS. No filter subject on a publish, so Resolve can never surface a
	// destination template here.
	name, _, inbox := spanname.Resolve("publish", msg.Subject, "", j.conn.InboxPrefixes())
	attrs := publishAttrs(msg, j.conn.ServerAttrs())
	if inbox {
		attrs = append(attrs, spanname.InboxAttrs(msg.Subject)...)
	}
	ctx, span := tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attrs...),
	)
	defer span.End()
	prop.Inject(ctx, &otelnats.HeaderCarrier{H: msg.Header})
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
	return &tracedStream{conn: j.conn, streamName: name, Stream: s}, nil
}

func (j *tracedJSImpl) Consumer(ctx context.Context, stream string, consumer string) (Consumer, error) {
	cons, err := j.js.Consumer(ctx, stream, consumer)
	if err != nil {
		return nil, err
	}
	var destination string
	if info := cons.CachedInfo(); info != nil {
		destination = filterDestination(info.Config)
	}
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: consumer, destination: destination, c: cons}, nil
}

func (j *tracedJSImpl) CreateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	destination := filterDestination(cfg)
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: name, destination: destination, c: cons}, nil
}

func (j *tracedJSImpl) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	destination := filterDestination(cfg)
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: name, destination: destination, c: cons}, nil
}

func (j *tracedJSImpl) UpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.UpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	destination := filterDestination(cfg)
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: name, destination: destination, c: cons}, nil
}

func (j *tracedJSImpl) OrderedConsumer(ctx context.Context, stream string, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := j.js.OrderedConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	destination := filterDestination(jetstream.ConsumerConfig{FilterSubjects: cfg.FilterSubjects})
	return &tracedConsumer{conn: j.conn, streamName: stream, consumerName: orderedConsumerNameFromConfig(cfg), destination: destination, c: cons}, nil
}

func (j *tracedJSImpl) DeleteConsumer(ctx context.Context, stream string, consumer string) error {
	return j.js.DeleteConsumer(ctx, stream, consumer)
}

func (j *tracedJSImpl) PushConsumer(ctx context.Context, stream string, consumer string) (PushConsumer, error) {
	cons, err := j.js.PushConsumer(ctx, stream, consumer)
	var destination string
	if err == nil {
		if info := cons.CachedInfo(); info != nil {
			destination = filterDestination(info.Config)
		}
	}
	return newTracedPushConsumer(j.conn, consumer, destination, cons, err)
}

func (j *tracedJSImpl) CreatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.CreatePushConsumer(ctx, stream, cfg)
	return newTracedPushConsumer(j.conn, consumerNameFromConfig(cfg), filterDestination(cfg), cons, err)
}

func (j *tracedJSImpl) CreateOrUpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.CreateOrUpdatePushConsumer(ctx, stream, cfg)
	return newTracedPushConsumer(j.conn, consumerNameFromConfig(cfg), filterDestination(cfg), cons, err)
}

func (j *tracedJSImpl) UpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.UpdatePushConsumer(ctx, stream, cfg)
	return newTracedPushConsumer(j.conn, consumerNameFromConfig(cfg), filterDestination(cfg), cons, err)
}

func (j *tracedJSImpl) Unwrap() jetstream.JetStream { return j.js }

func (j *tracedJSImpl) CreateOrUpdateStream(ctx context.Context, cfg StreamConfig) (Stream, error) {
	s, err := j.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &tracedStream{conn: j.conn, streamName: cfg.Name, Stream: s}, nil
}

func (j *tracedJSImpl) DeleteStream(ctx context.Context, name string) error {
	return j.js.DeleteStream(ctx, name)
}
