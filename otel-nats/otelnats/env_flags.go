package otelnats

import (
	"errors"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// envNATSTracingEnabled is this module's own switch. The process-wide master
// switch is not named here: it belongs to otel-flags, which owns every
// process-scoped name.
const envNATSTracingEnabled = "OTEL_NATS_TRACING_ENABLED"

// flagKeyNATSTracing is the OpenFeature key an operator flips on the relay proxy
// to turn this module's tracing on or off without restarting the application.
//
// The relay is authoritative in BOTH directions: it can disable tracing the
// deployment enabled and enable tracing the deployment left off. What bounds it
// is the master switch above (otel-flags), which no relay value for this key can
// escape, and the fact that a process with no relay configured never asks.
const flagKeyNATSTracing = "otel-nats-tracing"

// defaultNATSTracing is the bottom rung of the ladder. It is false so a process
// that configures nothing traces nothing.
const defaultNATSTracing = false

// natsResolver resolves this module's relay value through the process-global
// OpenFeature client. It caches nothing, so a relay change reaches a live
// connection on its very next operation.
var natsResolver = otelflags.NewResolver()

// gateState carries everything about a connection's switches that is fixed at
// construction, so no hot path reads an environment variable.
//
// It is copied by value into everything derived from a Conn — oteljetstream
// wrappers, consumers, iterators, batch forwarders — so a derived wrapper
// inherits the connection's decision rather than re-resolving it.
type gateState struct {
	// relayPossible was resolved once, at construction. False means the relay is
	// structurally incapable of returning anything but the value passed to it,
	// so this connection resolves statically and never touches the OpenFeature
	// SDK.
	relayPossible bool

	// masterLocal and tracingLocal are the env > option > default answers. They
	// are the evaluation defaults handed to the relay, not a separate tier.
	masterLocal  bool
	tracingLocal bool
}

// resolveGates resolves every static tier for one connection, collecting BOTH
// configuration errors before returning either.
//
// One configuration file can carry every OTEL_* variable, and each is read
// independently, so returning the first failure alone would make the caller fix
// one and rediscover the next on the following run.
//
// otelflags.ValidateAndInstall is part of the same read: it validates the
// process-scoped provider variables this module never names, and performs the
// one-time provider install. Doing it here rather than inside the first
// evaluation keeps a provider initialisation off the hot path of an instrumented
// operation.
func resolveGates(tracingOption *bool) (gateState, error) {
	providerErr := otelflags.ValidateAndInstall()
	masterLocal, masterErr := otelflags.MasterLocal()
	tracingLocal, tracingErr := otelflags.ResolveLocal(tracingOption, envNATSTracingEnabled, defaultNATSTracing)

	if err := errors.Join(providerErr, masterErr, tracingErr); err != nil {
		return gateState{}, err
	}

	return gateState{
		relayPossible: otelflags.RelayPossible(),
		masterLocal:   masterLocal,
		tracingLocal:  tracingLocal,
	}, nil
}

// tracedPossible reports whether the instrumented implementation must be built
// at all.
//
// It is `relayPossible || (masterLocal && tracingLocal)` rather than the static
// answer alone, because the relay can now ENABLE: a connection whose environment
// says off must still be able to start tracing when the relay says so, and
// construction happens once. When no relay can exist, the static answer is
// final and a switched-off connection allocates nothing instrumented — the
// pre-dynamic zero-cost path, preserved exactly.
func (g gateState) tracedPossible() bool {
	return g.relayPossible || (g.masterLocal && g.tracingLocal)
}

// tracing resolves this connection's effective tracing state for one operation.
//
// With no relay possible it is pure arithmetic on two booleans fixed at
// construction — no evaluation, no allocation. Otherwise it is two Boolean
// calls, master first so a veto short-circuits the module's own key.
func (g gateState) tracing() bool {
	if !g.relayPossible {
		return g.masterLocal && g.tracingLocal
	}
	return otelflags.MasterEnabled(g.masterLocal) &&
		natsResolver.Value(flagKeyNATSTracing, g.tracingLocal)
}
