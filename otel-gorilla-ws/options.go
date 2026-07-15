package otelgorillaws

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
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

// WithTracingEnabled overrides the env-gate default (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED
// AND OTEL_GORILLA_WS_TRACING_ENABLED) for this Conn only, in either
// direction. When unset, tracing follows the env gates exactly as before.
// When set, this value is authoritative — it controls featureEnabled, the
// flag gating whether any OTel SDK code path runs at all (span creation,
// propagator inject/extract via the wire envelope).
//
// In Dial and Upgrader.Upgrade the effective feature flag also gates otel-ws
// subprotocol negotiation: a connection whose effective tracing is off never
// offers (Dial) or confirms (Upgrade) otel-ws, so the peer is never committed
// to the JSON envelope wire format that this side would not unwrap. The
// reverse does not hold — WithTracingEnabled(true) cannot force the envelope
// onto a connection whose peer did not negotiate otel-ws; negotiation outcome
// (Conn.tracingEnabled) still requires both sides to agree.
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

// effectiveFeatureEnabled resolves the feature flag for a connection: the
// WithTracingEnabled override when present, otherwise the env gates.
func effectiveFeatureEnabled(cfg connOptions) bool {
	if cfg.featureEnabled != nil {
		return *cfg.featureEnabled
	}
	return wsTracingEnabled()
}

// configureConn applies cfg to c: propagator, featureEnabled and tracer.
func configureConn(c *Conn, cfg connOptions) {
	if cfg.propagator != nil {
		c.propagator = cfg.propagator
	} else {
		c.propagator = otel.GetTextMapPropagator()
	}

	c.featureEnabled = effectiveFeatureEnabled(cfg)

	if !c.featureEnabled {
		// Feature flag off ⇒ no OTel SDK call on caller's TracerProvider; use noop tracer.
		c.tracer = noop.NewTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
		return
	}

	tp := cfg.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	c.tracer = tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()), trace.WithSchemaURL(semconv.SchemaURL))
}
