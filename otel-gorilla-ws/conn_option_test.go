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

// clearWSTracingEnv unsets both tracing env vars for the duration of the
// test, restoring their prior values on cleanup, and resets the process-wide
// gate so the change is observed.
func clearWSTracingEnv(t *testing.T) {
	t.Helper()
	prevGlobal, globalExisted := os.LookupEnv(envGlobalTracingEnabled)
	prevWS, wsExisted := os.LookupEnv(envWSTracingEnabled)
	_ = os.Unsetenv(envGlobalTracingEnabled)
	_ = os.Unsetenv(envWSTracingEnabled)
	t.Cleanup(func() {
		if globalExisted {
			_ = os.Setenv(envGlobalTracingEnabled, prevGlobal)
		} else {
			_ = os.Unsetenv(envGlobalTracingEnabled)
		}
		if wsExisted {
			_ = os.Setenv(envWSTracingEnabled, prevWS)
		} else {
			_ = os.Unsetenv(envWSTracingEnabled)
		}
	})
	resetWSGateForTest()
	t.Cleanup(resetWSGateForTest)
}

// enableWSTracingEnv sets both tracing env vars truthy for the duration of
// the test and resets the process-wide gate so the change is observed.
func enableWSTracingEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "true")
	t.Setenv(envWSTracingEnabled, "true")
	resetWSGateForTest()
	t.Cleanup(resetWSGateForTest)
}

// TestConfigureConn_TracingEnabledFalse_UsesNoopTracer pins that option-off
// (env on) installs a noop tracer — matrix below only asserts featureEnabled.
func TestConfigureConn_TracingEnabledFalse_UsesNoopTracer(t *testing.T) {
	enableWSTracingEnv(t)
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)

	c := &Conn{}
	configureConn(c, resolveConnOptions([]Option{WithTracingEnabled(false)}))
	if c.featureEnabled {
		t.Fatal("expected featureEnabled false")
	}
	_, span := c.tracer.Start(context.Background(), "option-disabled")
	span.End()
	if len(globalRecorder.Ended()) != 0 {
		t.Fatalf("expected no recorded spans (noop tracer), got %d", len(globalRecorder.Ended()))
	}
}

// TestWithTracingEnabled_EnvOptionMatrix pins the full env × option decision
// table for featureEnabled. Option is authoritative in either direction when
// present; when absent, the GLOBAL∧MODULE env gate decides (unset/falsy → off).
func TestWithTracingEnabled_EnvOptionMatrix(t *testing.T) {
	type envState string
	const (
		envUnset envState = "unset"
		envOn    envState = "on"
		envOff   envState = "off" // explicit falsy, not merely unset
	)
	type optState string
	const (
		optAbsent optState = "absent"
		optOn     optState = "on"
		optOff    optState = "off"
	)

	cases := []struct {
		name string
		env  envState
		opt  optState
		want bool
	}{
		{"env unset, option absent → off", envUnset, optAbsent, false},
		{"env unset, option on → on", envUnset, optOn, true},
		{"env unset, option off → off", envUnset, optOff, false},
		{"env on, option absent → on", envOn, optAbsent, true},
		{"env off, option absent → off", envOff, optAbsent, false},
		{"env on, option off → off", envOn, optOff, false},
		{"env off, option on → on", envOff, optOn, true},
		{"env on, option on → on", envOn, optOn, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.env {
			case envUnset:
				clearWSTracingEnv(t)
			case envOn:
				enableWSTracingEnv(t)
			case envOff:
				t.Setenv(envGlobalTracingEnabled, "false")
				t.Setenv(envWSTracingEnabled, "false")
				resetWSGateForTest()
				t.Cleanup(resetWSGateForTest)
			}

			var opts []Option
			switch tc.opt {
			case optOn:
				opts = []Option{WithTracingEnabled(true)}
			case optOff:
				opts = []Option{WithTracingEnabled(false)}
			}

			cfg := resolveConnOptions(opts)
			if got := effectiveFeatureEnabled(cfg); got != tc.want {
				t.Fatalf("effectiveFeatureEnabled = %v, want %v", got, tc.want)
			}

			c := &Conn{}
			configureConn(c, cfg)
			if c.featureEnabled != tc.want {
				t.Fatalf("featureEnabled = %v, want %v", c.featureEnabled, tc.want)
			}
		})
	}
}

