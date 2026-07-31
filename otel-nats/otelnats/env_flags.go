package otelnats

import (
	"github.com/akira-core/instrumentation-go/otel-nats/otelnats/internal/flags"
)

const (
	envGlobalTracingEnabled = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envNATSTracingEnabled   = "OTEL_NATS_TRACING_ENABLED"
)

// flagKeyNATSTracing is the OpenFeature key an operator flips on the relay proxy
// to turn this module's tracing on or off without restarting the application.
const flagKeyNATSTracing = "otel-nats-tracing"

// Index into natsResolver's specs.
const idxTracing = 0

// natsResolver resolves this module's dynamic flags through the process-global
// OpenFeature client, with OTEL_NATS_TRACING_ENABLED as the evaluation default —
// so an application that never installs a provider behaves exactly as it did
// before dynamic flags existed.
//
// The global switch is deliberately NOT a Spec: it is an out-of-band kill switch
// with no relay counterpart, ANDed ahead of the resolver so that no OpenFeature
// code path runs at all while it is off.
var natsResolver = newNATSResolver()

func newNATSResolver() *flags.Resolver {
	return flags.NewResolver("otel-nats",
		flags.WithSpecs(
			flags.Spec{Key: flagKeyNATSTracing, EnvVar: envNATSTracingEnabled},
		),
	)
}

// natsTracingEnabled reports the module's effective tracing state for a
// connection that carries no WithTracingEnabled override.
//
// Unlike the cached gate it replaces, this may return a different answer than it
// did a second ago: callers MUST read it per operation rather than caching it on
// a wrapper struct.
func natsTracingEnabled() bool {
	if !flags.EnvEnabled(envGlobalTracingEnabled) {
		return false
	}
	return natsResolver.Enabled(idxTracing)
}

// dynamicTracingPossible reports whether this process may ever trace NATS —
// i.e. whether the instrumented implementation must be constructed at all.
// It reads the global kill switch only, never the relay, because the choice of
// which implementations to build is necessarily static.
func dynamicTracingPossible() bool {
	return flags.EnvEnabled(envGlobalTracingEnabled)
}
