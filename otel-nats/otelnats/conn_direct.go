package otelnats

import (
	"context"
	"time"

	nats "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// directConn is the passthrough connImpl used when tracing is off. No spans,
// no propagator inject/extract, no deliver tracer (the invariant from
// conn.go's old construction site is now enforced by type: this impl never
// holds a deliverTracer). Subscribe handlers receive Msg with an empty Ctx.
type directConn struct {
	nc *nats.Conn
}

func (d *directConn) TracingEnabled() bool     { return false }
func (d *directConn) DeliverSpanEnabled() bool { return false }

// TraceContext returns a noop tracer and the global propagator. External
// callers that capture these (and call .Start on the tracer) see zero spans.
// The propagator is returned non-nil so callers that pass it elsewhere don't
// have to nil-check.
func (d *directConn) TraceContext() (trace.Tracer, propagation.TextMapPropagator) {
	return noop.NewTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version())), propagation.NewCompositeTextMapPropagator()
}

func (d *directConn) ServerAttrs() []attribute.KeyValue { return nil }
func (d *directConn) TraceDest() string                 { return "" }

func (d *directConn) Publish(_ context.Context, subject string, data []byte) error {
	return d.nc.Publish(subject, data)
}

func (d *directConn) PublishMsg(_ context.Context, msg *nats.Msg) error {
	return d.nc.PublishMsg(msg)
}

func (d *directConn) Request(subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	return d.nc.Request(subject, data, timeout)
}

func (d *directConn) RequestWithContext(ctx context.Context, subject string, data []byte) (*nats.Msg, error) {
	return d.nc.RequestWithContext(ctx, subject, data)
}

func (d *directConn) RequestMsg(msg *nats.Msg, timeout time.Duration) (*nats.Msg, error) {
	return d.nc.RequestMsg(msg, timeout)
}

func (d *directConn) RequestMsgWithContext(ctx context.Context, msg *nats.Msg) (*nats.Msg, error) {
	return d.nc.RequestMsgWithContext(ctx, msg)
}

func (d *directConn) wrapMsgHandler(_, _ string, handler MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		handler(Msg{Msg: msg, Ctx: context.Background()})
	}
}

// traceEventHandler returns a no-op subscription handler — when tracing is
// off there are no trace events to emit. The subscription itself still runs
// so callers can attach Unsubscribe lifecycle as usual.
func (d *directConn) traceEventHandler() nats.MsgHandler {
	return func(*nats.Msg) {}
}

func (d *directConn) StartDeliverSpan(ctx context.Context, _ string) context.Context {
	return ctx
}

func (d *directConn) ConsumerContextWithDeliver(ctx context.Context, _ string, _ trace.SpanContext) context.Context {
	return ctx
}
