package otelnats

import (
	"errors"
	"fmt"
	"os"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats/internal/flags"
)

const (
	// envGlobalTracingEnabled aliases the shared kill-switch name so the literal
	// has exactly one home (internal/flags) and cannot drift from it.
	envGlobalTracingEnabled = flags.EnvGlobalTracing
	envNATSTracingEnabled   = "OTEL_NATS_TRACING_ENABLED"
)

// flagKeyNATSTracing is the OpenFeature key an operator flips on the relay proxy
// to turn this module's tracing off without restarting the application.
//
// The relay can only REVOKE. The key is resolved with an evaluation default of
// true and ANDed with envNATSTracingEnabled, so a module the deployment left off
// can never be switched on remotely.
const flagKeyNATSTracing = "otel-nats-tracing"

// Index into natsResolver's flag keys.
const idxTracing = 0

// ErrTracingConfigConflict reports that both OTEL_INSTRUMENTATION_GO_TRACING_ENABLED
// and WithTracingEnabled were supplied. They are two spellings of one switch, so
// setting both is an error even when they agree: the rule to remember is "set
// one", which is checkable at a glance, rather than "make them match", which
// depends on the truthiness allow-list.
var ErrTracingConfigConflict = errors.New(
	"otelnats: WithTracingEnabled and " + envGlobalTracingEnabled +
		" are mutually exclusive; set exactly one")

// natsResolver resolves this module's relay verdict through the process-global
// OpenFeature client. It caches nothing, so a revocation reaches a live
// connection on its very next operation.
//
// The global switch is deliberately NOT a flag key: it is an out-of-band kill
// switch with no relay counterpart, ANDed ahead of the resolver so that no
// OpenFeature code path runs at all while it is off.
var natsResolver = flags.NewResolver(
	flags.WithFlagKeys(flagKeyNATSTracing),
)

// natsRelayAllows reports the relay verdict alone.
//
// It is only half the answer: callers MUST AND it with the connection's static
// capability (gate1 && envNATSTracingEnabled, fixed at construction), which is
// what Conn.tracingOn does. Reading it on its own would let the relay enable a
// module the deployment left off.
func natsRelayAllows() bool {
	return natsResolver.Allowed(idxTracing)
}

// resolveGate1 resolves the process-wide first tier for one connection.
//
// The environment variable and the option are two spellings of one switch, so
// supplying both is rejected on PRESENCE, not on value: EnvSet is required
// because EnvEnabled cannot tell "unset" (the deployment expressed no opinion,
// and the option may supply one) from "set to something falsy".
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

// tracedPossible reports whether a connection may EVER trace: both
// environment-derived tiers, ANDed and fixed at construction. When it is false
// only the passthrough implementation is built, so no OTel SDK code path is
// reachable for that connection's lifetime and the resolver is never consulted.
//
// Including the module env var is safe precisely because the relay can only
// revoke: with it off, no relay value could ever raise the answer.
func tracedPossible(override *bool) (bool, error) {
	gate1, err := resolveGate1(override)
	if err != nil {
		return false, err
	}
	return gate1 && flags.EnvEnabled(envNATSTracingEnabled), nil
}
