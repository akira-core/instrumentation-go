package otelnats

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"time"

	nats "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
	ScopeName              = "instrumentation-go/otel-nats/otelnats"
	instrumentationVersion = "0.8.0"
	messagingSystem        = "nats"
)

// Msg carries a message and the context with extracted trace (Subscribe/QueueSubscribe).
// Use m.Msg for the message and m.Context() for the trace context.
type Msg struct {
	Msg *nats.Msg
	Ctx context.Context
}

// Context returns the context with extracted trace.
func (m Msg) Context() context.Context { return m.Ctx }

// MsgHandler is the callback for subscriptions. Same as nats.MsgHandler but receives Msg
// (trace in m.Context(), message in m.Msg). Type name matches nats.MsgHandler.
type MsgHandler func(m Msg)

// Conn is a tracing-aware wrapper around *nats.Conn. API mirrors nats.Conn; the only
// difference is Publish/PublishMsg take context.Context and handlers receive Msg.
// All instrumentation behaviour lives behind a polymorphic connImpl — tracedConn
// when tracing is on, directConn (passthrough) when off.
//
// Selection happens per operation, not once at construction, so a relay flag
// change reaches a long-lived connection without it being re-established. NO
// connection is ever static — not even one carrying WithTracingEnabled, which
// supplies the first tier and says nothing about the relay.
//
// What construction does fix is the ceiling: traced is nil when the two
// environment-derived tiers say this connection could never trace, and then no
// OTel SDK code path is reachable for its lifetime and the resolver is never
// consulted.
type Conn struct {
	nc *nats.Conn

	// direct is always non-nil. traced is non-nil only when the static tiers
	// allow tracing, and is then selected per operation by the relay verdict.
	direct connImpl
	traced connImpl
}

// impl returns the implementation this operation runs through.
// The condition must stay lockstep with msgHandler and traceEventMsgHandler
// (traced == nil? / natsRelayAllows?) — do not change one alone.
func (c *Conn) impl() connImpl {
	if c.traced != nil && natsRelayAllows() {
		return c.traced
	}
	return c.direct
}

// msgHandler returns the nats.MsgHandler bound into a subscription. A
// subscription is created once and often lives for the whole process, so it
// must NOT pin impl()'s answer at subscribe time — both handlers are built once
// here and the relay verdict is re-resolved per message, so a subscription
// created before a revocation follows it.
func (c *Conn) msgHandler(subject, queue string, handler MsgHandler) nats.MsgHandler {
	if c.traced == nil {
		return c.direct.wrapMsgHandler(subject, queue, handler)
	}
	th := c.traced.wrapMsgHandler(subject, queue, handler)
	dh := c.direct.wrapMsgHandler(subject, queue, handler)
	return func(m *nats.Msg) {
		if natsRelayAllows() {
			th(m)
		} else {
			dh(m)
		}
	}
}

// traceEventMsgHandler is msgHandler's sibling for SubscribeTraceEvents: hop
// spans follow the relay verdict per event rather than being pinned at
// subscribe time.
func (c *Conn) traceEventMsgHandler() nats.MsgHandler {
	if c.traced == nil {
		return c.direct.traceEventHandler()
	}
	th := c.traced.traceEventHandler()
	dh := c.direct.traceEventHandler()
	return func(m *nats.Msg) {
		if natsRelayAllows() {
			th(m)
		} else {
			dh(m)
		}
	}
}

// connImpl is the polymorphic core of Conn. Two implementations exist
// (tracedConn / directConn).
type connImpl interface {
	Publish(ctx context.Context, subject string, data []byte) error
	PublishMsg(ctx context.Context, msg *nats.Msg) error
	Request(subject string, data []byte, timeout time.Duration) (*nats.Msg, error)
	RequestWithContext(ctx context.Context, subject string, data []byte) (*nats.Msg, error)
	RequestMsg(msg *nats.Msg, timeout time.Duration) (*nats.Msg, error)
	RequestMsgWithContext(ctx context.Context, msg *nats.Msg) (*nats.Msg, error)
	wrapMsgHandler(subject, queue string, handler MsgHandler) nats.MsgHandler
	traceEventHandler() nats.MsgHandler
	TracingEnabled() bool
	TraceContext() (trace.Tracer, propagation.TextMapPropagator)
	ServerAttrs() []attribute.KeyValue
	TraceDest() string
}

// Option configures Conn. Per OTel contrib: accept TracerProvider and Propagators, not Tracer.
type Option interface {
	apply(*connConfig)
}

type optionFunc func(*connConfig)

func (f optionFunc) apply(c *connConfig) { f(c) }

type connConfig struct {
	TracerProvider trace.TracerProvider
	Propagators    propagation.TextMapPropagator
	TraceDest      string
	TracingEnabled *bool
}

func newConnConfig(opts ...Option) *connConfig {
	c := &connConfig{}
	for _, o := range opts {
		if o == nil {
			continue
		}
		o.apply(c)
	}
	return c
}

