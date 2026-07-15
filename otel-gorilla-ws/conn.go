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
// Both sides gate negotiation on the effective tracing feature flag (env
// gates, overridable per connection via WithTracingEnabled): a feature-off
// side never offers or confirms otel-ws, so the peer is never committed to
// the envelope wire format a feature-off side would not unwrap.
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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
const ScopeName = "instrumentation-go/otel-gorilla-ws"

// otelWSProtocol is the subprotocol token injected during the WebSocket handshake
// to negotiate otel-ws trace propagation support.
const otelWSProtocol = "otel-ws"

// Conn is a WebSocket connection with built-in OpenTelemetry trace-context
// propagation.  It embeds *websocket.Conn so that callers can still use all
// other gorilla/websocket methods directly.
type Conn struct {
	*websocket.Conn

	propagator     propagation.TextMapPropagator
	tracer         trace.Tracer
	tracingEnabled bool // true only after successful otel-ws subprotocol negotiation
	featureEnabled bool // env feature flag controlling both span and propagation
}

// Subprotocol returns the application protocol negotiated for this connection.
// For otel-ws negotiations, the "otel-ws+" prefix is removed (e.g. "otel-ws+json" -> "json").
// For plain protocols it returns the original value.
func (c *Conn) Subprotocol() string {
	return appProtocolFromRaw(c.Conn.Subprotocol())
}

// NewConn wraps an existing gorilla *websocket.Conn with tracing always enabled.
// This preserves backwards-compatible behaviour for callers that manage the
// WebSocket handshake themselves. For spec-compliant subprotocol negotiation,
// use Dial (client) or Upgrader.Upgrade (server).
func NewConn(conn *websocket.Conn, opts ...Option) *Conn {
	return newConn(conn, true, opts...)
}

// newConn wraps conn with the given negotiation outcome, resolving opts.
func newConn(conn *websocket.Conn, tracingEnabled bool, opts ...Option) *Conn {
	return newConnFromConfig(conn, tracingEnabled, resolveConnOptions(opts))
}

// newConnFromConfig is the constructor core shared by NewConn, Dial and
// Upgrader.Upgrade — the latter two resolve their options before the
// handshake (to gate otel-ws negotiation) and pass the parsed config here.
func newConnFromConfig(conn *websocket.Conn, tracingEnabled bool, cfg connOptions) *Conn {
	c := &Conn{
		Conn:           conn,
		tracingEnabled: tracingEnabled,
	}
	configureConn(c, cfg)
	return c
}

// WriteMessage sends a message over the WebSocket connection and always creates
// a "websocket.send" producer span. Trace context injection into the wire envelope
// happens only when otel-ws propagation is enabled.
func (c *Conn) WriteMessage(ctx context.Context, messageType int, data []byte) error {
	if !c.featureEnabled {
		return c.Conn.WriteMessage(messageType, data)
	}
	ctx, span := c.tracer.Start(ctx, "websocket.send",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.Int("websocket.message.type", messageType),
			attribute.Int("websocket.message.body.size", len(data)),
		),
	)
	defer span.End()

	payload := data
	if c.tracingEnabled {
		carrier := make(propagation.MapCarrier)
		c.propagator.Inject(ctx, carrier)

		encoded, err := marshalWire(carrier, data)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		payload = encoded
	}
	if err := c.Conn.WriteMessage(messageType, payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// ReadMessage reads the next message from the WebSocket connection and always
// creates a "websocket.receive" consumer span. Trace context extraction from
// the wire envelope happens only when otel-ws propagation is enabled.
func (c *Conn) ReadMessage(ctx context.Context) (context.Context, int, []byte, error) {
	if !c.featureEnabled {
		msgType, raw, err := c.Conn.ReadMessage()
		return ctx, msgType, raw, err
	}
	msgType, raw, err := c.Conn.ReadMessage()
	if err != nil {
		_, span := c.tracer.Start(ctx, "websocket.receive",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attribute.Int("websocket.message.type", msgType)),
		)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return ctx, msgType, raw, err
	}

	outCtx := ctx
	payload := raw
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
	}

	if c.tracingEnabled {
		decoded, hdrs, ok := tryUnmarshalWire(raw)
		if ok {
			payload = decoded

			carrier := propagation.MapCarrier(hdrs)
			senderCtx := c.propagator.Extract(ctx, carrier)
			if sc := trace.SpanContextFromContext(senderCtx); sc.IsValid() {
				startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: sc}))
			}
			outCtx = senderCtx
		}
	}

	outCtx, span := c.tracer.Start(outCtx, "websocket.receive",
		append(startOpts,
			trace.WithAttributes(
				attribute.Int("websocket.message.type", msgType),
				attribute.Int("websocket.message.body.size", len(payload)),
			),
		)...,
	)
	span.End()

	return outCtx, msgType, payload, nil
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
//
// When the connection's effective tracing feature is off (env gates, or
// WithTracingEnabled(false)), otel-ws is not offered at all: a feature-off
// side never unwraps the JSON envelope, so offering the subprotocol would
// commit an otel-ws-aware server to a wire format this client cannot read.
// As defense in depth for that path (and whenever subprotocols is empty),
// any otel-ws token the caller placed in requestHeader is stripped before
// gorilla sees it — see stripOTelSubprotocol.
func Dial(ctx context.Context, urlStr string, requestHeader http.Header, subprotocols []string, opts ...Option) (*Conn, *http.Response, error) {
	cfg := resolveConnOptions(opts)
	featureOn := effectiveFeatureEnabled(cfg)

	var otelInjected bool
	dialProtos := subprotocols
	if featureOn && len(subprotocols) > 0 {
		dialProtos = make([]string, 0, len(subprotocols)+1)
		dialProtos = append(dialProtos, otelWSProtocol)
		dialProtos = append(dialProtos, subprotocols...)
		otelInjected = true
	} else {
		// gorilla's Dialer silently sends a caller-supplied
		// Sec-Websocket-Protocol request header verbatim whenever
		// Dialer.Subprotocols is empty (true whenever otelInjected is false
		// here). Strip any otel-ws token so it can't smuggle an otel-ws offer
		// past this feature-off/no-subprotocols path.
		requestHeader = stripOTelSubprotocol(requestHeader)
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

	var tracingEnabled bool
	if otelInjected {
		negotiated := raw.Subprotocol()
		// Scenario C: server returned a non-otel app protocol → passthrough.
		// Scenario D: server returned no protocol → passthrough (connection kept alive).
		// Scenario G: server returned "otel-ws+<proto>" → tracing enabled.
		tracingEnabled = isOTelWireProtocol(negotiated)
	}
	// Scenario E: otelInjected=false → tracingEnabled=false (passthrough).

	return newConnFromConfig(raw, tracingEnabled, cfg), resp, nil
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
