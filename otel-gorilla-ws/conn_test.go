package otelgorillaws_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	otelgorillaws "github.com/Marz32onE/instrumentation-go/otel-gorilla-ws"
)

// plainUpgrader is a bare gorilla upgrader used to simulate plain WebSocket servers.
var plainUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// newTestTP creates a TracerProvider backed by an in-memory SpanRecorder,
// sets it as the global provider, and registers t.Cleanup to shut it down.
func newTestTP(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
	return sr
}

// attrsToMap converts span attributes to map[string]any for easy lookup.
func attrsToMap(s sdktrace.ReadOnlySpan) map[string]any {
	m := make(map[string]any, len(s.Attributes()))
	for _, a := range s.Attributes() {
		m[string(a.Key)] = a.Value.AsInterface()
	}
	return m
}

// otelUpgrader returns an OTelUpgrader for use in server-side tests.
func otelUpgrader(appProtos []string) *otelgorillaws.Upgrader {
	return &otelgorillaws.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: appProtos,
	}
}

// wsURL converts an httptest server URL to a WebSocket URL.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// ── Existing round-trip tests (updated to use spec-compliant API) ─────────────

func TestRoundTrip(t *testing.T) {
	sr := newTestTP(t)

	up := otelUpgrader([]string{"json"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		ctx, typ, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		_ = conn.WriteMessage(ctx, typ, data)
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err, "dial")
	defer conn.Close()

	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(`{"x":1}`)))

	_, _, _, err = conn.ReadMessage(context.Background())
	require.NoError(t, err)

	assert.NotEmpty(t, sr.Ended(), "expected spans to be recorded")
}

func TestSpanAttributes(t *testing.T) {
	sr := newTestTP(t)

	up := otelUpgrader([]string{"json"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		ctx, typ, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(ctx, typ, data)
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	payload := []byte(`{"msg":"hello"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))

	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	var sendSpans, recvSpans []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		switch s.Name() {
		case "websocket.send":
			sendSpans = append(sendSpans, s)
		case "websocket.receive":
			recvSpans = append(recvSpans, s)
		}
	}

	// Client WriteMessage + server echo WriteMessage = 2 send spans
	require.NotEmpty(t, sendSpans, "expected websocket.send spans")
	assert.Equal(t, oteltrace.SpanKindProducer, sendSpans[0].SpanKind())
	sendAttrs := attrsToMap(sendSpans[0])
	assert.Equal(t, int64(websocket.TextMessage), sendAttrs["websocket.message.type"])
	assert.Equal(t, int64(len(payload)), sendAttrs["messaging.message.body.size"])

	// Server ReadMessage + client ReadMessage = 2 receive spans
	require.NotEmpty(t, recvSpans, "expected websocket.receive spans")
	assert.Equal(t, oteltrace.SpanKindConsumer, recvSpans[0].SpanKind())

	// Client's receive span (last one) must link back to sender span context
	lastRecv := recvSpans[len(recvSpans)-1]
	require.NotEmpty(t, lastRecv.Links(), "receive span must link to sender span context")
}

// ── Client-side scenario tests ────────────────────────────────────────────────

// TestDial_ScenarioC: client proposes "otel-ws,json" but the plain server returns
// "json" (no otel-ws+ prefix). Propagation must be disabled; payload must pass through
// unchanged while send/receive spans are still recorded.
func TestDial_ScenarioC(t *testing.T) {
	sr := newTestTP(t)

	// Plain server that accepts "json" — does NOT understand otel-ws.
	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{
			CheckOrigin:  func(r *http.Request) bool { return true },
			Subprotocols: []string{"json"},
		}
		raw, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer raw.Close()
		// Echo the raw payload back unchanged.
		_, data, err := raw.ReadMessage()
		if err != nil {
			return
		}
		_ = raw.WriteMessage(websocket.TextMessage, data)
	}))
	defer plainSrv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(plainSrv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	// Server negotiated "json" (no otel-ws+) → tracing must be disabled.
	assert.Equal(t, "json", conn.Subprotocol(), "server should have returned json")

	payload := []byte(`{"hello":"world"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))

	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)

	// Passthrough: no envelope wrapping, payload returned as-is.
	assert.Equal(t, payload, got, "payload must not be wrapped in tracing envelope")
	assert.Len(t, sr.Ended(), 2, "passthrough mode must still create send/receive spans")
}

// TestDial_ScenarioD: client proposes "otel-ws,json" but the server accepts no
// subprotocol (returns ""). Dial must succeed and the Conn must operate in
// passthrough mode (payload unchanged, spans still emitted).
func TestDial_ScenarioD(t *testing.T) {
	sr := newTestTP(t)

	// Server that accepts the WebSocket upgrade but does not select any subprotocol.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
			// Subprotocols is nil → gorilla returns "" → no Sec-WebSocket-Protocol sent.
		}
		raw, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer raw.Close()
		_, data, err := raw.ReadMessage()
		if err != nil {
			return
		}
		_ = raw.WriteMessage(websocket.TextMessage, data)
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err, "Dial must succeed even when server returns no subprotocol")
	defer conn.Close()

	assert.Empty(t, conn.Subprotocol(), "no subprotocol negotiated")

	payload := []byte(`{"d":"passthrough"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))

	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)

	assert.Equal(t, payload, got, "payload must not be wrapped in passthrough mode")
	assert.Len(t, sr.Ended(), 2, "passthrough mode must still create send/receive spans")
}

// TestDial_ScenarioE: client proposes no subprotocols. No otel-ws injection occurs
// and the returned Conn operates in passthrough mode (no envelope wrapping, spans still emitted).
func TestDial_ScenarioE(t *testing.T) {
	sr := newTestTP(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := plainUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer raw.Close()
		_, data, err := raw.ReadMessage()
		if err != nil {
			return
		}
		_ = raw.WriteMessage(websocket.TextMessage, data)
	}))
	defer srv.Close()

	// nil subprotocols → Scenario E
	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, nil)
	require.NoError(t, err)
	defer conn.Close()

	payload := []byte(`{"e":"passthrough"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))

	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)

	assert.Equal(t, payload, got, "payload must not be wrapped in Scenario E")
	assert.Len(t, sr.Ended(), 2, "Scenario E must still create send/receive spans")
}

