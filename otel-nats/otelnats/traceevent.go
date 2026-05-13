package otelnats

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	nats "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceEvent is the JSON payload that NATS server publishes to the Nats-Trace-Dest subject
// whenever a message passes through the server. See ADR-41 for the full specification.
// Each event covers one server; a multi-server cluster produces one payload per server.
type TraceEvent struct {
	Server  TraceServerInfo  `json:"server"`
	Request TraceRequestInfo `json:"request"`
	// Hops is the number of remote destinations (routes/gateways/leafnodes) the message was forwarded to.
	Hops   int        `json:"hops"`
	Events []TraceHop `json:"events"`
}

// TraceServerInfo identifies the NATS server that produced this trace event.
type TraceServerInfo struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	ID      string `json:"id"`
	Cluster string `json:"cluster"`
	Version string `json:"ver"`
}

// TraceRequestInfo holds metadata about the original message that triggered tracing.
// Headers contains the original NATS message headers (e.g. traceparent, Nats-Trace-Dest).
type TraceRequestInfo struct {
	Headers map[string][]string `json:"header"`
	MsgSize int                 `json:"msgsize"`
}

// TraceHop is a single infrastructure event in the message path on one NATS server.
// NATS publishes different struct types per event (ingress, egress, JetStream, subject-mapping,
// stream-export, service-import); we use a single flat struct covering all fields.
// Each TraceEvent may contain multiple hops (e.g. ingress + JetStream store + egress).
type TraceHop struct {
	// Type classifies the infrastructure step: "in" (ingress), "eg" (egress),
	// "sm" (subject mapping), "se" (stream export), "si" (service import), "js" (JetStream).
	Type string    `json:"type"`
	TS   time.Time `json:"ts"`
	// Kind is the connection type: 0=CLIENT, 1=ROUTER, 2=GATEWAY, 3=SYSTEM, 4=LEAF, 5=JETSTREAM, 6=ACCOUNT.
	Kind  int    `json:"kind"`
	CID   uint64 `json:"cid"`
	Name  string `json:"name"`
	Acc   string `json:"acc"`
	Subj  string `json:"subj"`
	Sub   string `json:"sub"`
	Queue string `json:"queue"`
	Error string `json:"error"`
	Hop   string `json:"hop"`
	// JetStream-specific
	Stream     string `json:"stream"`
	NoInterest bool   `json:"nointerest"`
	// Subject-mapping specific
	MappedTo string `json:"mappedto"`
	// Stream-export / service-import specific
	To   string `json:"to"`
	From string `json:"from"`
}

// hopKindName converts the NATS connection kind integer to a human-readable string.
func hopKindName(kind int) string {
	switch kind {
	case 0:
		return "CLIENT"
	case 1:
		return "ROUTER"
	case 2:
		return "GATEWAY"
	case 3:
		return "SYSTEM"
	case 4:
		return "LEAF"
	case 5:
		return "JETSTREAM"
	case 6:
		return "ACCOUNT"
	default:
		return "UNKNOWN"
	}
}

// hopTypeName converts the NATS trace event type code to a human-readable label.
func hopTypeName(t string) string {
	switch t {
	case "in":
		return "ingress"
	case "eg":
		return "egress"
	case "sm":
		return "stream.store"
	case "se":
		return "stream.egress"
	case "si":
		return "stream.incoming"
	case "js":
		return "jetstream"
	default:
		return t
	}
}

// SubscribeTraceEvents subscribes to the NATS trace destination subject and converts each
// infrastructure trace event into OTel spans. Each hop in the event payload becomes one
// point-in-time span (start == end == hop.TS) that is a child of the original publisher span,
// identified via the traceparent header embedded in the event's request headers.
//
// Usage:
//
//	sub, err := otelnats.SubscribeTraceEvents(conn, "nats.trace.events")
//	defer sub.Unsubscribe()
//
// The publisher must set Nats-Trace-Dest on each message (or use WithTraceDestination option).
// Requires NATS server 2.11+.
//
// The subscription handler is picked by the Conn's impl — tracedConn emits hop
// spans, directConn discards events.
func SubscribeTraceEvents(conn *Conn, subject string) (*nats.Subscription, error) {
	return conn.nc.Subscribe(subject, conn.impl.traceEventHandler())
}

