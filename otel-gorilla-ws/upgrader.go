package otelgorillaws

import (
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Upgrader wraps websocket.Upgrader and adds otel-ws subprotocol negotiation.
// Use Upgrade instead of websocket.Upgrader.Upgrade to get spec-compliant
// trace propagation negotiation on the server side.
//
// Server-side spec behaviour:
//   - Scenario F: client sends no subprotocols → accept, tracing disabled (passthrough)
//   - Scenario G: client sends "otel-ws,json" → respond "otel-ws+json", tracing enabled
//   - Scenario H: client sends "json" (no otel-ws) → accept normally, tracing disabled
type Upgrader struct {
	// Keep gorilla/websocket field names so callers can switch imports with minimal changes.
	HandshakeTimeout  time.Duration
	ReadBufferSize    int
	WriteBufferSize   int
	WriteBufferPool   websocket.BufferPool
	CheckOrigin       func(r *http.Request) bool
	EnableCompression bool
	Error             func(w http.ResponseWriter, r *http.Request, status int, reason error)

	// Subprotocols is equivalent to websocket.Upgrader.Subprotocols and represents
	// application-level protocols (e.g. []string{"json"}).
	Subprotocols []string

	// Inner is the underlying gorilla Upgrader. Set CheckOrigin, ReadBufferSize,
	// WriteBufferSize, Error, etc. on Inner as needed.
	// Do NOT set Inner.Subprotocols — use AppSubprotocols instead.
	Inner websocket.Upgrader

	// AppSubprotocols lists the application-level protocols this server supports
	// (e.g. ["json", "binary"]). The first match from the client's proposed list
	// is chosen. If nil, the first application protocol the client proposes is
	// accepted (accept-any semantics).
	AppSubprotocols []string
}

// Upgrade upgrades the HTTP connection to WebSocket with otel-ws negotiation.
//
// When the client includes "otel-ws" in its Sec-WebSocket-Protocol header,
// the server responds with "otel-ws" (no app protocol) or "otel-ws+<negotiated>"
// (with app protocol), and the returned Conn has tracing enabled (Scenario G).
// Otherwise the connection is accepted with
// normal protocol selection and the returned Conn operates in passthrough
// mode (Scenarios F and H).
//
// gorilla constraint: when Inner.Subprotocols is nil, gorilla's selectSubprotocol
// reads the subprotocol from responseHeader. When Inner.Subprotocols is non-nil,
// gorilla ignores responseHeader for protocol selection. This method sets
// Inner.Subprotocols=nil and injects the otel-ws+<proto> value via responseHeader
// for the otel-ws path; for the non-otel path it restores Inner.Subprotocols so
// gorilla performs normal matching.
//
// opts (WithTracerProvider, WithPropagators, WithTracingEnabled) configure the
// returned Conn the same way as NewConn/Dial.
//
// When the connection's effective tracing feature is off (env gates, or
// WithTracingEnabled(false)), otel-ws is never confirmed even if the client
// requested it: confirming would commit the client to the JSON envelope wire
// format that this feature-off side neither writes nor unwraps. The upgrade
// then proceeds with normal application-protocol selection (Scenario H).
func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header, opts ...Option) (*Conn, error) {
	clientProtos := websocket.Subprotocols(r)
	otelRequested, appClientProtos := splitClientProtocols(clientProtos)
	cfg := resolveConnOptions(opts)
	// Gate negotiation on the effective feature flag, resolved BEFORE the
	// handshake — the wire format each side speaks must match what it
	// advertises.
	negotiateOTel := otelRequested && effectiveFeatureEnabled(cfg)

	// Work on a copy of Inner so we never mutate the caller's upgrader.
	inner := u.Inner
	if u.HandshakeTimeout != 0 {
		inner.HandshakeTimeout = u.HandshakeTimeout
	}
	if u.ReadBufferSize != 0 {
		inner.ReadBufferSize = u.ReadBufferSize
	}
	if u.WriteBufferSize != 0 {
		inner.WriteBufferSize = u.WriteBufferSize
	}
	if u.WriteBufferPool != nil {
		inner.WriteBufferPool = u.WriteBufferPool
	}
	if u.CheckOrigin != nil {
		inner.CheckOrigin = u.CheckOrigin
	}
	if u.EnableCompression {
		inner.EnableCompression = true
	}
	if u.Error != nil {
		inner.Error = u.Error
	}

	appProtocols := u.Subprotocols
	if len(appProtocols) == 0 && u.AppSubprotocols != nil {
		appProtocols = u.AppSubprotocols
	}

	if negotiateOTel {
		// Match only app protocols (non otel-ws tokens).
		negotiated := selectFirst(appClientProtos, appProtocols)

		// Respond with "otel-ws" (no app proto) or "otel-ws+<negotiated>" so the
		// client can detect otel-ws support. inner.Subprotocols must be nil so
		// gorilla reads from responseHeader.
		inner.Subprotocols = nil
		responseHeader = cloneHeader(responseHeader)
		proto := otelWSProtocol
		if negotiated != "" {
			proto += "+" + negotiated
		}
		responseHeader.Set("Sec-Websocket-Protocol", proto)
	} else {
		// Scenarios F and H (and otel-ws requests with the feature off): normal
		// gorilla protocol selection from AppSubprotocols. The client's otel-ws
		// tokens are never in appProtocols, so they cannot be selected.
		inner.Subprotocols = appProtocols
	}

	raw, err := inner.Upgrade(w, r, responseHeader)
	if err != nil {
		return nil, err
	}

	return newConnFromConfig(raw, negotiateOTel, cfg), nil
}

// splitClientProtocols returns whether otel-ws propagation was requested and the
// application protocol candidates with otel-ws tokens stripped.
func splitClientProtocols(clientProtos []string) (bool, []string) {
	appProtos := make([]string, 0, len(clientProtos))
	otelRequested := false
	for _, p := range clientProtos {
		if p == otelWSProtocol {
			otelRequested = true
			continue
		}
		if strings.HasPrefix(p, otelWSProtocol+"+") {
			otelRequested = true
			trimmed := strings.TrimPrefix(p, otelWSProtocol+"+")
			if trimmed != "" {
				appProtos = append(appProtos, trimmed)
			}
			continue
		}
		appProtos = append(appProtos, p)
	}
	return otelRequested, appProtos
}

// cloneHeader returns a shallow clone of h (or a new empty header if h is nil).
func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// selectFirst returns the first element of clientProtos that also appears in
// serverProtos. If serverProtos is nil, the first element of clientProtos is
// returned (accept-any semantics). Returns "" if no match is found.
func selectFirst(clientProtos, serverProtos []string) string {
	if len(clientProtos) == 0 {
		return ""
	}
	if serverProtos == nil {
		return clientProtos[0]
	}
	for _, cp := range clientProtos {
		for _, sp := range serverProtos {
			if cp == sp {
				return cp
			}
		}
	}
	return ""
}
