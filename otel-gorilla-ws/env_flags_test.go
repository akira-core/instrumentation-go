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

// wsCapable resolves the module's static capability from the environment, which
// is what decides whether this connection could ever trace. The truthiness rules
// themselves live in internal/flags and are tested there; these cases pin how
// this module composes the two tiers.
func wsCapable(t *testing.T) bool {
	t.Helper()
	capable, err := effectiveCapability(resolveConnOptions(nil))
	if err != nil {
		t.Fatalf("effectiveCapability: %v", err)
	}
	return capable
}

func TestWSCapability_DefaultFalse(t *testing.T) {
	prev, existed := os.LookupEnv(envWSTracingEnabled)
	_ = os.Unsetenv(envWSTracingEnabled)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envWSTracingEnabled, prev)
		} else {
			_ = os.Unsetenv(envWSTracingEnabled)
		}
	})
	if wsCapable(t) {
		t.Fatal("expected tracing disabled when the module env var is unset")
	}
}

// TestWSCapability_EmptyStringIsDisabled is the BREAKING truthiness change:
// `export VAR=` used to open the gate and now closes it.
func TestWSCapability_EmptyStringIsDisabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "")
	t.Setenv(envWSTracingEnabled, "")
	if wsCapable(t) {
		t.Fatal("expected an empty string to mean disabled")
	}
}

func TestWSCapability_FalseTokens(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Setenv(envWSTracingEnabled, v)
		if wsCapable(t) {
			t.Fatalf("expected disabled for value %q", v)
		}
	}
}

func TestWSCapability_GlobalOffOverridesModule(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envWSTracingEnabled, "true")
	if wsCapable(t) {
		t.Fatal("expected the global kill switch to disable ws tracing")
	}
}

func TestWSCapability_RequiresBothTiers(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "1")
	if !wsCapable(t) {
		t.Fatal("expected capability with both tiers on")
	}
}

// TestFeatureDisabled_PassesThroughToNativeConn covers the disabled-mode
// invariant at the Conn level: with featureEnabled false (env gate off),
// WriteMessage/ReadMessage must delegate straight to *websocket.Conn — no
// span, no JSON envelope, no propagator inject/extract.
func TestFeatureDisabled_PassesThroughToNativeConn(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envWSTracingEnabled, "false")
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

	conn, err := newConn(rawConn, false)
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	if conn.featureEnabled() {
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
