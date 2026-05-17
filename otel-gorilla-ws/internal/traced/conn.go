// Package traced holds the enabled-mode WebSocket impl: emits
// websocket.send / websocket.receive spans on every public method and,
// when PropagationEnabled is true, wraps/unwraps the JSON envelope.
package traced

import (
	"context"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/shared"
)

// Conn carries the resolved tracer + propagator and a cached
// PropagationEnabled flag set at construction time (true ⇔ env gate on
// AND otel-ws subprotocol successfully negotiated). The flag is immutable
// for the connection's lifetime — no per-call env lookup.
type Conn struct {
	WS                 *websocket.Conn
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
}

// NewConn returns an instrumented impl wrapping ws.
func NewConn(ws *websocket.Conn, tracer trace.Tracer, propagator propagation.TextMapPropagator, propagationEnabled bool) *Conn {
	return &Conn{
		WS:                 ws,
		Tracer:             tracer,
		Propagator:         propagator,
		PropagationEnabled: propagationEnabled,
	}
}

// WriteMessage emits a websocket.send PRODUCER span. When
// PropagationEnabled is true it wraps the payload in the JSON envelope
// carrying traceparent/tracestate; otherwise the payload passes through
// unchanged on the wire.
func (c *Conn) WriteMessage(ctx context.Context, messageType int, data []byte) error {
	ctx, span := c.Tracer.Start(ctx, "websocket.send",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.Int("websocket.message.type", messageType),
			attribute.Int("messaging.message.body.size", len(data)),
		),
	)
	defer span.End()

	payload := data
	if c.PropagationEnabled {
		carrier := make(propagation.MapCarrier)
		c.Propagator.Inject(ctx, carrier)

		encoded, err := shared.MarshalWire(carrier, data)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		payload = encoded
	}
	if err := c.WS.WriteMessage(messageType, payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// ReadMessage emits a websocket.receive CONSUMER span. When
// PropagationEnabled is true and the incoming frame is an envelope, the
// span links to the producer's span context and the returned context
// carries the extracted remote trace; otherwise the caller's context is
// returned unchanged and the payload is the raw bytes from the wire.
func (c *Conn) ReadMessage(ctx context.Context) (context.Context, int, []byte, error) {
	msgType, raw, err := c.WS.ReadMessage()
	if err != nil {
		_, span := c.Tracer.Start(ctx, "websocket.receive",
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

	if c.PropagationEnabled {
		decoded, hdrs, ok := shared.TryUnmarshalWire(raw)
		if ok {
			payload = decoded

			carrier := propagation.MapCarrier(hdrs)
			senderCtx := c.Propagator.Extract(ctx, carrier)
			if sc := trace.SpanContextFromContext(senderCtx); sc.IsValid() {
				startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: sc}))
			}
			outCtx = senderCtx
		}
	}

	outCtx, span := c.Tracer.Start(outCtx, "websocket.receive",
		append(startOpts,
			trace.WithAttributes(
				attribute.Int("websocket.message.type", msgType),
				attribute.Int("messaging.message.body.size", len(payload)),
			),
		)...,
	)
	span.End()

	return outCtx, msgType, payload, nil
}
