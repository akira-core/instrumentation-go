package integration_test

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

	otelgorillaws "github.com/akira-core/instrumentation-go/otel-gorilla-ws"
)

func newIntegrationTP(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
	return recorder
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestIntegration_Handshake_OtelWSOnlyNoAppProtocol verifies that when the
// client offers only "otel-ws" (no app subprotocol — the JS otel-rxjs-ws
// default), the server responds with bare "otel-ws" and NOT "otel-ws+".
func TestIntegration_Handshake_OtelWSOnlyNoAppProtocol(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	upgrader := &otelgorillaws.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
		// No AppSubprotocols: accept-any semantics, negotiated will be "".
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		assert.Equal(t, "", conn.Subprotocol()) // no app proto negotiated

		ctx, typ, payload, err := conn.ReadMessage(context.Background())
		require.NoError(t, err)
		_ = conn.WriteMessage(ctx, typ, payload)
	}))
	defer srv.Close()

	// Simulate JS client: offer only "otel-ws" with no app subprotocol.
	rawDialer := &websocket.Dialer{Subprotocols: []string{"otel-ws"}}
	rawConn, _, err := rawDialer.Dial(wsURL(srv), nil)
	require.NoError(t, err)
	defer rawConn.Close()

	// Server must respond with bare "otel-ws", NOT "otel-ws+".
	assert.Equal(t, "otel-ws", rawConn.Subprotocol())

	// Send a message and receive the echoed response. Because the server Conn has
	// tracingEnabled=true it wraps the outgoing message in the otel wire envelope,
	// so the raw client sees {"header":...,"data":...} — not the original payload.
	// We only verify the round-trip completes without error.
	msg := []byte(`{"hello":"world"}`)
	require.NoError(t, rawConn.WriteMessage(websocket.TextMessage, msg))
	_, got, err := rawConn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(got), `"data"`, "server wraps response in otel wire envelope")
}

func TestIntegration_Handshake_SubprotocolNegotiation(t *testing.T) {
	recorder := newIntegrationTP(t)

	upgrader := &otelgorillaws.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		assert.Equal(t, "json", conn.Subprotocol())

		ctx, typ, payload, err := conn.ReadMessage(context.Background())
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(ctx, typ, payload))
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "json", conn.Subprotocol())

	input := []byte(`{"kind":"handshake"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, input))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, input, got)

	assert.GreaterOrEqual(t, len(recorder.Ended()), 2)
}
