package otelgorillaws

import (
	"errors"

	"github.com/akira-core/instrumentation-go/otel-gorilla-ws/internal/flags"
)

const (
	// envGlobalTracingEnabled aliases the shared kill-switch name so the literal
	// has exactly one home (internal/flags) and cannot drift from it.
	envGlobalTracingEnabled = flags.EnvGlobalTracing
	envWSTracingEnabled     = "OTEL_GORILLA_WS_TRACING_ENABLED"
)

// flagKeyWSTracing is the OpenFeature key an operator flips on the relay proxy
// to turn this module's span creation off without restarting the application.
//
// The relay can only REVOKE. The key is resolved with an evaluation default of
// true and ANDed with envWSTracingEnabled, so a module the deployment left off
// can never be switched on remotely.
const flagKeyWSTracing = "otel-gorilla-ws-tracing"

// Index into wsResolver's flag keys.
const idxTracing = 0

// ErrTracingConfigConflict reports that both OTEL_INSTRUMENTATION_GO_TRACING_ENABLED
// and WithTracingEnabled were supplied. They are two spellings of one switch, so
// setting both is an error even when they agree: the rule to remember is "set
// one", which is checkable at a glance, rather than "make them match", which
// depends on the truthiness allow-list.
var ErrTracingConfigConflict = errors.New(
	"otelgorillaws: WithTracingEnabled and " + envGlobalTracingEnabled +
		" are mutually exclusive; set exactly one")

// wsResolver resolves this module's relay verdict through the process-global
// OpenFeature client. It caches nothing, so a revocation reaches a live
// connection on its very next operation.
var wsResolver = flags.NewResolver(
	flags.WithFlagKeys(flagKeyWSTracing),
)

// wsRelayAllows reports the relay verdict alone.
//
// It is only half the answer: callers MUST AND it with the connection's static
// capability (gate1 && envWSTracingEnabled, fixed at construction), which is
// what Conn.featureEnabled does. Reading it on its own would let the relay
// enable a module the deployment left off.
func wsRelayAllows() bool {
	return wsResolver.Allowed(idxTracing)
}
