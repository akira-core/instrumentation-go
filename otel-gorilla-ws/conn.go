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
// wrapping); local send/receive spans may still be created when the feature gate
// is on. NewConn proves negotiation only via the raw connection's subprotocol.
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
	"go.opentelemetry.io/otel/trace/noop"
)

// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
const ScopeName = "instrumentation-go/otel-gorilla-ws"

// SubprotocolOTelWS is the WebSocket subprotocol token this package negotiates
// to enable trace-context propagation.
//
// It is exported for callers who run their own handshake and then wrap the
// result with NewConn, which enables the envelope only when the raw connection
// carries a negotiated otel-ws subprotocol. Offer this token (client) or echo
// it (server) to satisfy that. A stock websocket.Dialer or Upgrader can reach
// only the bare form, because gorilla echoes exact matches; the composite
// "otel-ws+<app>" form is produced only by this package's Upgrader.Upgrade.
//
// Exporting the token is not an escape hatch: it cannot force an envelope onto
// a peer that did not negotiate one.
const SubprotocolOTelWS = "otel-ws"

// otelWSProtocol is the internal spelling of SubprotocolOTelWS.
const otelWSProtocol = SubprotocolOTelWS

// IsOTelNegotiated reports whether conn's negotiated subprotocol proves otel-ws,
// i.e. whether NewConn will enable envelope handling on it.
//
// Callers running their own handshake use this to verify the outcome instead of
// assuming it: a connection that silently failed to negotiate otel-ws still
// works, but carries no trace context.
func IsOTelNegotiated(conn *websocket.Conn) bool {
	if conn == nil {
		return false
	}
	return isOTelWireProtocol(conn.Subprotocol())
}

// Conn is a WebSocket connection with built-in OpenTelemetry trace-context
// propagation.  It embeds *websocket.Conn so that callers can still use all
// other gorilla/websocket methods directly.
type Conn struct {
	*websocket.Conn

	propagator propagation.TextMapPropagator
	tracer     trace.Tracer

	// enveloped records the WIRE FACT: the peer envelopes every frame, because
	// otel-ws was negotiated (Dial / Upgrade) or proven from the raw
	// connection's subprotocol (NewConn). It is NOT clamped by capability —
	// whether the peer wraps its frames is settled by the handshake and this
	// side's local gate has no power over it. The read path keys on this, which
	// is what stops a capability-off wrapper from handing raw
	// {"header":...,"data":...} bytes to the application.
	enveloped bool

	// tracingEnabled is the WRITE-side envelope decision: enveloped AND
	// capable. A capability-off process writes raw frames even to a peer that
	// envelopes — safe, because that peer's probe falls back to the payload —
	// while a capable, negotiated connection whose relay verdict is off still
	// writes the envelope, because the peer committed to it at the handshake.
	tracingEnabled bool

	// capable records whether any OTel SDK path could EVER run on this
	// connection: gate.tracedPossible(), fixed at construction. False ⇒ a noop
	// tracer, no spans and no OpenFeature evaluation, exactly the pre-dynamic
	// disabled behavior. Distinct from enveloped (the wire fact) and from
	// featureEnabled() (the per-call gate).
	capable bool

	// gate holds the connection's switch state fixed at construction — the
	// master and module local values, and whether a relay can exist at all.
	gate gateState
}

// featureEnabled reports whether this call should create spans and inject or
// extract trace context.
//
// It is the per-call gate: the whole ladder, master switch included, resolved
// fresh every time so a relay change reaches this live connection on its next
// operation. There is no option branch — WithTracingEnabled supplied one rung of
// the ladder at construction.
//
// Distinct from enveloped and tracingEnabled, which describe the wire. A
// connection can be enveloped with featureEnabled false: it keeps writing
// envelopes because the peer expects them, but injects nothing and creates no
// span. And it can have featureEnabled true while not enveloped — a connection
// opened before the relay enabled this module emits local spans but cannot
// inject or extract, because negotiation cannot be revisited.
func (c *Conn) featureEnabled() bool {
	return c.gate.tracing()
}

// Subprotocol returns the application protocol negotiated for this connection.
// For otel-ws negotiations, the "otel-ws+" prefix is removed (e.g. "otel-ws+json" -> "json").
// For plain protocols it returns the original value.
func (c *Conn) Subprotocol() string {
	return appProtocolFromRaw(c.Conn.Subprotocol())
}

// NewConn wraps an existing gorilla *websocket.Conn. Envelope handling is
// enabled only when the raw connection's negotiated subprotocol proves otel-ws
// (see IsOTelNegotiated); otherwise the wire stays raw passthrough. Callers that
// manage the handshake themselves must leave a correct negotiated subprotocol on
// the connection — offer or echo SubprotocolOTelWS. For handshake-side
// negotiation, use Dial or Upgrader.Upgrade.
//
// It returns an error when an OTEL_*_ENABLED variable this connection reads is
// set to a value that is neither truthy nor falsy — including the empty string.
// Supplying WithTracingEnabled alongside its paired variable is legal; the
// variable wins.
//
// When capable and the relay verdict are on but otel-ws was not proven, local
// send/receive spans may still be created without inject/extract.
func NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error) {
	return newConn(conn, IsOTelNegotiated(conn), opts...)
}

