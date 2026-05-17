// Package otelgorillaws wraps github.com/gorilla/websocket and adds
// OpenTelemetry distributed-tracing support by propagating the W3C Trace
// Context inside the WebSocket message body via otel-ws subprotocol negotiation.
//
// Tracing is enabled only when both sides agree on the otel-ws subprotocol:
//   - Client: Dial injects "otel-ws" at the front of the proposed subprotocol list.
//     Tracing is enabled if the server responds with an "otel-ws+" prefixed protocol.
//   - Server: Upgrader.Upgrade detects "otel-ws" in the client's list and responds
//     with "otel-ws+<negotiated>". Tracing is enabled on acceptance.
//
// Connections without otel-ws negotiation operate in passthrough mode (no envelope
// wrapping), but send/receive spans are still created. NewConn keeps tracing on for callers
// that manage the WebSocket handshake themselves (backwards compatibility).
//
// Tracer initialization: Set the global TracerProvider and TextMapPropagator at
// process startup (see examples/) or pass WithTracerProvider/WithPropagators when
// creating a Conn. If options are omitted, each Conn falls back to
// otel.GetTracerProvider() and otel.GetTextMapPropagator().
package otelgorillaws

import (
	"context"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/shared"
	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/traced"
)

// otelWSProtocol is the subprotocol token injected during the WebSocket handshake
// to negotiate otel-ws trace propagation support.
const otelWSProtocol = "otel-ws"

// Conn is a WebSocket connection with built-in OpenTelemetry trace-context
// propagation. It embeds *websocket.Conn so callers can still use all other
// gorilla/websocket methods (Close, WriteJSON, ReadJSON, …) directly, while
// the overridden WriteMessage / ReadMessage dispatch through the chosen impl.
//
// Strategy-split: the impl is chosen exactly once at construction time —
// internal/direct.Conn when the env feature flag is off, internal/traced.Conn
// (with PropagationEnabled true|false derived from otel-ws subprotocol
// negotiation) when the flag is on. Public method bodies contain no runtime
// gate.
type Conn struct {
	*websocket.Conn
	impl shared.ConnImpl
}

// Compile-time assertions: both internal impls satisfy the strategy interface.
// Adding a method to ConnImpl without implementing it in both impls fails
// the build.
var (
	_ shared.ConnImpl = (*direct.Conn)(nil)
	_ shared.ConnImpl = (*traced.Conn)(nil)
)

// Subprotocol returns the application protocol negotiated for this connection.
// For otel-ws negotiations, the "otel-ws+" prefix is removed (e.g. "otel-ws+json" -> "json").
// For plain protocols it returns the original value.
func (c *Conn) Subprotocol() string {
	return appProtocolFromRaw(c.Conn.Subprotocol())
}

// WriteMessage sends a message over the WebSocket connection. Behaviour is
// determined entirely by the impl picked at construction time:
//   - direct.Conn: passthrough to *websocket.Conn.WriteMessage, no span, no envelope.
//   - traced.Conn with PropagationEnabled=false: emit websocket.send PRODUCER span,
//     no envelope wrap.
//   - traced.Conn with PropagationEnabled=true: emit span AND wrap payload in the
//     envelope carrying traceparent / tracestate.
func (c *Conn) WriteMessage(ctx context.Context, messageType int, data []byte) error {
	return c.impl.WriteMessage(ctx, messageType, data)
}

// ReadMessage reads the next message from the WebSocket connection. Behaviour
// is determined entirely by the impl:
//   - direct.Conn: passthrough, returns caller-supplied ctx.
//   - traced.Conn with PropagationEnabled=false: emit websocket.receive CONSUMER span,
//     return raw bytes and caller-supplied ctx (no envelope parse, no propagator.Extract).
//   - traced.Conn with PropagationEnabled=true: emit span, parse envelope, return
//     decoded payload and ctx carrying the extracted remote trace.
func (c *Conn) ReadMessage(ctx context.Context) (context.Context, int, []byte, error) {
	return c.impl.ReadMessage(ctx)
}

// NewConn wraps an existing gorilla *websocket.Conn. For backward compatibility
// with callers that manage the WebSocket handshake themselves, the traced impl
// is selected with PropagationEnabled=true when the env feature flag is on.
// For spec-compliant subprotocol negotiation, use Dial (client) or
// Upgrader.Upgrade (server) instead.
func NewConn(conn *websocket.Conn, opts ...Option) *Conn {
	return newConn(conn, true, opts...)
}

// newConn is the internal constructor used by NewConn, Dial and Upgrader.Upgrade.
// It picks the strategy-split impl exactly once based on the env feature flag
// and the negotiated otel-ws subprotocol bit.
func newConn(conn *websocket.Conn, negotiated bool, opts ...Option) *Conn {
	if !wsTracingEnabled() {
		return &Conn{Conn: conn, impl: direct.NewConn(conn)}
	}
	tracer, propagator := resolveOptions(opts)
	return &Conn{
		Conn: conn,
		impl: traced.NewConn(conn, tracer, propagator, negotiated),
	}
}

// Dial connects to the WebSocket server and returns a *Conn with trace
// propagation enabled only when the server supports otel-ws.
//
// If subprotocols is non-empty, "otel-ws" is injected at the front of the
// list during the WebSocket handshake. Tracing is enabled only if the server
// confirms otel-ws by returning a protocol with the "otel-ws+" prefix
// (Scenario G). If the server returns a non-otel protocol or no protocol at
// all, the connection operates in passthrough mode (Scenarios C and D).
//
// If subprotocols is nil or empty, no otel-ws injection is performed and the
// returned Conn operates in passthrough mode (Scenario E).
func Dial(ctx context.Context, urlStr string, requestHeader http.Header, subprotocols []string, opts ...Option) (*Conn, *http.Response, error) {
	var otelInjected bool
	dialProtos := subprotocols
	if len(subprotocols) > 0 {
		dialProtos = make([]string, 0, len(subprotocols)+1)
		dialProtos = append(dialProtos, otelWSProtocol)
		dialProtos = append(dialProtos, subprotocols...)
		otelInjected = true
	}

	dialer := websocket.Dialer{
		Subprotocols:     dialProtos,
		HandshakeTimeout: websocket.DefaultDialer.HandshakeTimeout,
		TLSClientConfig:  websocket.DefaultDialer.TLSClientConfig,
		Proxy:            websocket.DefaultDialer.Proxy,
	}

	raw, resp, err := dialer.DialContext(ctx, urlStr, requestHeader)
	if err != nil {
		return nil, resp, err
	}

	var negotiated bool
	if otelInjected {
		// Scenario C: server returned a non-otel app protocol → passthrough.
		// Scenario D: server returned no protocol → passthrough.
		// Scenario G: server returned "otel-ws+<proto>" → tracing enabled.
		negotiated = isOTelWireProtocol(raw.Subprotocol())
	}
	// Scenario E: otelInjected=false → negotiated=false (passthrough).

	return newConn(raw, negotiated, opts...), resp, nil
}

func appProtocolFromRaw(rawProto string) string {
	switch {
	case strings.HasPrefix(rawProto, otelWSProtocol+"+"):
		return strings.TrimPrefix(rawProto, otelWSProtocol+"+")
	case rawProto == otelWSProtocol:
		return ""
	default:
		return rawProto
	}
}

func isOTelWireProtocol(rawProto string) bool {
	return rawProto == otelWSProtocol || strings.HasPrefix(rawProto, otelWSProtocol+"+")
}
