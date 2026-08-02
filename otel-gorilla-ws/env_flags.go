package otelgorillaws

import (
	"github.com/akira-core/instrumentation-go/otel-gorilla-ws/internal/flags"
)

const (
	// envGlobalTracingEnabled aliases the shared kill-switch name so the literal
	// has exactly one home (internal/flags) and cannot drift from it.
	envGlobalTracingEnabled = flags.EnvGlobalTracing
	envWSTracingEnabled     = "OTEL_GORILLA_WS_TRACING_ENABLED"
)

// flagKeyWSTracing is the OpenFeature key an operator flips on the relay proxy
// to turn this module's span creation on or off without restarting the
// application.
const flagKeyWSTracing = "otel-gorilla-ws-tracing"

// Index into wsResolver's specs.
const idxTracing = 0

// wsResolver resolves this module's dynamic flags through the process-global
// OpenFeature client, with OTEL_GORILLA_WS_TRACING_ENABLED as the evaluation
// default — so an application that never installs a provider behaves exactly as
// it did before dynamic flags existed.
var wsResolver = newWSResolver()

func newWSResolver() *flags.Resolver {
	return flags.NewResolver("otel-gorilla-ws",
		flags.WithSpecs(
			flags.Spec{Key: flagKeyWSTracing, EnvVar: envWSTracingEnabled},
		),
	)
}

// wsTracingEnabled reports the module's effective span-creation state for a
// connection that carries no WithTracingEnabled override. It may return a
// different answer than it did a second ago, so callers MUST read it per
// operation rather than caching it on the Conn.
func wsTracingEnabled() bool {
	if !flags.GlobalTracingPossible() {
		return false
	}
	return wsResolver.Enabled(idxTracing)
}
