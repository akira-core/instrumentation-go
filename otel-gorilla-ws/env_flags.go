package otelgorillaws

import (
	"github.com/akira-core/instrumentation-go/otel-gorilla-ws/internal/flags"
)

const (
	envGlobalTracingEnabled = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
	envWSTracingEnabled     = "OTEL_GORILLA_WS_TRACING_ENABLED"
)

// wsGate caches the composed two-tier tracing gate
// (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED AND OTEL_GORILLA_WS_TRACING_ENABLED)
// for the lifetime of the process. The actual per-Conn tracing decision is
// AND-ed with runtime Sec-WebSocket-Protocol negotiation at Dial / Upgrade time.
var wsGate = flags.NewGate(func() bool {
	return flags.EnvEnabled(envGlobalTracingEnabled) && flags.EnvEnabled(envWSTracingEnabled)
})

func wsTracingEnabled() bool {
	return wsGate.Enabled()
}
