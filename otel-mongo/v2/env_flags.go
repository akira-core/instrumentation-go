package otelmongo

import (
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/flags"
)

const (
	envGlobalTracingEnabled    = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envMongoTracingEnabled     = "OTEL_MONGO_TRACING_ENABLED"
	envMongoPropagationEnabled = "OTEL_MONGO_PROPAGATION_ENABLED"
)

func mongoTracingEnabled() bool {
	if !flags.EnvEnabled(envGlobalTracingEnabled) {
		return false
	}
	return flags.EnvEnabled(envMongoTracingEnabled)
}

// mongoPropagationEnvOnly reports OTEL_MONGO_PROPAGATION_ENABLED alone (no global gate).
// Used by resolveDocumentPropagation as the env default.
func mongoPropagationEnvOnly() bool {
	return flags.EnvEnabled(envMongoPropagationEnabled)
}

func mongoPropagationEnabled() bool {
	return resolveDocumentPropagation(mongoTracingEnabled(), nil)
}

// resolveDocumentPropagation returns the effective _oteltrace propagation flag
// for a Client, given that Client's already-resolved effective tracing state
// (tracingEnabled — the env gates, or a WithTracingEnabled override if one was
// supplied). tracingEnabled must be false before propagation is force-disabled,
// so no _oteltrace inject/extract occurs while wrapper spans are off. When
// tracingEnabled is true, an explicit option override (e.g.
// WithTracePropagationEnabled) wins, otherwise OTEL_MONGO_PROPAGATION_ENABLED
// is the default. WithTracePropagationEnabled cannot bypass tracingEnabled
// being false, however that false came about.
//
// tracingEnabled is a parameter rather than an internal mongoTracingEnabled()
// call so a WithTracingEnabled(true) override (env gates unset) still lets
// WithTracePropagationEnabled take effect — the package-level
// mongoPropagationEnabled() (used only by the process-wide, env-only
// propEnabledGate that ContextFromDocument/ContextFromRawDocument read) passes
// the plain env-derived mongoTracingEnabled() explicitly, preserving its
// existing env-only behavior.
func resolveDocumentPropagation(tracingEnabled bool, override *bool) bool {
	if !tracingEnabled {
		return false
	}
	return resolveFlag(override, mongoPropagationEnvOnly())
}

func resolveFlag(override *bool, envDefault bool) bool {
	if override != nil {
		return *override
	}
	return envDefault
}

// propEnabledGate caches the effective document propagation flag (full three-tier
// resolution) for the lifetime of the process. Used by package-level
// ContextFromDocument / ContextFromRawDocument to avoid repeated os.LookupEnv calls
// in hot loops (e.g. change-stream iteration).
//
// WARNING: env changes after the first call are ignored. This is intentional — OTel
// instrumentation env is expected to be set at process startup. Tests must call
// resetPropEnabledCacheForTest after t.Setenv to re-evaluate.
var propEnabledGate = flags.NewGate(mongoPropagationEnabled)

func cachedPropagationEnabled() bool {
	return propEnabledGate.Enabled()
}