// WithTracerProvider sets the TracerProvider for this Conn. Defaults to otel.GetTracerProvider().
func WithTracerProvider(tp trace.TracerProvider) Option {
	return optionFunc(func(c *connConfig) {
		if tp != nil {
			c.TracerProvider = tp
		}
	})
}

// WithPropagators sets the TextMapPropagator for inject/extract. Defaults to otel.GetTextMapPropagator().
func WithPropagators(p propagation.TextMapPropagator) Option {
	return optionFunc(func(c *connConfig) {
		if p != nil {
			c.Propagators = p
		}
	})
}

// WithTraceDestination sets the Nats-Trace-Dest header value injected on every PublishMsg call.
// When set, the NATS server (2.11+) publishes infrastructure trace events to that subject,
// which can be consumed by SubscribeTraceEvents to emit OTel spans. Empty string disables.
func WithTraceDestination(subject string) Option {
	return optionFunc(func(c *connConfig) {
		c.TraceDest = subject
	})
}

// WithTracingEnabled supplies the process-wide first-tier switch for this Conn
// only. It is an alternative SPELLING of OTEL_INSTRUMENTATION_GO_TRACING_ENABLED,
// not an override of it: supplying both is a configuration error reported by the
// constructor (ErrTracingConfigConflict), even when the two agree.
//
// It is one tier of three and says nothing about the other two. A Conn carrying
// it — and everything derived from it, such as oteljetstream wrappers — still
// reads OTEL_NATS_TRACING_ENABLED at construction and still resolves the relay
// verdict on EVERY operation, so it still stops when the relay revokes. There
// is no way to opt a connection out of a revocation.
//
// It exists for callers who cannot set a process environment variable: tests,
// or several independently configured connections in one binary. With the
// environment variable unset, the option is the only source of the first tier,
// so both a tracing and a non-tracing Conn can be built in one process.
func WithTracingEnabled(v bool) Option {
	return optionFunc(func(c *connConfig) {
		c.TracingEnabled = &v
	})
}

// Version returns the instrumentation module version for tracer creation (OTel contrib guideline).
func Version() string {
	return instrumentationVersion
}

func newConn(nc *nats.Conn, opts ...Option) (*Conn, error) {
	cfg := newConnConfig(opts...)
	direct := &directConn{nc: nc}

	// Both environment-derived tiers, ANDed and fixed here. When they say no,
	// only the passthrough is built: no OTel code path is reachable and the
	// resolver is never consulted. Including the module env var is safe because
	// the relay can only revoke — with it off, no relay value could raise the
	// answer, so the instrumented implementation could never be reached.
	possible, err := tracedPossible(cfg.TracingEnabled)
	if err != nil {
		return nil, err
	}
	if !possible {
		return &Conn{nc: nc, direct: direct}, nil
	}

	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	tp := cfg.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	tracer := tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()), trace.WithSchemaURL(semconv.SchemaURL))
	traced := &tracedConn{
		nc:          nc,
		tracer:      tracer,
		propagator:  cfg.Propagators,
		serverAttrs: serverAttrsFromConn(nc),
		traceDest:   cfg.TraceDest,
	}

	// Both implementations exist; the relay verdict selects between them per
	// operation. The option supplied gate1 and does not pin anything.
	return &Conn{nc: nc, direct: direct, traced: traced}, nil
}

// serverAttrsFromConn parses the connected NATS server address into server.address / server.port attributes.
// The default port 4222 is omitted (consistent with otel-mongo omitting 27017).
func serverAttrsFromConn(nc *nats.Conn) []attribute.KeyValue {
	addr := nc.ConnectedAddr()
	if addr == "" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		slog.Debug("otelnats: server address parse failed, using raw addr", "addr", addr, "error", err)
		return []attribute.KeyValue{attribute.String("server.address", addr)}
	}
	attrs := []attribute.KeyValue{attribute.String("server.address", host)}
	if port, err := strconv.Atoi(portStr); err == nil && port > 0 && port != 4222 {
		attrs = append(attrs, attribute.Int("server.port", port))
	}
	return attrs
}

// TracingEnabled reports whether tracing and trace propagation are enabled.
func (c *Conn) TracingEnabled() bool { return c.impl().TracingEnabled() }

// TraceDest returns the configured Nats-Trace-Dest subject (empty if disabled).
func (c *Conn) TraceDest() string { return c.impl().TraceDest() }

// ServerAttrs returns the pre-built server.address / server.port attributes for this connection.
func (c *Conn) ServerAttrs() []attribute.KeyValue { return c.impl().ServerAttrs() }

// TraceContext returns the tracer and propagator used by this Conn. Used by oteljetstream.
func (c *Conn) TraceContext() (trace.Tracer, propagation.TextMapPropagator) {
	return c.impl().TraceContext()
}

// NatsConn returns the underlying *nats.Conn (same as nats package).
func (c *Conn) NatsConn() *nats.Conn {
	return c.nc
}

// Close closes the connection (same as nats.Conn.Close).
func (c *Conn) Close() {
	c.nc.Close()
}

// Drain flushes and closes (same as nats.Conn.Drain).
func (c *Conn) Drain() error {
	return c.nc.Drain()
}

