package otelmongo

import (
	"os"
	"strings"
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
// The global env (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED) must be on; an explicit option
// override (e.g. WithTracePropagationEnabled) wins, otherwise OTEL_MONGO_PROPAGATION_ENABLED
// is the default. WithTracePropagationEnabled cannot bypass a disabled global.
func resolveDocumentPropagation(override *bool) bool {
	if !envEnabledByDefault(envGlobalTracingEnabled) {
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
