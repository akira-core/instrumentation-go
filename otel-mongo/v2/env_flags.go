package otelmongo

import (
	"errors"
	"fmt"
	"os"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/flags"
)

const (
	// envGlobalTracingEnabled aliases the shared kill-switch name so the literal
	// has exactly one home (internal/flags) and cannot drift from it.
	envGlobalTracingEnabled    = flags.EnvGlobalTracing
	envMongoTracingEnabled     = "OTEL_MONGO_TRACING_ENABLED"
	envMongoPropagationEnabled = "OTEL_MONGO_PROPAGATION_ENABLED"
)

// OpenFeature keys an operator flips on the relay proxy to turn this module's
// behavior off without restarting the application. v1 and v2 share both keys, as
// they share both env vars, so revoking otel-mongo-tracing stops both.
//
// The relay can only REVOKE. Both keys resolve with an evaluation default of
// true and are ANDed with their paired env var, so nothing on the relay can
// enable what the deployment left off. That matters most for propagation:
// _oteltrace is roughly 90 bytes written into the application's own documents,
// never stripped on read, and removable only by a $unset migration.
const (
	flagKeyMongoTracing     = "otel-mongo-tracing"
	flagKeyMongoPropagation = "otel-mongo-propagation"
)

// Indices into mongoResolver's flag keys.
const (
	idxTracing = iota
	idxPropagation
)

// Configuration-conflict sentinels. The env var and the option are two spellings
// of one switch, so supplying both is an error even when they agree: the rule to
// remember is "set one", which is checkable at a glance, rather than "make them
// match", which depends on the truthiness allow-list.
var (
	ErrTracingConfigConflict = errors.New(
		"otelmongo/v2: WithTracingEnabled and " + envGlobalTracingEnabled +
			" are mutually exclusive; set exactly one")

	ErrTracePropagationConfigConflict = errors.New(
		"otelmongo/v2: WithTracePropagationEnabled and " + envMongoPropagationEnabled +
			" are mutually exclusive; set exactly one")
)

// mongoResolver resolves this module's relay verdicts through the process-global
// OpenFeature client. It caches nothing, so a revocation reaches a live client on
// its very next operation.
//
// The global switch is deliberately NOT a flag key: it is an out-of-band kill
// switch with no relay counterpart, ANDed ahead of the resolver so that no
// OpenFeature code path runs at all while it is off.
var mongoResolver = flags.NewResolver(
	flags.WithFlagKeys(
		flagKeyMongoTracing,     // paired with envMongoTracingEnabled, ANDed by this package
		flagKeyMongoPropagation, // paired with envMongoPropagationEnabled, ANDed by this package
	),
)

// mongoRelayAllowsTracing and mongoRelayAllowsPropagation report the relay
// verdicts alone.
//
// Each is only part of the answer: callers MUST AND them with the client's
// static tiers, which is what gateState does. Reading either on its own would
// let the relay enable what the deployment left off.
func mongoRelayAllowsTracing() bool     { return mongoResolver.Allowed(idxTracing) }
func mongoRelayAllowsPropagation() bool { return mongoResolver.Allowed(idxPropagation) }

// resolveGate1 resolves the process-wide first tier for one client.
//
// Rejection is on PRESENCE, not on value: EnvSet is required because EnvEnabled
// cannot tell "unset" (the deployment expressed no opinion, and the option may
// supply one) from "set to something falsy".
func resolveGate1(override *bool) (bool, error) {
	if flags.GlobalTracingSet() && override != nil {
		return false, fmt.Errorf("%w: option=%v, %s=%q",
			ErrTracingConfigConflict, *override,
			envGlobalTracingEnabled, os.Getenv(envGlobalTracingEnabled))
	}
	if override != nil {
		return *override, nil
	}
	return flags.GlobalTracingPossible(), nil
}

// resolvePropagationTier resolves the _oteltrace propagation tier for one client.
// Same presence rule as resolveGate1, with its own sentinel.
func resolvePropagationTier(override *bool) (bool, error) {
	if flags.EnvSet(envMongoPropagationEnabled) && override != nil {
		return false, fmt.Errorf("%w: option=%v, %s=%q",
			ErrTracePropagationConfigConflict, *override,
			envMongoPropagationEnabled, os.Getenv(envMongoPropagationEnabled))
	}
	if override != nil {
		return *override, nil
	}
	return flags.EnvEnabled(envMongoPropagationEnabled), nil
}

// resolveGates resolves every static tier for one client, collecting ALL
// configuration conflicts before returning any of them.
//
// A caller can violate both rules at once — one configuration file setting every
// environment variable, one code path passing every option — so both checks run
// and the failures are joined in a fixed order (tracing first, propagation
// second). Returning only the first would make the caller fix one conflict and
// rediscover the other on the next run, which is the failure mode configuration
// errors are worst at.
func resolveGates(tracingOverride, propagationOverride *bool) (gateState, error) {
	gate1, tracingErr := resolveGate1(tracingOverride)
	gateProp, propErr := resolvePropagationTier(propagationOverride)

	if err := errors.Join(tracingErr, propErr); err != nil {
		return gateState{}, err
	}

	return gateState{
		// Both terms are environment-derived and fixed here, so they also decide
		// which implementations are allocated at all. Including the module env
		// var is safe because the relay can only revoke: with it off, no relay
		// value could raise the answer.
		tracedBuilt: gate1 && flags.EnvEnabled(envMongoTracingEnabled),
		propagation: gateProp,
	}, nil
}

// envGates resolves the static tiers from the environment alone, for the
// constructors that accept no options. It cannot fail: a configuration conflict
// requires an option to conflict with, and there is none here.
func envGates() gateState {
	g, _ := resolveGates(nil, nil)
	return g
}
