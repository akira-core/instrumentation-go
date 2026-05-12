package otelmongo

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	envGlobalTracingEnabled    = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envMongoTracingEnabled     = "OTEL_MONGO_TRACING_ENABLED"
	envMongoPropagationEnabled = "OTEL_MONGO_PROPAGATION_ENABLED"
)

func mongoTracingEnabled() bool {
	if !envEnabledByDefault(envGlobalTracingEnabled) {
		return false
	}
	return envEnabledByDefault(envMongoTracingEnabled)
}

// mongoPropagationEnvOnly reports OTEL_MONGO_PROPAGATION_ENABLED alone (no global gate).
// Used by resolveDocumentPropagation as the env default.
func mongoPropagationEnvOnly() bool {
	return envEnabledByDefault(envMongoPropagationEnabled)
}

func mongoPropagationEnabled() bool {
	return resolveDocumentPropagation(nil)
}

// resolveDocumentPropagation returns the effective _oteltrace propagation flag for a Client.
// Both the global env (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED) and the module env
// (OTEL_MONGO_TRACING_ENABLED) must be on; otherwise propagation is force-disabled so
// no _oteltrace inject/extract occurs while wrapper spans are off. When both are on,
// an explicit option override (e.g. WithTracePropagationEnabled) wins, otherwise
// OTEL_MONGO_PROPAGATION_ENABLED is the default. WithTracePropagationEnabled cannot
// bypass a disabled tracing gate.
func resolveDocumentPropagation(override *bool) bool {
	if !mongoTracingEnabled() {
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

// cachedPropagationEnabled returns the effective document propagation flag, evaluated once
// per process. Used by package-level ContextFromDocument / ContextFromRawDocument to avoid
// repeated os.LookupEnv calls in hot loops (e.g. change-stream iteration).
//
// WARNING: env changes after the first call are ignored. This is intentional — OTel
// instrumentation env is expected to be set at process startup. Tests must call
// resetPropEnabledCacheForTest after t.Setenv to re-evaluate.
var (
	propEnabledOnce sync.Once
	propEnabledFlag atomic.Bool
)

func cachedPropagationEnabled() bool {
	propEnabledOnce.Do(func() {
		propEnabledFlag.Store(mongoPropagationEnabled())
	})
	return propEnabledFlag.Load()
}

func envEnabledByDefault(key string) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
