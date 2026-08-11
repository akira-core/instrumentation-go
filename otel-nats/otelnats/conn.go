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
	instrumentationVersion = "0.9.0"
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
// supplies one rung of the ladder and says nothing about the relay or the
// master switch.
//
// What construction does fix is whether an instrumented implementation exists at
// all. traced is nil only when no relay can exist in this process AND the local
// answer is off; then no OTel SDK code path is reachable for the connection's
// lifetime and no evaluation is ever performed.
type Conn struct {
	nc *nats.Conn

	// gate holds everything about this connection's switches that was fixed at
	// construction. Everything derived from the Conn copies it, so a derived
	// wrapper inherits the decision rather than re-resolving it.
	gate gateState

	// direct is always non-nil. traced is non-nil only when gate.tracedPossible
	// said an instrumented path could ever be reached, and is then selected per
	// operation.
	direct connImpl
	traced connImpl
}

// impl returns the implementation this operation runs through.
// The condition must stay lockstep with msgHandler and traceEventMsgHandler
// (traced == nil? / gate.tracing()?) — do not change one alone.
func (c *Conn) impl() connImpl {
	if c.traced != nil && c.gate.tracing() {
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
		if c.gate.tracing() {
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
		if c.gate.tracing() {
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

// WithTracingEnabled supplies this module's tracing switch for this Conn only.
//
// It is the THIRD rung of a four-rung ladder — relay > env > option > default —
// so it is a per-connection default, not an override of anything above it:
//
//   - OTEL_NATS_TRACING_ENABLED wins over it. That is deliberate, and it is what
//     lets a deployment disable this module without silencing the process and
//     without a relay, even when the application's Go code asked for tracing.
//   - The relay wins over both, in either direction.
//   - The master switch (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED, or its relay
//     key) is ANDed above the whole ladder and accepts no option at all. It
//     stops a Conn carrying WithTracingEnabled(true) like any other.
//
// Supplying it alongside OTEL_NATS_TRACING_ENABLED is legal and no longer an
// error; the variable simply wins. An unreadable value in that variable is still
// a construction error, because the option does not excuse a variable that
// outranks it.
//
// It exists for callers who cannot set a process environment variable: tests, or
// several independently configured connections in one binary. With the variable
// unset the option is the deciding rung, so a tracing and a non-tracing Conn can
// be built in the same process.
//
// It does not make a Conn static. A Conn carrying it — and everything derived
// from it, such as oteljetstream wrappers — resolves the master switch and the
// relay on EVERY operation.
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

	// Everything the relay cannot change, fixed here: the master switch's local
	// value, this module's local value, and whether a relay can exist at all.
	gate, err := resolveGates(cfg.TracingEnabled)
	if err != nil {
		return nil, err
	}
	// No relay possible and the local answer is off ⇒ only the passthrough is
	// built, no OTel code path is reachable, and no evaluation is ever performed.
	// When a relay IS possible the instrumented implementation must exist even
	// though the environment says off, because the relay can enable it later and
	// construction happens once.
	if !gate.tracedPossible() {
		return &Conn{nc: nc, gate: gate, direct: direct}, nil
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
		nc:            nc,
		tracer:        tracer,
		propagator:    cfg.Propagators,
		serverAttrs:   serverAttrsFromConn(nc),
		traceDest:     cfg.TraceDest,
		inboxPrefixes: inboxPrefixes(nc),
	}

	// Both implementations exist; the per-operation resolution selects between
	// them. The option supplied one rung of the ladder and pins nothing.
	return &Conn{nc: nc, gate: gate, direct: direct, traced: traced}, nil
}

// inboxPrefixes returns the subject prefixes that identify a request/reply inbox as
// seen from this connection. Resolved once at construction; nats.Conn.Opts is fixed
// for the connection's life.
//
// Two prefixes are recognised, deliberately:
//
//   - this connection's own prefix, nats.CustomInboxPrefix + "." (the dot is appended
//     by the client, and CustomInboxPrefix rejects a trailing one), which names every
//     inbox this process creates; and
//   - the default "_INBOX." always, which names the inboxes of every peer that did not
//     customise it.
//
// Recognising only the local prefix would fail exactly where custom prefixes are used.
// A responder sees the REQUESTER's inbox in msg.Reply, and the requester is the side
// that customises — the reason to customise is that granting "subscribe _INBOX.>" hands
// a client every other client's replies, which is the requester's permission, while a
// responder needs no inbox permission at all (allow_responses covers it). So the common
// shape is a custom-prefix requester talking to a default-prefix peer, in both
// directions.
//
// Residual gap: two peers with DIFFERENT custom prefixes do not recognise each other's
// inboxes. Rare, and a collector-side span rename covers it.
func inboxPrefixes(nc *nats.Conn) []string {
	if p := nc.Opts.InboxPrefix; p != "" && p+"." != nats.InboxPrefix {
		return []string{p + ".", nats.InboxPrefix}
	}
	return []string{nats.InboxPrefix}
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
