package otelgorillaws

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/akira-core/instrumentation-go/otel-gorilla-ws/internal/flags"
)

// Option configures a Conn.
type Option func(*connOptions)

type connOptions struct {
	propagator     propagation.TextMapPropagator
	tracerProvider trace.TracerProvider
	featureEnabled *bool
}

// WithPropagators sets a TextMapPropagator for this connection only.
// If not provided, the global propagator is used.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *connOptions) {
		if p != nil {
			o.propagator = p
		}
	}
}

// WithTracerProvider sets a TracerProvider for this connection only.
// If not provided, the global provider is used.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *connOptions) {
		if tp != nil {
			o.tracerProvider = tp
		}
	}
}

// WithTracingEnabled overrides flag resolution for this Conn only, in either
// direction. When set, this value is authoritative — it takes precedence over
// the global env kill switch, the module env var and any value the relay proxy
// serves, and it controls whether any OTel SDK code path runs at all (span
// creation, propagator inject/extract via the wire envelope).
//
// An overridden Conn is fully STATIC: no OpenFeature evaluation ever runs for
// it, so a later relay change cannot start or stop its tracing. Connections
// constructed WITHOUT this option resolve span creation per operation and do
// follow the relay.
//
// In Dial and Upgrader.Upgrade this option also gates otel-ws subprotocol
// negotiation: a connection constructed with WithTracingEnabled(false) never
// offers (Dial) or confirms (Upgrade) otel-ws, so the peer is never committed
// to the JSON envelope wire format that this side would not unwrap. Without the
// option, negotiation follows the global env kill switch alone
// (flags.GlobalTracingPossible) and NOT the relay value. Handshake cannot be
// revisited, so gating negotiation on the dynamic flag would leave connections
// established while off permanently unable to propagate. Cost: library peers
// with the global switch on exchange the JSON envelope even while tracing is
// dynamically off. The reverse does not hold — WithTracingEnabled(true) cannot
// force the envelope onto a connection whose peer did not negotiate otel-ws;
// the negotiation outcome (Conn.tracingEnabled) still requires both sides to
// agree (or, for NewConn, a proven otel-ws subprotocol on the raw conn).
func WithTracingEnabled(v bool) Option {
	return func(o *connOptions) {
		o.featureEnabled = &v
	}
}

// resolveConnOptions parses opts into a connOptions, skipping nil entries.
func resolveConnOptions(opts []Option) connOptions {
	cfg := connOptions{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&cfg)
	}
	return cfg
}

// effectiveCapability resolves whether a connection may EVER trace: the
// WithTracingEnabled override when present, otherwise the global env kill
// switch. It deliberately does not consult the relay.
//
// This is the static half of the decision. It answers two questions that cannot
// be revisited after construction — whether to negotiate the otel-ws subprotocol
// during the handshake, and whether to build a real tracer at all — so it must
// not depend on a value that can change a second later.
func effectiveCapability(cfg connOptions) bool {
	if cfg.featureEnabled != nil {
		return *cfg.featureEnabled
	}
	return flags.GlobalTracingPossible()
}

// configureConn applies cfg to c: propagator, feature override and tracer.
// It also clamps tracingEnabled so capability-off connections never retain a
// stale negotiation flag (choke-point for the capable ⇒ no envelope invariant).
func configureConn(c *Conn, cfg connOptions) {
	if cfg.propagator != nil {
		c.propagator = cfg.propagator
	} else {
		c.propagator = otel.GetTextMapPropagator()
	}

	c.featureOverride = cfg.featureEnabled
	c.capable = effectiveCapability(cfg)
	c.tracingEnabled = c.tracingEnabled && c.capable

	if !c.capable {
		// This connection can never trace ⇒ no OTel SDK call on the caller's
		// TracerProvider; use a noop tracer.
		c.tracer = noop.NewTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
		return
	}

	tp := cfg.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	c.tracer = tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()), trace.WithSchemaURL(semconv.SchemaURL))
}
