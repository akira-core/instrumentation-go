package otelgorillaws

import (
	"fmt"
	"os"

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

// WithTracingEnabled supplies the process-wide first-tier switch for this Conn
// only. It is an alternative SPELLING of OTEL_INSTRUMENTATION_GO_TRACING_ENABLED,
// not an override of it: supplying both is a configuration error reported by the
// constructor (ErrTracingConfigConflict), even when the two agree.
//
// It is one tier of three and says nothing about the other two. A connection
// carrying it still reads OTEL_GORILLA_WS_TRACING_ENABLED at construction and
// still resolves the relay verdict on EVERY operation, so it still stops when
// the relay revokes. There is no way to opt a connection out of a revocation.
//
// In Dial and Upgrader.Upgrade it also participates in gating otel-ws
// subprotocol negotiation, which is resolved from the static tiers alone
// (this switch AND the module env var) because a handshake cannot be revisited.
// Excluding the relay costs nothing: the relay can only revoke, so a connection
// whose module env var is off at handshake time could never later need the
// envelope. The reverse does not hold — WithTracingEnabled(true) cannot force
// the envelope onto a peer that did not negotiate otel-ws.
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

// resolveGate1 resolves the process-wide first tier for one connection.
//
// The environment variable and the option are two spellings of one switch, so
// supplying both is rejected on PRESENCE, not on value: EnvSet is required
// because EnvEnabled cannot tell "unset" (the deployment expressed no opinion,
// and the option may supply one) from "set to something falsy".
func resolveGate1(cfg connOptions) (bool, error) {
	if flags.GlobalTracingSet() && cfg.featureEnabled != nil {
		return false, fmt.Errorf("%w: option=%v, %s=%q",
			ErrTracingConfigConflict, *cfg.featureEnabled,
			envGlobalTracingEnabled, os.Getenv(envGlobalTracingEnabled))
	}
	if cfg.featureEnabled != nil {
		return *cfg.featureEnabled, nil
	}
	return flags.GlobalTracingPossible(), nil
}

// effectiveCapability resolves whether a connection may EVER trace: both
// environment-derived tiers, ANDed. It deliberately does not consult the relay.
//
// This is the whole static part of the decision. It answers two questions that
// cannot be revisited after construction — whether to negotiate the otel-ws
// subprotocol during the handshake, and whether to build a real tracer at all —
// so it must not depend on a value that can change a second later. Including
// the module env var is safe precisely because the relay can only revoke: with
// it off, no relay value could ever raise the answer.
func effectiveCapability(cfg connOptions) (bool, error) {
	gate1, err := resolveGate1(cfg)
	if err != nil {
		return false, err
	}
	return gate1 && flags.EnvEnabled(envWSTracingEnabled), nil
}

// configureConn applies cfg to c: propagator, feature override and tracer.
//
// It clamps the WRITE-side envelope decision with capability, so a
// capability-off process never emits an envelope. It deliberately does NOT
// clamp c.enveloped, which records whether the PEER envelopes — a fact settled
// by the handshake that this side's local gate has no power over. Clamping that
// too is what made ReadMessage hand raw {"header":...,"data":...} bytes to the
// application.
func configureConn(c *Conn, cfg connOptions, capable bool) {
	if cfg.propagator != nil {
		c.propagator = cfg.propagator
	} else {
		c.propagator = otel.GetTextMapPropagator()
	}

	c.capable = capable
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
