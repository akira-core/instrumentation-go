package otelmongo

import (
	"errors"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// This module's own switches. The process-wide master switch is not named here:
// it belongs to otel-flags, which owns every process-scoped name.
const (
	envMongoTracingEnabled     = "OTEL_MONGO_TRACING_ENABLED"
	envMongoPropagationEnabled = "OTEL_MONGO_PROPAGATION_ENABLED"
)

// OpenFeature keys an operator flips on the relay proxy to turn this module's
// behavior on or off without restarting the application. v1 and v2 share both
// keys, as they share both env vars, so a change to otel-mongo-tracing reaches
// both.
//
// The relay is authoritative in BOTH directions, which matters most for
// propagation: _oteltrace is roughly 90 bytes written into the application's own
// documents, never stripped on read, and removable only by a $unset migration.
// Three things bound that — the master switch above, the propagation tier's
// hardcoded default of false, and the fact that a process with no relay
// configured never asks.
const (
	flagKeyMongoTracing     = "otel-mongo-tracing"
	flagKeyMongoPropagation = "otel-mongo-propagation"
)

// Hardcoded defaults — the bottom rung of the ladder. Both false, so a process
// that configures nothing traces nothing and writes nothing.
//
// defaultMongoPropagation is the one default in this repository that protects
// stored data rather than telemetry volume: nothing may write _oteltrace unless
// an option, an environment variable or a deliberately created relay flag says
// so. Absence in every source can never enable it.
const (
	defaultMongoTracing     = false
	defaultMongoPropagation = false
)

// Indices into mongoResolver's flag keys.
const (
	idxTracing = iota
	idxPropagation
)

// mongoResolver resolves this module's relay values through the process-global
// OpenFeature client. It caches nothing, so a relay change reaches a live client
// on its very next operation.
var mongoResolver = otelflags.NewResolver(
	otelflags.WithFlagKeys(
		flagKeyMongoTracing,
		flagKeyMongoPropagation,
	),
)

// resolveGates resolves every static tier for one client, collecting ALL
// configuration errors before returning any of them.
//
// A deployment can carry more than one unreadable value — one configuration file
// setting every OTEL_*_ENABLED variable — so all three reads run and the
// failures are joined in a fixed order (master, tracing, propagation). Returning
// only the first would make the caller fix one and rediscover the next on the
// following run, which is the failure mode configuration errors are worst at.
func resolveGates(tracingOption, propagationOption *bool) (gateState, error) {
	masterLocal, masterErr := otelflags.MasterLocal()
	tracingLocal, tracingErr := otelflags.ResolveLocal(tracingOption, envMongoTracingEnabled, defaultMongoTracing)
	propLocal, propErr := otelflags.ResolveLocal(propagationOption, envMongoPropagationEnabled, defaultMongoPropagation)

	if err := errors.Join(masterErr, tracingErr, propErr); err != nil {
		return gateState{}, err
	}

	return gateState{
		relayPossible: otelflags.RelayPossible(),
		masterLocal:   masterLocal,
		tracingLocal:  tracingLocal,
		propLocal:     propLocal,
	}, nil
}

// envGates resolves the static tiers from the environment alone, for the
// constructors that accept no options.
//
// It can still fail — an unreadable environment value does not need an option to
// conflict with — but its callers have no error return, so it falls back to a
// fully-disabled gate. That is the safe direction and it is not silent: every
// option-accepting constructor in this module reports the same value properly,
// so a deployment carrying one learns about it at its first Connect.
func envGates() gateState {
	g, err := resolveGates(nil, nil)
	if err != nil {
		return gateState{}
	}
	return g
}
