package otelnats

import (
	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats/internal/flags"
)

const (
	envGlobalTracingEnabled   = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envNATSTracingEnabled     = "OTEL_NATS_TRACING_ENABLED"
	envNATSPropagationEnabled = "OTEL_NATS_PROPAGATION_ENABLED"
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

// natsPropagationGate caches the composed three-tier propagation gate
// (natsGate AND OTEL_NATS_PROPAGATION_ENABLED). Defaults OFF when the
// propagation env var is unset (universal default-OFF posture). When OFF,
// the traced impl still emits wrapper spans but skips W3C header inject on
// publish and skips Extract on subscribe. The tracing gate is a hard
// prerequisite — explicitly setting propagation=true while tracing is off
// keeps propagation disabled.
var natsPropagationGate = flags.NewGate(func() bool {
	return natsGate.Enabled() && flags.EnvEnabled(envNATSPropagationEnabled)
})

func natsPropagationEnabled() bool {
	return natsPropagationGate.Enabled()
}