// buildTraceEventHandler returns the instrumented closure that decodes a
// NATS trace event payload and emits one OTel span per hop. Lives in this file
// to keep the heavy event-parsing logic next to the type definitions; called
// by tracedConn.traceEventHandler().
func buildTraceEventHandler(tracer trace.Tracer, prop propagation.TextMapPropagator) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var event TraceEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			slog.Warn("otelnats: traceevent unmarshal failed", "error", err, "raw", string(msg.Data))
			return
		}
		slog.Debug("otelnats: traceevent received",
			"server", event.Server.Name,
			"hops", event.Hops,
			"events", len(event.Events),
			"request_headers", event.Request.Headers,
		)
		parentCtx := extractTraceParent(prop, event.Request.Headers)

		serverName := event.Server.Name
		if serverName == "" {
			serverName = event.Server.Host
		}

		for _, hop := range event.Events {
			emitHopSpan(tracer, parentCtx, hop, serverName, event.Server.Version, event.Hops)
		}
	}
}

// extractTraceParent builds an OTel context from the traceparent header embedded in the
// original message's headers (stored in TraceRequestInfo.Headers). Keys are lowercased
// before extraction so both "Traceparent" and "traceparent" are found correctly.
func extractTraceParent(prop propagation.TextMapPropagator, hdrs map[string][]string) context.Context {
	carrier := propagation.MapCarrier{}
	for k, vals := range hdrs {
		if len(vals) > 0 {
			carrier[strings.ToLower(k)] = vals[0]
		}
	}
	return prop.Extract(context.Background(), carrier)
}

// emitHopSpan creates a single point-in-time OTel span for one NATS infrastructure hop.
// The span name follows the pattern "nats.<KIND>.<type>" (e.g. "nats.CLIENT.ingress").
func emitHopSpan(tracer trace.Tracer, parentCtx context.Context, hop TraceHop, serverName, serverVersion string, hops int) {
	spanName := "nats." + hopKindName(hop.Kind) + "." + hopTypeName(hop.Type)

	attrs := []attribute.KeyValue{
		attribute.String("nats.server.name", serverName),
		attribute.String("nats.server.version", serverVersion),
		attribute.String("nats.event.type", hop.Type),
		attribute.String("nats.event.kind", hopKindName(hop.Kind)),
		attribute.Int("nats.hops", hops),
	}
	if hop.Subj != "" {
		attrs = append(attrs, attribute.String("nats.subject", hop.Subj))
	}
	if hop.Acc != "" {
		attrs = append(attrs, attribute.String("nats.account", hop.Acc))
	}
	if hop.Name != "" {
		attrs = append(attrs, attribute.String("nats.connection.name", hop.Name))
	}
	if hop.Hop != "" {
		attrs = append(attrs, attribute.String("nats.hop", hop.Hop))
	}
	if hop.Queue != "" {
		attrs = append(attrs, attribute.String("nats.queue", hop.Queue))
	}
	if hop.Error != "" {
		attrs = append(attrs, attribute.String("nats.error", hop.Error))
	}
	if hop.NoInterest {
		attrs = append(attrs, attribute.Bool("nats.no_interest", true))
	}
	if hop.Stream != "" {
		attrs = append(attrs, attribute.String("nats.stream", hop.Stream))
	}
	if hop.MappedTo != "" {
		attrs = append(attrs, attribute.String("nats.mapped_to", hop.MappedTo))
	}
	if hop.To != "" {
		attrs = append(attrs, attribute.String("nats.to", hop.To))
	}
	if hop.From != "" {
		attrs = append(attrs, attribute.String("nats.from", hop.From))
	}

	// Point-in-time span: start and end are both set to the hop timestamp.
	_, span := tracer.Start(parentCtx, spanName,
		trace.WithTimestamp(hop.TS),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	span.End(trace.WithTimestamp(hop.TS))
}