// ── Server-side scenario tests ────────────────────────────────────────────────

// TestUpgrader_ScenarioF: plain client sends no subprotocols. OTelUpgrader must
// accept the connection in passthrough mode (tracing disabled, spans still emitted).
func TestUpgrader_ScenarioF(t *testing.T) {
	sr := newTestTP(t)

	up := otelUpgrader([]string{"json"})
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_, _, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, data)
	}))
	defer srv.Close()

	// Plain client — sends no subprotocols.
	dialer := websocket.Dialer{}
	raw, _, err := dialer.DialContext(context.Background(), wsURL(srv), nil)
	require.NoError(t, err)
	defer raw.Close()

	payload := []byte(`{"f":"passthrough"}`)
	require.NoError(t, raw.WriteMessage(websocket.TextMessage, payload))

	_, got, err := raw.ReadMessage()
	require.NoError(t, err)

	assert.Equal(t, payload, got, "server must echo payload unchanged in Scenario F")
	<-handlerDone
	assert.Len(t, sr.Ended(), 2, "Scenario F must still create send/receive spans")
}

// TestUpgrader_ScenarioG: otel-ws client sends "otel-ws,json". OTelUpgrader must
// respond with "otel-ws+json" and enable tracing on both sides.
func TestUpgrader_ScenarioG(t *testing.T) {
	sr := newTestTP(t)

	up := otelUpgrader([]string{"json"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		assert.Equal(t, "json", conn.Subprotocol(), "server conn app subprotocol")

		ctx, typ, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(ctx, typ, data)
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "json", conn.Subprotocol(), "client conn app subprotocol")

	payload := []byte(`{"g":"tracing"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))

	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)

	assert.Equal(t, payload, got, "payload must round-trip correctly in Scenario G")
	assert.NotEmpty(t, sr.Ended(), "spans must be recorded in Scenario G")
}

// TestUpgrader_ScenarioG_FromPrefixedClientToken verifies server-side parsing when
// a client directly proposes "otel-ws+json" instead of separate "otel-ws,json".
func TestUpgrader_ScenarioG_FromPrefixedClientToken(t *testing.T) {
	sr := newTestTP(t)

	up := otelUpgrader([]string{"json"})
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		assert.Equal(t, "json", conn.Subprotocol(), "server conn app subprotocol")

		ctx, typ, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(ctx, typ, data)
	}))
	defer srv.Close()

	dialer := websocket.Dialer{Subprotocols: []string{"otel-ws+json"}}
	raw, _, err := dialer.DialContext(context.Background(), wsURL(srv), nil)
	require.NoError(t, err)
	defer raw.Close()

	assert.Equal(t, "otel-ws+json", raw.Subprotocol(), "client sees raw prefixed subprotocol")

	payload := []byte(`{"gp":"prefixed-token"}`)
	require.NoError(t, raw.WriteMessage(websocket.TextMessage, payload))

	_, got, err := raw.ReadMessage()
	require.NoError(t, err)
	assert.NotEqual(t, payload, got, "plain client should receive propagation envelope")
	assert.Contains(t, string(got), `"header"`, "envelope must contain tracing header field")
	assert.Contains(t, string(got), `"data"`, "envelope must contain tracing data field")
	<-handlerDone
	assert.Len(t, sr.Ended(), 2, "server read/write must create spans")
}

// TestUpgrader_ScenarioH: plain client sends "json" (no otel-ws). OTelUpgrader must
// accept with "json" and operate in passthrough mode (tracing disabled, spans still emitted).
func TestUpgrader_ScenarioH(t *testing.T) {
	sr := newTestTP(t)

	up := otelUpgrader([]string{"json"})
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		assert.Equal(t, "json", conn.Subprotocol(), "server conn subprotocol in Scenario H")

		_, _, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, data)
	}))
	defer srv.Close()

	// Plain client that requests "json" only — no otel-ws.
	dialer := websocket.Dialer{Subprotocols: []string{"json"}}
	raw, _, err := dialer.DialContext(context.Background(), wsURL(srv), nil)
	require.NoError(t, err)
	defer raw.Close()

	assert.Equal(t, "json", raw.Subprotocol(), "plain client sees json subprotocol")

	payload := []byte(`{"h":"passthrough"}`)
	require.NoError(t, raw.WriteMessage(websocket.TextMessage, payload))

	_, got, err := raw.ReadMessage()
	require.NoError(t, err)

	assert.Equal(t, payload, got, "server must echo payload unchanged in Scenario H")
	<-handlerDone
	assert.Len(t, sr.Ended(), 2, "Scenario H must still create send/receive spans")
}
