// Package otelgorillaws provides OpenTelemetry instrumentation for the
// github.com/gorilla/websocket package. It wraps *websocket.Conn with W3C
// Trace Context propagation via a JSON envelope on the wire and produces
// CLIENT/SERVER spans for WriteMessage/ReadMessage when the OTel
// Sec-WebSocket-Protocol subprotocol is negotiated end-to-end.
//
// Feature flags are default-disabled. With either OTEL_INSTRUMENTATION_GO_TRACING_ENABLED
// or OTEL_GORILLA_WS_TRACING_ENABLED off, the wrapper is constructed with its
// disabled-mode impl (passthrough to the underlying *websocket.Conn — no JSON
// envelope, no span creation). Even with flags on, the connection falls back
// to passthrough when the peer does not advertise the OTel subprotocol.
//
// The package does NOT initialise a TracerProvider. Set the global provider
// and propagator at process startup via go.opentelemetry.io/otel.
package otelgorillaws
