package otelnats

import (
	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats/internal/flags"
)

const (
	envGlobalTracingEnabled = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envNATSTracingEnabled   = "OTEL_NATS_TRACING_ENABLED"
)

// natsGate caches the composed two-tier tracing gate
// (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED AND OTEL_NATS_TRACING_ENABLED)
// for the lifetime of the process. Constructed once at package init; the
// constructor reads it once per Conn creation, so hot paths pay zero env
// lookup cost.
var natsGate = flags.NewGate(func() bool {
	return flags.EnvEnabled(envGlobalTracingEnabled) && flags.EnvEnabled(envNATSTracingEnabled)
})

func natsTracingEnabled() bool {
	return natsGate.Enabled()
}