// TestNewConn_WithTracingEnabled_OverridesEnvGate is a full-stack proof: the
// env gate resolves to disabled (unset), but NewConn(conn,
// WithTracingEnabled(true)) still produces real spans end-to-end through a
// real WebSocket round trip.
func TestNewConn_WithTracingEnabled_OverridesEnvGate(t *testing.T) {
	clearWSTracingEnv(t)

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

	conn := NewConn(rawConn, WithTracingEnabled(true))
	if !conn.featureEnabled {
		t.Fatal("expected featureEnabled true: option must override the disabled env gate")
	}

	if err := conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if _, _, _, err := conn.ReadMessage(context.Background()); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	if len(sr.Ended()) == 0 {
		t.Fatal("expected recorded spans: WithTracingEnabled(true) must produce real spans despite the disabled env gate")
	}
}

// TestUpgrader_Upgrade_WithTracingEnabled_OverridesEnvGate proves the option
// reaches the server-side Upgrader.Upgrade path too, not just NewConn/Dial.
func TestUpgrader_Upgrade_WithTracingEnabled_OverridesEnvGate(t *testing.T) {
	clearWSTracingEnv(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	up := Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := up.Upgrade(w, r, nil, WithTracingEnabled(true))
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if !conn.featureEnabled {
			t.Error("expected featureEnabled true on the server-side Conn: option must override the disabled env gate")
		}
		if err := conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("server hello")); err != nil {
			t.Errorf("WriteMessage: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	rawConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = rawConn.Close() })
	if _, _, err := rawConn.ReadMessage(); err != nil {
		t.Fatalf("client read: %v", err)
	}

	// The server-side send span ends via `defer span.End()` inside WriteMessage,
	// which runs only after the bytes are already on the wire — so the client's
	// ReadMessage above can return before that span is recorded. Wait for the
	// handler (hence the deferred span.End) to finish before asserting, else
	// sr.Ended() races the server goroutine.
	<-serverDone

	if len(sr.Ended()) == 0 {
		t.Fatal("expected recorded spans on the server side: WithTracingEnabled(true) must reach Upgrader.Upgrade")
	}
}

// TestResolveConnOptions_SkipsNilOptions pins nil-tolerance of the option
// parser: nil entries are skipped, non-nil ones apply.
func TestResolveConnOptions_SkipsNilOptions(t *testing.T) {
	cfg := resolveConnOptions([]Option{nil, WithTracingEnabled(true), nil})
	if cfg.featureEnabled == nil || !*cfg.featureEnabled {
		t.Fatal("expected featureEnabled override to survive surrounding nil options")
	}
}

// TestUpgrader_TracingDisabled_DoesNotNegotiateOTelWS is the wire-corruption
// regression test: a server whose effective tracing is off must not confirm
// otel-ws. Before the negotiation gate, the server echoed otel-ws+json and
// the aware peer enveloped every message, which the feature-off server handed
// to the application un-unwrapped.
func TestUpgrader_TracingDisabled_DoesNotNegotiateOTelWS(t *testing.T) {
	clearWSTracingEnv(t)

	payload := `{"clean":"payload"}`
	up := Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil, WithTracingEnabled(false))
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if conn.tracingEnabled {
			t.Error("server must not negotiate otel-ws when its tracing is off")
		}
		_, mt, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		if string(data) != payload {
			t.Errorf("server received %q, want raw payload %q", data, payload)
		}
		_ = conn.WriteMessage(context.Background(), mt, data)
	}))
	t.Cleanup(srv.Close)

	// otel-ws-aware client with tracing forced ON: it offers otel-ws, but the
	// feature-off server must not confirm, leaving both sides un-enveloped.
	client, _, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil,
		[]string{"json"}, WithTracingEnabled(true))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.tracingEnabled {
		t.Fatal("client must not see an otel-ws confirmation from a tracing-off server")
	}

	if err := client.WriteMessage(context.Background(), websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, _, got, err := client.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("client received %q, want clean round-tripped payload %q", got, payload)
	}
}

