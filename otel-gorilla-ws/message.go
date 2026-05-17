package otelgorillaws

import (
	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/shared"
)

// Re-exported header keys for callers that switch from raw gorilla to this
// package. The canonical definitions live in internal/shared.
const (
	// TraceparentHeader is the canonical W3C trace context header key.
	TraceparentHeader = shared.Traceparent
	// TracestateHeader is the canonical W3C trace context header key.
	TracestateHeader = shared.Tracestate
)

// wireEnvelope is the on-wire format shared with the JS instrumentation
// packages. Aliased to the canonical type in internal/shared so the wire
// shape stays single-sourced.
type wireEnvelope = shared.WireEnvelope

// marshalWire wraps payload in the envelope format. Thin facade over
// shared.MarshalWire; kept for in-package tests.
func marshalWire(carrier map[string]string, payload []byte) ([]byte, error) {
	return shared.MarshalWire(carrier, payload)
}

// tryUnmarshalWire extracts trace headers from an incoming message and
// returns the original user payload. Thin facade over shared.TryUnmarshalWire.
func tryUnmarshalWire(data []byte) (payload []byte, headers map[string]string, ok bool) {
	return shared.TryUnmarshalWire(data)
}
