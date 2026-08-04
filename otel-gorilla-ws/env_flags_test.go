package otelgorillaws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// wsTracing resolves the module's effective tracing state from everything except
// a relay, which these tests deliberately leave absent. The truthiness rules
// themselves live in otel-flags and are tested there; these cases pin how this
// module composes the ladder and the master switch.
func wsTracing(t *testing.T) bool {
	t.Helper()
	gate, err := resolveGates(resolveConnOptions(nil))
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	return gate.tracing()
}

// clearWSFlagEnv clears every variable these tests touch, so a case starts from
// "the deployment expressed no opinion".
func clearWSFlagEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		otelflags.EnvGlobalTracing,
		envWSTracingEnabled,
		otelflags.EnvFlagsEndpoint,
	} {
		prev, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, prev)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

func TestWSTracing_NothingConfiguredIsOff(t *testing.T) {
	clearWSFlagEnv(t)
	if wsTracing(t) {
		t.Fatal("tracing on with nothing configured; the module default must be false")
	}
}

// TestWSTracing_ModuleVariableAloneEnables is the behaviour change against
// 0.7.0: the module variable used to be inert unless the global one was set.
func TestWSTracing_ModuleVariableAloneEnables(t *testing.T) {
	clearWSFlagEnv(t)
	t.Setenv(envWSTracingEnabled, "true")
	if !wsTracing(t) {
		t.Fatal("tracing off with the module variable truthy; the master defaults to enabled")
	}
}

func TestWSTracing_MasterVariableAloneDoesNotEnable(t *testing.T) {
	clearWSFlagEnv(t)
	t.Setenv(otelflags.EnvGlobalTracing, "true")
	if wsTracing(t) {
		t.Fatal("tracing on with only the master variable set; the master is a veto, not an enabler")
	}
}

func TestWSTracing_FalsyTokens(t *testing.T) {
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Run(v, func(t *testing.T) {
			clearWSFlagEnv(t)
			t.Setenv(envWSTracingEnabled, v)
			if wsTracing(t) {
				t.Fatalf("tracing on for value %q", v)
			}
		})
	}
}

func TestWSTracing_MasterVetoOverridesModule(t *testing.T) {
	clearWSFlagEnv(t)
	t.Setenv(otelflags.EnvGlobalTracing, "false")
	t.Setenv(envWSTracingEnabled, "true")
	if wsTracing(t) {
		t.Fatal("the master veto did not stop a module its own variable enabled")
	}
}

// TestWSTracing_InvalidValueFailsConstruction covers the BREAKING change that
// can stop a process from starting; the empty string is included deliberately.
func TestWSTracing_InvalidValueFailsConstruction(t *testing.T) {
	for _, v := range []string{"", "enabled", "2", "y"} {
		t.Run("value="+v, func(t *testing.T) {
			clearWSFlagEnv(t)
			t.Setenv(envWSTracingEnabled, v)

			if _, err := resolveGates(resolveConnOptions(nil)); !errors.Is(err, otelflags.ErrInvalidFlagValue) {
				t.Fatalf("resolveGates err = %v, want ErrInvalidFlagValue", err)
			}
		})
	}
}

// TestWSTracing_EnvironmentBeatsOption is the ordering that reverses 0.7.0.
func TestWSTracing_EnvironmentBeatsOption(t *testing.T) {
	clearWSFlagEnv(t)
	t.Setenv(envWSTracingEnabled, "false")

	gate, err := resolveGates(resolveConnOptions([]Option{WithTracingEnabled(true)}))
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	if gate.tracing() {
		t.Fatal("the option won over the environment variable; the variable outranks it")
	}
}

// TestFeatureDisabled_PassesThroughToNativeConn covers the disabled-mode
// invariant at the Conn level: with featureEnabled false (env gate off),
// WriteMessage/ReadMessage must delegate straight to *websocket.Conn — no
// span, no JSON envelope, no propagator inject/extract.
func TestFeatureDisabled_PassesThroughToNativeConn(t *testing.T) {
	clearWSFlagEnv(t)
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
