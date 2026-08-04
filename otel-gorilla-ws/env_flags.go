package otelgorillaws

import (
	"errors"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// envWSTracingEnabled is this module's own switch. The process-wide master
// switch is not named here: it belongs to otel-flags, which owns every
// process-scoped name.
const envWSTracingEnabled = "OTEL_GORILLA_WS_TRACING_ENABLED"

// flagKeyWSTracing is the OpenFeature key an operator flips on the relay proxy
// to turn this module's spans on or off without restarting the application.
//
// The relay is authoritative in BOTH directions. What it cannot do here is
// retrofit the wire: otel-ws negotiation happens during the handshake and cannot
// be revisited, so enabling this key reaches connections opened afterwards, not
// live ones. Disabling it reaches every connection immediately for spans and
// inject/extract, but the envelope keeps being written because the peer parses
// every frame as one.
const flagKeyWSTracing = "otel-gorilla-ws-tracing"

// defaultWSTracing is the bottom rung of the ladder. It is false so a process
// that configures nothing traces nothing and negotiates nothing.
const defaultWSTracing = false

// Index into wsResolver's flag keys.
const idxTracing = 0

// wsResolver resolves this module's relay value through the process-global
// OpenFeature client. It caches nothing, so a relay change reaches a live
// connection on its very next operation.
var wsResolver = otelflags.NewResolver(
	otelflags.WithFlagKeys(flagKeyWSTracing),
)

// gateState carries everything about a connection's switches that is fixed at
// construction, so no hot path reads an environment variable.
type gateState struct {
	// relayPossible was resolved once, at construction. False means the relay is
	// structurally incapable of returning anything but the value passed to it,
	// so this connection resolves statically and never touches the OpenFeature
	// SDK.
	relayPossible bool

	// masterLocal and tracingLocal are the env > option > default answers.
	masterLocal  bool
	tracingLocal bool
}

// resolveGates resolves every static tier for one connection, collecting BOTH
// configuration errors before returning either.
func resolveGates(cfg connOptions) (gateState, error) {
	masterLocal, masterErr := otelflags.MasterLocal()
	tracingLocal, tracingErr := otelflags.ResolveLocal(cfg.featureEnabled, envWSTracingEnabled, defaultWSTracing)

	if err := errors.Join(masterErr, tracingErr); err != nil {
		return gateState{}, err
	}

	return gateState{
		relayPossible: otelflags.RelayPossible(),
		masterLocal:   masterLocal,
		tracingLocal:  tracingLocal,
	}, nil
}

// tracedPossible reports whether any OTel SDK path could ever run on this
// connection — whether to build a real tracer rather than a noop one, and
// whether the resolver will ever be consulted.
//
// It is `relayPossible || (masterLocal && tracingLocal)` rather than the static
// answer alone, because the relay can now ENABLE. When no relay can exist the
// static answer is final and a switched-off connection keeps the pre-dynamic
// zero-cost path exactly.
func (g gateState) tracedPossible() bool {
	return g.relayPossible || (g.masterLocal && g.tracingLocal)
}

// tracing resolves this connection's effective tracing state.
//
// Called per WriteMessage/ReadMessage for the span gate, and exactly once —
// immediately before the handshake — to decide otel-ws negotiation. Those are
// the same expression on purpose: a connection negotiates when it is tracing at
// that moment, and a later change moves the spans but cannot move the wire.
func (g gateState) tracing() bool {
	if !g.relayPossible {
		return g.masterLocal && g.tracingLocal
	}
	return otelflags.MasterEnabled(g.masterLocal) &&
		wsResolver.Value(idxTracing, g.tracingLocal)
}