// newConn wraps conn with the given negotiation outcome, resolving opts.
func newConn(conn *websocket.Conn, enveloped bool, opts ...Option) (*Conn, error) {
	cfg := resolveConnOptions(opts)
	gate, err := resolveGates(cfg)
	if err != nil {
		return nil, err
	}
	return newConnFromConfig(conn, enveloped, cfg, gate), nil
}

// newConnFromConfig is the constructor core shared by NewConn, Dial and
// Upgrader.Upgrade — the latter two resolve their options and gate state before
// the handshake (to gate otel-ws negotiation) and pass both here.
func newConnFromConfig(conn *websocket.Conn, enveloped bool, cfg connOptions, gate gateState) *Conn {
	c := &Conn{
		Conn:      conn,
		enveloped: enveloped,
	}
	configureConn(c, cfg, gate)
	return c
}

// WriteMessage sends a message over the WebSocket connection. A "websocket.send"
// producer span is created while the dynamic span gate is on. On a connection
// that negotiated otel-ws the JSON envelope is ALWAYS written — the peer parses
// every frame as an envelope, so the wire format cannot follow a flag that may
// flip mid-connection; while the gate is off the envelope carries an empty
// header and no span is created.
func (c *Conn) WriteMessage(ctx context.Context, messageType int, data []byte) error {
	if !c.capable {
		// This connection can never trace: pure passthrough, no envelope.
		return c.Conn.WriteMessage(messageType, data)
	}
	feature := c.featureEnabled()

	// Always hold a non-nil span: feature-off uses a non-recording noop so error
	// paths never need nil guards (and a future branch cannot panic on flag-off).
	span := trace.Span(noop.Span{})
	if feature {
		ctx, span = c.tracer.Start(ctx, "websocket.send",
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(
				attribute.Int("websocket.message.type", messageType),
				attribute.Int("websocket.message.body.size", len(data)),
			),
		)
		defer span.End()
	}

	payload := data
	if c.tracingEnabled {
		// Negotiated peer ⇒ envelope unconditionally. Writing raw bytes here
		// while the gate is off would hand the peer's tryUnmarshalWire an
		// unwrapped payload — and any application payload shaped like
		// {"header":...,"data":...} would be silently dismembered by a peer
		// whose gate is on (e.g. pinned on via WithTracingEnabled(true)).
		carrier := make(propagation.MapCarrier)
		if feature {
			c.propagator.Inject(ctx, carrier)
		}
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

// ReadMessage reads the next message from the WebSocket connection. A
// "websocket.receive" consumer span is created while the per-call gate is on.
//
// On a connection whose peer envelopes, the envelope is ALWAYS unwrapped —
// including when this side is not capable of tracing at all. The peer's framing
// is a fact of the handshake, not of this side's gate, so keying the unwrap on
// capability would hand raw {"header":...,"data":...} bytes to the application
// whenever a capability-off process wraps a negotiated connection, or whenever
// the peer is pinned on while this side is off.
func (c *Conn) ReadMessage(ctx context.Context) (context.Context, int, []byte, error) {
	if !c.capable && !c.enveloped {
		msgType, raw, err := c.Conn.ReadMessage()
		return ctx, msgType, raw, err
	}
	feature := c.featureEnabled()
	msgType, raw, err := c.Conn.ReadMessage()
	if err != nil {
		if feature {
			_, span := c.tracer.Start(ctx, "websocket.receive",
				trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithAttributes(attribute.Int("websocket.message.type", msgType)),
			)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
		}
		return ctx, msgType, raw, err
	}

	outCtx := ctx
	payload := raw
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
	}

	if c.enveloped {
		decoded, hdrs, ok := tryUnmarshalWire(raw)
		if ok {
			payload = decoded

			if feature {
				carrier := propagation.MapCarrier(hdrs)
				senderCtx := c.propagator.Extract(ctx, carrier)
				if sc := trace.SpanContextFromContext(senderCtx); sc.IsValid() {
					startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: sc}))
				}
				outCtx = senderCtx
			}
		}
	}

	if !feature {
		return outCtx, msgType, payload, nil
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
	gate, err := resolveGates(cfg)
	if err != nil {
		return nil, nil, err
	}
	// Resolved ONCE, here, from the whole ladder including the relay. A handshake
	// cannot be revisited, so this is the only moment the wire decision can be
	// made — and it is why enabling this module reaches new connections only.
	featureOn := gate.tracing()

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

	var enveloped bool
	if otelInjected {
		negotiated := raw.Subprotocol()
		// Scenario C: server returned a non-otel app protocol → passthrough.
		// Scenario D: server returned no protocol → passthrough (connection kept alive).
		// Scenario G: server returned "otel-ws+<proto>" → envelope on the wire.
		enveloped = isOTelWireProtocol(negotiated)
	}
	// Scenario E: otelInjected=false → enveloped=false (passthrough).

	return newConnFromConfig(raw, enveloped, cfg, gate), resp, nil
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
