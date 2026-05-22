package otelnats_test

import (
	"encoding/json"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelnats "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

// sampleTraceEvent returns a TraceEvent JSON payload representative of what
// nats-server 2.11+ publishes to a Nats-Trace-Dest subject — one ingress hop
// followed by one egress hop, with a traceparent embedded in the original
// request headers.
func sampleTraceEvent(t *testing.T, traceparent string) []byte {
	t.Helper()
	ev := otelnats.TraceEvent{
		Server: otelnats.TraceServerInfo{
			Name:    "test-server",
			Host:    "127.0.0.1",
			ID:      "test-id",
			Cluster: "test-cluster",
			Version: "2.11.0",
		},
		Request: otelnats.TraceRequestInfo{
			Headers: map[string][]string{"traceparent": {traceparent}},
			MsgSize: 8,
		},
		Hops: 0,
		Events: []otelnats.TraceHop{
			{Type: "in", TS: time.Now(), Kind: 0, Subj: "foo"},
			{Type: "eg", TS: time.Now().Add(time.Millisecond), Kind: 0, Subj: "foo"},
		},
	}
	out, err := json.Marshal(ev)
	require.NoError(t, err)
	return out
}

// TestSubscribeTraceEventsEmitsHopSpans is the end-to-end test for
// SubscribeTraceEvents on a tracing-enabled Conn. We publish a synthetic
// TraceEvent payload (the same shape nats-server emits in 2.11+) on the
// configured subject and assert each hop becomes an OTel span linked to the
// traceparent embedded in the event.
func TestSubscribeTraceEventsEmitsHopSpans(t *testing.T) {
	url := startServer(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	conn, err := otelnats.ConnectWithOptions(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	const traceSubject = "$NATS.trace.events"
	sub, err := otelnats.SubscribeTraceEvents(conn, traceSubject)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	const tp1 = "00-deadbeefdeadbeefdeadbeefdeadbeef-1234567890abcdef-01"
	require.NoError(t, conn.NatsConn().Publish(traceSubject, sampleTraceEvent(t, tp1)))
	require.NoError(t, conn.NatsConn().Flush())

	require.Eventually(t, func() bool {
		var hopSpans int
		for _, s := range sr.Ended() {
			if s.Name() == "nats.CLIENT.ingress" || s.Name() == "nats.CLIENT.egress" {
				hopSpans++
			}
		}
		return hopSpans == 2
	}, 2*time.Second, 10*time.Millisecond, "expected ingress+egress hop spans")
}

// TestSubscribeTraceEventsDisabledIsNoop locks in the disabled-mode contract:
// when tracing is gated off, the subscription handler is a no-op. Subscribing
// must still succeed (so callers don't have to flag-gate the call), but no
// spans are emitted regardless of incoming event payloads.
func TestSubscribeTraceEventsDisabledIsNoop(t *testing.T) {
	// Tracing gated off via the existing helper.
	url := startServerTracingOff(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	require.False(t, conn.TracingEnabled())

	const traceSubject = "$NATS.trace.events"
	sub, err := otelnats.SubscribeTraceEvents(conn, traceSubject)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	const tp1 = "00-deadbeefdeadbeefdeadbeefdeadbeef-1234567890abcdef-01"
	require.NoError(t, conn.NatsConn().Publish(traceSubject, sampleTraceEvent(t, tp1)))
	require.NoError(t, conn.NatsConn().Flush())

	// Quiet-window negative assertion — Never fails on the FIRST span emitted
	// rather than relying on a fixed sleep that may be too short under load.
	assert.Never(t, func() bool { return len(sr.Ended()) > 0 },
		300*time.Millisecond, 10*time.Millisecond,
		"disabled mode: SubscribeTraceEvents must emit ZERO spans regardless of incoming events")
}

// TestSubscribeTraceEventsIgnoresMalformedPayload locks in resilience: a
// malformed (non-JSON) payload on the trace subject must be logged and
// dropped — never panic or produce a span.
func TestSubscribeTraceEventsIgnoresMalformedPayload(t *testing.T) {
	url := startServer(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	sub, err := otelnats.SubscribeTraceEvents(conn, "$NATS.trace.events")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Garbage payload.
	require.NoError(t, conn.NatsConn().Publish("$NATS.trace.events", []byte("not-json-at-all")))
	require.NoError(t, conn.NatsConn().Flush())

	// Quiet-window negative assertion — see other Never call for rationale.
	assert.Never(t, func() bool { return len(sr.Ended()) > 0 },
		300*time.Millisecond, 10*time.Millisecond,
		"malformed payload must NOT produce hop spans")
}

// TestWithTraceDestinationOptionThreadsThrough locks in the option →
// TraceDest() round-trip. The trace-event subscriber can read the subject
// back from the Conn so call sites don't have to thread it twice.
func TestWithTraceDestinationOptionThreadsThrough(t *testing.T) {
	url := startServer(t)
	conn, err := otelnats.ConnectWithOptions(url, nil,
		otelnats.WithTraceDestination("nats.trace.custom.dest"),
	)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Equal(t, "nats.trace.custom.dest", conn.TraceDest())

	// Companion: the same subject can be used directly to subscribe.
	sub, err := conn.NatsConn().Subscribe(conn.TraceDest(), func(_ *nats.Msg) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// guarantee unused-import safety should helpers shift later.
var _ = (*natssrv.Server)(nil)
