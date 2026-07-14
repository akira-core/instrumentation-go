package otelgorillaws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func resetWSGateForTest() {
	wsGate.ResetForTest()
}

func TestWSTracingEnabled_DefaultFalse(t *testing.T) {
	prev, existed := os.LookupEnv(envWSTracingEnabled)
	_ = os.Unsetenv(envWSTracingEnabled)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envWSTracingEnabled, prev)
		} else {
			_ = os.Unsetenv(envWSTracingEnabled)
		}
	})
	resetWSGateForTest()
	t.Cleanup(resetWSGateForTest)
	if wsTracingEnabled() {
		t.Fatal("expected tracing disabled when env var is unset")
	}
}

func TestWSTracingEnabled_EmptyStringIsEnabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "")
	t.Setenv(envWSTracingEnabled, "")
	resetWSGateForTest()
	t.Cleanup(resetWSGateForTest)
	if !wsTracingEnabled() {
		t.Fatal("expected empty string to mean enabled")
	}
}

func TestWSTracingEnabled_FalseTokens(t *testing.T) {
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Setenv(envWSTracingEnabled, v)
		resetWSGateForTest()
		if wsTracingEnabled() {
			t.Fatalf("expected disabled for value %q", v)
		}
	}
	t.Cleanup(resetWSGateForTest)
}

func TestWSTracingEnabled_GlobalOffOverridesModule(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envWSTracingEnabled, "true")
	resetWSGateForTest()
	t.Cleanup(resetWSGateForTest)
	if wsTracingEnabled() {
		t.Fatal("expected global flag to disable ws tracing")
	}
}

// TestFeatureDisabled_PassesThroughToNativeConn covers the disabled-mode
// invariant at the Conn level: with featureEnabled false (env gate off),
// WriteMessage/ReadMessage must delegate straight to *websocket.Conn — no
// span, no JSON envelope, no propagator inject/extract.
func TestFeatureDisabled_PassesThroughToNativeConn(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envWSTracingEnabled, "false")
	resetWSGateForTest()
	t.Cleanup(resetWSGateForTest)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		raw, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer raw.Close()
		mt, data, err := raw.ReadMessage()
		if err != nil {
			return
		}
		_ = raw.WriteMessage(mt, data)
	}))
	t.Cleanup(srv.Close)

	rawConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = rawConn.Close() })

	conn := newConn(rawConn, false)
	if conn.featureEnabled {
		t.Fatal("expected featureEnabled false")
	}

	payload := []byte(`{"x":1}`)
	if err := conn.WriteMessage(context.Background(), websocket.TextMessage, payload); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	_, _, got, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected passthrough payload %q, got %q — envelope must not be applied when tracing is disabled", payload, got)
	}

	if len(sr.Ended()) != 0 {
		t.Fatalf("expected zero spans when tracing is disabled, got %d", len(sr.Ended()))
	}
}
