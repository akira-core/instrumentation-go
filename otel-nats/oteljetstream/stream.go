package oteljetstream

import (
	"context"
	"time"
)

// Stream mirrors jetstream.Stream in full for managing consumers and messages
// with tracing. Two impls exist: tracedStream wraps every consumer-returning
// method; directStream constructs passthrough variants. Message-management and
// consumer-admin methods (GetMsg, Purge, PauseConsumer, ...) are pure
// passthroughs — no trace propagation applies to these control-plane calls.
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
	PauseConsumer(ctx context.Context, consumer string, pauseUntil time.Time) (*ConsumerPauseResponse, error)
	ResumeConsumer(ctx context.Context, consumer string) (*ConsumerPauseResponse, error)
	UnpinConsumer(ctx context.Context, consumer string, group string) error
	ResetConsumer(ctx context.Context, consumer string) (*ConsumerResetResponse, error)
	ResetConsumerToSequence(ctx context.Context, consumer string, seq uint64) (*ConsumerResetResponse, error)
	PushConsumer(ctx context.Context, consumer string) (PushConsumer, error)
	CreatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error)
	CreateOrUpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error)
	UpdatePushConsumer(ctx context.Context, cfg ConsumerConfig) (PushConsumer, error)
	GetMsg(ctx context.Context, seq uint64, opts ...GetMsgOpt) (*RawStreamMsg, error)
	GetLastMsgForSubject(ctx context.Context, subject string) (*RawStreamMsg, error)
	DeleteMsg(ctx context.Context, seq uint64) error
	SecureDeleteMsg(ctx context.Context, seq uint64) error
	Purge(ctx context.Context, opts ...StreamPurgeOpt) error
}
