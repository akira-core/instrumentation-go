package oteljetstream

import (
	"context"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

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

// JetStream is the main interface for JetStream with tracing. Two impls exist:
// tracedJSImpl applies full instrumentation; directJSImpl is a passthrough.
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

// New returns a JetStream interface that propagates trace context across publishes
// and consumer paths. The returned impl is chosen by conn.TracingEnabled() —
// tracedJSImpl when on, directJSImpl when off — so per-method gates disappear.
//
// Usage: js, err := oteljetstream.New(otelnatsConn)
func New(conn *otelnats.Conn) (JetStream, error) {
	js, err := jetstream.New(conn.NatsConn())
	if err != nil {
		return nil, err
	}
	if conn.TracingEnabled() {
		return &tracedJSImpl{conn: conn, js: js}, nil
	}
	return &directJSImpl{js: js}, nil
}

func consumerNameFromConfig(cfg ConsumerConfig) string {
	name := cfg.Durable
	if name == "" && cfg.Name != "" {
		name = cfg.Name
	}
	if name == "" {
		name = "consumer"
	}
	return name
}
