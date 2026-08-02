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

	// featureOverride is the WithTracingEnabled value when the option was passed.
	// When non-nil the connection is static: featureEnabled returns it verbatim
	// and no OpenFeature evaluation runs. When nil, featureEnabled resolves the
	// dynamic flag per call so a relay change reaches this live connection.
	featureOverride *bool

	// capable records whether this connection could EVER trace — the override
	// when present, otherwise the global kill switch at construction. False ⇒
	// pure passthrough: no envelope handling, no spans, no OpenFeature
	// evaluation, exactly the pre-dynamic disabled behavior. Distinct from both
	// tracingEnabled (the negotiation outcome) and featureEnabled() (the
	// per-call span gate): a capable, negotiated connection whose dynamic flag
	// is off still speaks the envelope wire format, because the peer committed
	// to it at the handshake.
	capable bool
}

// featureEnabled reports whether this call should create spans and inject or
// extract trace context.
//
// Distinct from tracingEnabled, which is the otel-ws subprotocol NEGOTIATION
// outcome and is fixed for the connection's lifetime. A connection can have
// tracingEnabled true (peer agreed to envelopes) while featureEnabled is false
// (relay turned tracing off): it keeps writing envelopes, because the peer
// expects them, but injects nothing and creates no span.
func (c *Conn) featureEnabled() bool {
	if c.featureOverride != nil {
		return *c.featureOverride
	}
	return wsTracingEnabled()
}

// Subprotocol returns the application protocol negotiated for this connection.
// For otel-ws negotiations, the "otel-ws+" prefix is removed (e.g. "otel-ws+json" -> "json").
// For plain protocols it returns the original value.
func (c *Conn) Subprotocol() string {
	return appProtocolFromRaw(c.Conn.Subprotocol())
}

// NewConn wraps an existing gorilla *websocket.Conn. Envelope handling is
// enabled only when the raw connection's negotiated subprotocol proves otel-ws
// (isOTelWireProtocol); otherwise the wire stays raw passthrough. Callers that
// manage the handshake themselves must leave a correct negotiated subprotocol
// on the connection. For handshake-side negotiation, use Dial or Upgrader.Upgrade.
//
// When capable and the dynamic feature gate are on but otel-ws was not proven,
// local send/receive spans may still be created without inject/extract.
func NewConn(conn *websocket.Conn, opts ...Option) *Conn {
	negotiated := false
	if conn != nil {
		negotiated = isOTelWireProtocol(conn.Subprotocol())
	}
	return newConn(conn, negotiated, opts...)
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
// "websocket.receive" consumer span is created while the dynamic span gate is
// on. On a connection that negotiated otel-ws the envelope is ALWAYS unwrapped —
// the peer envelopes every frame regardless of this side's gate, so skipping
// the unwrap would hand raw {"header":...,"data":...} bytes to the application
// whenever the gate is off while the peer still envelopes (a pinned-on peer,
// TTL skew, or in-flight messages written before a flip).
func (c *Conn) ReadMessage(ctx context.Context) (context.Context, int, []byte, error) {
	if !c.capable {
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

	if c.tracingEnabled {
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
	featureOn := effectiveCapability(cfg)

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
