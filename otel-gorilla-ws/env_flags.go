package otelgorillaws

import (
	"github.com/akira-core/instrumentation-go/otel-gorilla-ws/internal/flags"
)

const (
	envGlobalTracingEnabled = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
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
	if !flags.EnvEnabled(envGlobalTracingEnabled) {
		return false
	}
	return wsResolver.Enabled(idxTracing)
}

// wsNegotiationPossible reports whether this process may ever need the otel-ws
// wire envelope, and therefore whether Dial should offer and Upgrader.Upgrade
// should confirm the subprotocol.
//
// It reads the global kill switch ONLY, deliberately ignoring the relay value.
// Subprotocol negotiation happens during the handshake and cannot be revisited,
// so gating it on the dynamic flag would leave every connection established
// during an "off" window permanently unable to propagate trace context — and
// WebSocket connections routinely live for hours. The cost of this choice is
// that peers which both run this library with the global switch on exchange the
// JSON envelope even while tracing is dynamically off.
func wsNegotiationPossible() bool {
	return flags.EnvEnabled(envGlobalTracingEnabled)
}
