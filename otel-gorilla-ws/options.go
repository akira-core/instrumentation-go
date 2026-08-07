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

// WithTracingEnabled supplies this module's tracing switch for this connection
// only.
//
// It is the THIRD rung of a four-rung ladder — relay > env > option > default —
// so it is a per-connection default, not an override of anything above it:
//
//   - OTEL_GORILLA_WS_TRACING_ENABLED wins over it, so a deployment can disable
//     this module even where the application's Go code asked for tracing.
//   - The relay wins over both, in either direction.
//   - The master switch (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED, or its relay
//     key) is ANDed above the whole ladder and accepts no option at all.
//
// Supplying it alongside OTEL_GORILLA_WS_TRACING_ENABLED is legal; the variable
// wins. An unreadable value in that variable is still a construction error.
//
// In Dial and Upgrader.Upgrade it also participates in gating otel-ws
// subprotocol negotiation, which is resolved ONCE immediately before the
// handshake from the whole ladder, relay included. Two consequences worth
// knowing, because they are not symmetric:
//
//   - Enabling reaches only connections opened afterwards. A connection made
//     while this module was off never gains the envelope, and this option
//     cannot restore it — a peer that did not negotiate otel-ws will not parse
//     one. Such a connection can still emit local spans; it just cannot inject
//     or extract.
//   - Disabling reaches every connection immediately for spans and
//     inject/extract, but not for the envelope, which the peer is still
//     parsing. Removing that wire cost requires cycling the connection.
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

// configureConn applies cfg to c: propagator, gate state and tracer.
//
// It clamps the WRITE-side envelope decision with capability, so a process where
// no OTel path can ever run never emits an envelope. It deliberately does NOT
// clamp c.enveloped, which records whether the PEER envelopes — a fact settled
// by the handshake that this side's local gate has no power over. Clamping that
// too is what made ReadMessage hand raw {"header":...,"data":...} bytes to the
// application.
func configureConn(c *Conn, cfg connOptions, gate gateState) {
	if cfg.propagator != nil {
		c.propagator = cfg.propagator
	} else {
		c.propagator = otel.GetTextMapPropagator()
	}

	c.gate = gate
	c.capable = gate.tracedPossible()
	c.tracingEnabled = c.enveloped && c.capable

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
