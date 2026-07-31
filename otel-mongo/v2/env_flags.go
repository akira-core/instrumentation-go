package otelmongo

import (
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/flags"
)

const (
	envGlobalTracingEnabled    = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envMongoTracingEnabled     = "OTEL_MONGO_TRACING_ENABLED"
	envMongoPropagationEnabled = "OTEL_MONGO_PROPAGATION_ENABLED"
)

// OpenFeature keys an operator flips on the relay proxy to change this module's
// behavior without restarting the application. v1 and v2 share both keys, as
// they share both env vars.
const (
	flagKeyMongoTracing     = "otel-mongo-tracing"
	flagKeyMongoPropagation = "otel-mongo-propagation"
)

// Indices into mongoResolver's specs. Both flags live in one resolver so they
// always come from the same snapshot instant — a torn read reporting tracing off
// while propagation still read on could never correspond to a relay state.
const (
	idxTracing = iota
	idxPropagation
)

// mongoResolver resolves this module's dynamic flags through the process-global
// OpenFeature client, with the module env vars as the evaluation defaults — so
// an application that never installs a provider behaves exactly as it did
// before dynamic flags existed.
//
// The global switch is deliberately NOT a Spec: it is an out-of-band kill switch
// with no relay counterpart, ANDed ahead of the resolver so that no OpenFeature
// code path runs at all while it is off.
var mongoResolver = newMongoResolver()

func newMongoResolver() *flags.Resolver {
	return flags.NewResolver("otel-mongo",
		flags.WithSpecs(
			flags.Spec{Key: flagKeyMongoTracing, EnvVar: envMongoTracingEnabled},
			flags.Spec{Key: flagKeyMongoPropagation, EnvVar: envMongoPropagationEnabled},
		),
	)
}

// mongoTracingEnabled reports the module's effective tracing state for a Client
// that carries no WithTracingEnabled override.
//
// Unlike the plain env read it replaces, this may return a different answer than
// it did a second ago: callers MUST read it per operation rather than caching it
// on a wrapper struct.
func mongoTracingEnabled() bool {
	if !flags.EnvEnabled(envGlobalTracingEnabled) {
		return false
	}
	return mongoResolver.Enabled(idxTracing)
}

// dynamicTracingPossible reports whether this process may ever trace Mongo —
// i.e. whether the instrumented implementations must be constructed at all.
// It reads the global kill switch only, never the relay, because the choice of
// which implementations to build is necessarily static.
func dynamicTracingPossible() bool {
	return flags.EnvEnabled(envGlobalTracingEnabled)
}

// mongoPropagationEnvOnly reports OTEL_MONGO_PROPAGATION_ENABLED alone, without
// consulting the relay. It is the propagation default for clients pinned static
// by WithTracingEnabled: no OpenFeature evaluation may run for them — and when
// the pin is what enabled tracing past a disabled global kill switch, none may
// run at all, or the relay could reach a process whose kill switch is off.
func mongoPropagationEnvOnly() bool {
	return flags.EnvEnabled(envMongoPropagationEnabled)
}

// mongoPropagationResolved reports the module's propagation flag on its own,
// without the tracing gate. Formerly a plain OTEL_MONGO_PROPAGATION_ENABLED read;
// now the relay decides when it has an opinion and that env var is the fallback.
// Used by resolveDocumentPropagation as the default.
func mongoPropagationResolved() bool {
	return mongoResolver.Enabled(idxPropagation)
}

func mongoPropagationEnabled() bool {
	return resolveDocumentPropagation(mongoTracingEnabled(), nil)
}

// resolveDocumentPropagation returns the effective _oteltrace propagation flag
// for a Client, given that Client's already-resolved effective tracing state
// (tracingEnabled — the resolved flags, or a WithTracingEnabled override if one
// was supplied). tracingEnabled must be false before propagation is
// force-disabled, so no _oteltrace inject/extract occurs while wrapper spans are
// off. When tracingEnabled is true, an explicit option override (e.g.
// WithTracePropagationEnabled) wins, otherwise the resolved propagation flag is
// the default. WithTracePropagationEnabled cannot bypass tracingEnabled being
// false, however that false came about.
//
// tracingEnabled is a parameter rather than an internal mongoTracingEnabled()
// call so a WithTracingEnabled(true) override (global switch off) still lets
// WithTracePropagationEnabled take effect. Reintroducing an internal recompute
// here would silently break that combination.
func resolveDocumentPropagation(tracingEnabled bool, override *bool) bool {
	if !tracingEnabled {
		return false
	}
	return resolveFlag(override, mongoPropagationResolved())
}

func resolveFlag(override *bool, envDefault bool) bool {
	if override != nil {
		return *override
	}
	return envDefault
}

// cachedPropagationEnabled reports the full three-tier propagation decision for
// the package-level ContextFromDocument / ContextFromRawDocument helpers.
//
// Those are package-level functions with no Client to consult, so they see
// neither WithTracingEnabled nor WithTracePropagationEnabled — a Client whose
// Collection writes _oteltrace because of an override may still see ok == false
// here. They DO follow the relay, within the resolver's TTL, so a flag that
// stops the Collection path also stops a change-stream reader in the same loop.
//
// Caching lives in the resolver, so repeated calls in a hot decode loop do not
// re-enter the OpenFeature evaluation pipeline.
func cachedPropagationEnabled() bool {
	return mongoPropagationEnabled()
}
