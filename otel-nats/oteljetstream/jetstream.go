package oteljetstream

import (
	"context"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

const messagingSystem = "nats"

func publishAttrs(msg *nats.Msg, serverAttrs []attribute.KeyValue) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.MessagingSystemKey.String(messagingSystem),
		semconv.MessagingDestinationNameKey.String(msg.Subject),
		attribute.String(string(semconv.MessagingOperationTypeKey), "send"),
		semconv.MessagingOperationNameKey.String("publish"),
	}
	if len(msg.Data) > 0 {
		attrs = append(attrs, semconv.MessagingMessageBodySize(len(msg.Data)))
	}
	attrs = append(attrs, serverAttrs...)
	return attrs
}

// PubAck is the publish acknowledgement type (alias of jetstream.PubAck).
type PubAck = jetstream.PubAck

// StreamConfig mirrors jetstream.StreamConfig for stream creation.
type StreamConfig = jetstream.StreamConfig

// ConsumerConfig mirrors jetstream.ConsumerConfig for consumer creation.
type ConsumerConfig = jetstream.ConsumerConfig

// StreamInfo mirrors jetstream.StreamInfo (stream metadata from Info).
type StreamInfo = jetstream.StreamInfo

// StreamInfoOpt is option for Stream.Info (e.g. jetstream.WithDeletedDetails).
type StreamInfoOpt = jetstream.StreamInfoOpt

// ConsumerNameLister mirrors jetstream.ConsumerNameLister (iterate consumer names).
type ConsumerNameLister = jetstream.ConsumerNameLister

// ConsumerInfoLister mirrors jetstream.ConsumerInfoLister (iterate consumer infos).
type ConsumerInfoLister = jetstream.ConsumerInfoLister

// OrderedConsumerConfig mirrors jetstream.OrderedConsumerConfig.
type OrderedConsumerConfig = jetstream.OrderedConsumerConfig

// ConsumerPauseResponse mirrors jetstream.ConsumerPauseResponse.
type ConsumerPauseResponse = jetstream.ConsumerPauseResponse

// PushConsumeOpt mirrors jetstream.PushConsumeOpt for PushConsumer.Consume options.
type PushConsumeOpt = jetstream.PushConsumeOpt

// AckPolicy and ack policies mirror jetstream (so callers need not import jetstream).
type AckPolicy = jetstream.AckPolicy

// JetStream ack policies for consumer options.
const (
	AckExplicitPolicy = jetstream.AckExplicitPolicy
	AckNonePolicy     = jetstream.AckNonePolicy
	AckAllPolicy      = jetstream.AckAllPolicy
)

// JetStream is the main interface for JetStream with tracing. Aligns with jetstream.JetStream
// but only sync publish APIs; Publish/PublishMsg accept context for trace.
type JetStream interface {
	Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*PubAck, error)
	PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*PubAck, error)
	Consumer(ctx context.Context, stream string, consumer string) (Consumer, error)
	CreateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error)
	CreateOrUpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error)
	UpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error)
	OrderedConsumer(ctx context.Context, stream string, cfg OrderedConsumerConfig) (Consumer, error)
	DeleteConsumer(ctx context.Context, stream string, consumer string) error
	PauseConsumer(ctx context.Context, stream string, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error)
	ResumeConsumer(ctx context.Context, stream string, consumer string) (*ConsumerPauseResponse, error)
	PushConsumer(ctx context.Context, stream string, consumer string) (PushConsumer, error)
	CreatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error)
	CreateOrUpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error)
	UpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error)
	Stream(ctx context.Context, name string) (Stream, error)
	CreateOrUpdateStream(ctx context.Context, cfg StreamConfig) (Stream, error)
	DeleteStream(ctx context.Context, name string) error
}

type jsImpl struct {
	conn *otelnats.Conn
	js   jetstream.JetStream
}

// New returns a JetStream interface that injects trace from context on Publish and uses the given traced Conn.
// Usage: js, err := oteljetstream.New(otelnatsConn)
func New(conn *otelnats.Conn) (JetStream, error) {
	js, err := jetstream.New(conn.NatsConn())
	if err != nil {
		return nil, err
	}
	return &jsImpl{conn: conn, js: js}, nil
}

func (j *jsImpl) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*PubAck, error) {
	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  make(nats.Header),
	}
	return j.PublishMsg(ctx, msg, opts...)
}

func (j *jsImpl) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*PubAck, error) {
	tracer, prop := j.conn.TraceContext()
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	if !j.conn.TracingEnabled() {
		return j.js.PublishMsg(ctx, msg, opts...)
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

func (j *jsImpl) Stream(ctx context.Context, name string) (Stream, error) {
	s, err := j.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return &streamImpl{conn: j.conn, streamName: name, s: s}, nil
}

func (j *jsImpl) Consumer(ctx context.Context, stream string, consumer string) (Consumer, error) {
	cons, err := j.js.Consumer(ctx, stream, consumer)
	if err != nil {
		return nil, err
	}
	return &consumerImpl{conn: j.conn, streamName: stream, consumerName: consumer, c: cons}, nil
}

func (j *jsImpl) CreateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &consumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) CreateOrUpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &consumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) UpdateConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (Consumer, error) {
	cons, err := j.js.UpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &consumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) OrderedConsumer(ctx context.Context, stream string, cfg OrderedConsumerConfig) (Consumer, error) {
	cons, err := j.js.OrderedConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := cfg.NamePrefix
	if name == "" {
		name = "ordered-consumer"
	}
	return &consumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) DeleteConsumer(ctx context.Context, stream string, consumer string) error {
	return j.js.DeleteConsumer(ctx, stream, consumer)
}

func (j *jsImpl) PauseConsumer(ctx context.Context, stream string, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error) {
	return j.js.PauseConsumer(ctx, stream, consumer, pauseUntil)
}

func (j *jsImpl) ResumeConsumer(ctx context.Context, stream string, consumer string) (*ConsumerPauseResponse, error) {
	return j.js.ResumeConsumer(ctx, stream, consumer)
}

func (j *jsImpl) PushConsumer(ctx context.Context, stream string, consumer string) (PushConsumer, error) {
	cons, err := j.js.PushConsumer(ctx, stream, consumer)
	if err != nil {
		return nil, err
	}
	return &pushConsumerImpl{conn: j.conn, streamName: stream, consumerName: consumer, c: cons}, nil
}

func (j *jsImpl) CreatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.CreatePushConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &pushConsumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) CreateOrUpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.CreateOrUpdatePushConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &pushConsumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) UpdatePushConsumer(ctx context.Context, stream string, cfg ConsumerConfig) (PushConsumer, error) {
	cons, err := j.js.UpdatePushConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	name := consumerNameFromConfig(cfg)
	return &pushConsumerImpl{conn: j.conn, streamName: stream, consumerName: name, c: cons}, nil
}

func (j *jsImpl) CreateOrUpdateStream(ctx context.Context, cfg StreamConfig) (Stream, error) {
	s, err := j.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &streamImpl{conn: j.conn, streamName: cfg.Name, s: s}, nil
}

func (j *jsImpl) DeleteStream(ctx context.Context, name string) error {
	return j.js.DeleteStream(ctx, name)
}