// Publish publishes data to subject. Same as nats.Conn.Publish but accepts context for trace.
func (c *Conn) Publish(ctx context.Context, subject string, data []byte) error {
	return c.impl().Publish(ctx, subject, data)
}

// PublishMsg publishes the message. Same as nats.Conn.PublishMsg but accepts context for trace.
// Per OTel messaging semconv: "Send" span with creation context injected into message; consumer
// spans link to this context. Span name is "{operation.name} {destination}".
func (c *Conn) PublishMsg(ctx context.Context, msg *nats.Msg) error {
	return c.impl().PublishMsg(ctx, msg)
}

// Request sends a request and waits for reply. Signature mirrors nats.Conn.Request exactly.
// When tracing is enabled the producer span uses context.Background() as parent — callers that
// need to chain into an existing trace should use RequestWithContext or RequestMsgWithContext.
func (c *Conn) Request(subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	return c.impl().Request(subject, data, timeout)
}

// RequestWithContext sends a request with the timeout controlled by ctx. Signature mirrors
// nats.Conn.RequestWithContext exactly; the producer span uses ctx as parent for trace chaining.
func (c *Conn) RequestWithContext(ctx context.Context, subject string, data []byte) (*nats.Msg, error) {
	return c.impl().RequestWithContext(ctx, subject, data)
}

// RequestMsg sends a pre-built request message. Signature mirrors nats.Conn.RequestMsg exactly.
// When tracing is enabled the producer span uses context.Background() as parent.
func (c *Conn) RequestMsg(msg *nats.Msg, timeout time.Duration) (*nats.Msg, error) {
	return c.impl().RequestMsg(msg, timeout)
}

// RequestMsgWithContext sends a pre-built request message with ctx-controlled timeout.
// Signature mirrors nats.Conn.RequestMsgWithContext exactly; the producer span uses ctx as parent.
func (c *Conn) RequestMsgWithContext(ctx context.Context, msg *nats.Msg) (*nats.Msg, error) {
	return c.impl().RequestMsgWithContext(ctx, msg)
}

// Subscribe subscribes to subject. Handler receives Msg (m.Msg, m.Context()).
func (c *Conn) Subscribe(subject string, handler MsgHandler) (*nats.Subscription, error) {
	return c.nc.Subscribe(subject, c.msgHandler(subject, "", handler))
}

// QueueSubscribe is the queue-group variant. Handler receives Msg.
func (c *Conn) QueueSubscribe(subject, queue string, handler MsgHandler) (*nats.Subscription, error) {
	return c.nc.QueueSubscribe(subject, queue, c.msgHandler(subject, queue, handler))
}

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
	if msg.Reply != "" {
		attrs = append(attrs, semconv.MessagingMessageConversationID(msg.Reply))
	}
	attrs = append(attrs, serverAttrs...)
	return attrs
}

// requestAttrs builds attributes for the CLIENT span of a request/reply RPC.
// Mirrors publishAttrs but with messaging.operation.name=request so backends
// distinguish blocking RPC from fire-and-forget publish on the same destination.
func requestAttrs(msg *nats.Msg, serverAttrs []attribute.KeyValue) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.MessagingSystemKey.String(messagingSystem),
		semconv.MessagingDestinationNameKey.String(msg.Subject),
		attribute.String(string(semconv.MessagingOperationTypeKey), "send"),
		semconv.MessagingOperationNameKey.String("request"),
	}
	if len(msg.Data) > 0 {
		attrs = append(attrs, semconv.MessagingMessageBodySize(len(msg.Data)))
	}
	if msg.Reply != "" {
		attrs = append(attrs, semconv.MessagingMessageConversationID(msg.Reply))
	}
	attrs = append(attrs, serverAttrs...)
	return attrs
}

// receiveAttrs builds consumer span attributes. opType is "process" (push) or "receive" (pull).
// Note: oteljetstream/consumer.go has parallel receiveBaseAttrs/receiveMsgAttrs for jetstream.Msg — keep
// the attribute sets in sync, EXCEPT conversation_id: a JetStream message's Reply field is the native
// $JS.ACK.<stream>.<consumer>.… acknowledgement subject, not a conversation identifier, so the JetStream
// builders must never map it to messaging.message.conversation_id.
func receiveAttrs(msg *nats.Msg, queue string, opType string, serverAttrs []attribute.KeyValue) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.MessagingSystemKey.String(messagingSystem),
		semconv.MessagingDestinationNameKey.String(msg.Subject),
		attribute.String(string(semconv.MessagingOperationTypeKey), opType),
		semconv.MessagingOperationNameKey.String(opType),
	}
	if len(msg.Data) > 0 {
		attrs = append(attrs, semconv.MessagingMessageBodySize(len(msg.Data)))
	}
	if msg.Reply != "" {
		attrs = append(attrs, semconv.MessagingMessageConversationID(msg.Reply))
	}
	if queue != "" {
		attrs = append(attrs, semconv.MessagingConsumerGroupName(queue))
	}
	attrs = append(attrs, serverAttrs...)
	return attrs
}