// TestDial_TracingDisabled_DoesNotOfferOTelWS is the client-side counterpart:
// a client whose effective tracing is off never offers otel-ws, so an
// otel-ws-aware server (tracing on) neither confirms nor envelopes.
func TestDial_TracingDisabled_DoesNotOfferOTelWS(t *testing.T) {
	enableWSTracingEnv(t)

	payload := `{"clean":"payload"}`
	up := Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil) // env gates on: would confirm otel-ws if offered
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if conn.tracingEnabled {
			t.Error("server saw an otel-ws offer from a tracing-off client")
		}
		_, mt, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(context.Background(), mt, data)
	}))
	t.Cleanup(srv.Close)

	client, _, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil,
		[]string{"json"}, WithTracingEnabled(false))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.tracingEnabled {
		t.Fatal("tracing-off client must not negotiate otel-ws")
	}
	if got := client.Subprotocol(); got != "json" {
		t.Fatalf("negotiated app subprotocol = %q, want %q", got, "json")
	}

	if err := client.WriteMessage(context.Background(), websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, _, got, err := client.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("client received %q, want clean round-tripped payload %q", got, payload)
	}
}

// TestUpgrader_FeatureOff_StripsOTelFromCallerResponseHeader is a
// defense-in-depth regression test: gorilla reads Sec-Websocket-Protocol
// straight from a caller-supplied responseHeader whenever Inner.Subprotocols
// is nil (true here since both Subprotocols and AppSubprotocols are unset),
// bypassing this package's own negotiation logic entirely. If a caller's
// responseHeader happens to carry an otel-ws token, a feature-off Upgrade
// must still strip it before calling into gorilla — otherwise gorilla echoes
// it back verbatim and the client believes otel-ws was negotiated even
// though this server's Conn has tracing disabled.
func TestUpgrader_FeatureOff_StripsOTelFromCallerResponseHeader(t *testing.T) {
	clearWSTracingEnv(t)

	up := Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseHeader := http.Header{"Sec-Websocket-Protocol": {"otel-ws+json"}}
		conn, err := up.Upgrade(w, r, responseHeader, WithTracingEnabled(false))
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if conn.tracingEnabled {
			t.Error("server must not negotiate otel-ws when its tracing is off, even via a caller-supplied responseHeader")
		}
	}))
	t.Cleanup(srv.Close)

	// Raw gorilla dial (not this package's Dial) to observe the true
	// wire-level response, bypassing any client-side stripping.
	rawConn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = rawConn.Close() })

	if got := rawConn.Subprotocol(); strings.Contains(got, "otel-ws") {
		t.Fatalf("client observed negotiated subprotocol %q, want no otel-ws token: the caller responseHeader must be stripped when the feature is off", got)
	}
	if got := resp.Header.Get("Sec-Websocket-Protocol"); strings.Contains(got, "otel-ws") {
		t.Fatalf("response header Sec-Websocket-Protocol = %q, want no otel-ws token", got)
	}
}

// TestDial_FeatureOff_StripsOTelFromCallerRequestHeader is the client-side
// counterpart: gorilla's Dialer sends a caller-supplied requestHeader's
// Sec-Websocket-Protocol value verbatim whenever Dialer.Subprotocols is
// empty (true here since subprotocols is nil and otelInjected therefore
// stays false), bypassing this package's own negotiation logic entirely. If
// a caller's requestHeader happens to carry an otel-ws token, a
// feature-off/no-subprotocols Dial must still strip it — otherwise an
// otel-ws-aware server confirms and envelopes every message, which this
// client's Conn (tracingEnabled false) never unwraps.
func TestDial_FeatureOff_StripsOTelFromCallerRequestHeader(t *testing.T) {
	enableWSTracingEnv(t)

	payload := `{"clean":"payload"}`
	up := Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// env gates on: this server WOULD confirm otel-ws if it were offered.
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if conn.tracingEnabled {
			t.Error("server must not see an otel-ws offer smuggled through a feature-off client's requestHeader")
		}
		_, mt, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		if string(data) != payload {
			t.Errorf("server received %q, want raw payload %q", data, payload)
		}
		_ = conn.WriteMessage(context.Background(), mt, data)
	}))
	t.Cleanup(srv.Close)

	requestHeader := http.Header{"Sec-Websocket-Protocol": {"otel-ws"}}
	client, resp, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), requestHeader, nil, WithTracingEnabled(false))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if client.tracingEnabled {
		t.Fatal("client must not have tracing enabled: feature is off")
	}
	if got := resp.Header.Get("Sec-Websocket-Protocol"); strings.Contains(got, "otel-ws") {
		t.Fatalf("response header Sec-Websocket-Protocol = %q, want no otel-ws confirmation: the caller requestHeader must be stripped before dialing", got)
	}

	if err := client.WriteMessage(context.Background(), websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, _, got, err := client.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("client received %q, want clean round-tripped payload %q", got, payload)
	}
}
