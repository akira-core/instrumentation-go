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

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// clearWSTracingEnv unsets every switch for the duration of the test, which is
// also the state in which WithTracingEnabled is the deciding rung.
func clearWSTracingEnv(t *testing.T) {
	t.Helper()
	clearWSFlagEnv(t)
}

// optionDecidesEnv leaves the module variable UNSET so the option decides.
//
// This replaces the old moduleEnvOnly, and the change is the point: the option
// is now BELOW its environment variable, so a test that sets the variable and
// then passes a contradicting option is asserting the variable's value, not the
// option's.
func optionDecidesEnv(t *testing.T) {
	t.Helper()
	clearWSFlagEnv(t)
}

// moduleEnvOn turns this module's variable on and leaves the master unset.
func moduleEnvOn(t *testing.T) {
	t.Helper()
	clearWSFlagEnv(t)
	t.Setenv(envWSTracingEnabled, "true")
}

// TestConfigureConn_TracingEnabledFalse_UsesNoopTracer pins that option-off
// (env on) installs a noop tracer — matrix below only asserts featureEnabled.
func TestConfigureConn_TracingEnabledFalse_UsesNoopTracer(t *testing.T) {
	optionDecidesEnv(t)
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)

	c := &Conn{}
	cfg := resolveConnOptions([]Option{WithTracingEnabled(false)})
	gate, err := resolveGates(cfg)
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	configureConn(c, cfg, gate)
	if c.featureEnabled() {
		t.Fatal("expected featureEnabled false")
	}
	_, span := c.tracer.Start(context.Background(), "option-disabled")
	span.End()
	if len(globalRecorder.Ended()) != 0 {
		t.Fatalf("expected no recorded spans (noop tracer), got %d", len(globalRecorder.Ended()))
	}
}

// TestTracingTiers_EnvOptionMatrix pins how the ladder and the master switch
// compose, with no relay configured so the local answer is final.
//
// Two rows reverse earlier releases: the module variable alone is now enough
// (the master defaults to enabled), and the environment variable beats the
// option (so there is no conflict row — supplying both is ordinary
// configuration).
func TestTracingTiers_EnvOptionMatrix(t *testing.T) {
	type v struct {
		set   bool
		value string
	}
	unset := v{}
	on := v{set: true, value: "1"}
	off := v{set: true, value: "false"}

	cases := []struct {
		name   string
		master v
		module v
		option *bool
		want   bool
	}{
		{name: "nothing set", master: unset, module: unset, want: false},

		{name: "master on alone", master: on, module: unset, want: false},
		{name: "master on, module on", master: on, module: on, want: true},
		{name: "master on, module off", master: on, module: off, want: false},
		{name: "master off, module on", master: off, module: on, want: false},
		{name: "master off, option on", master: off, module: unset, option: ptr(true), want: false},

		{name: "module on alone", master: unset, module: on, want: true},

		{name: "option on, variable unset", master: unset, module: unset, option: ptr(true), want: true},
		{name: "option off, variable unset", master: unset, module: unset, option: ptr(false), want: false},
		{name: "variable off beats option on", master: unset, module: off, option: ptr(true), want: false},
		{name: "variable on beats option off", master: unset, module: on, option: ptr(false), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, otelflags.EnvFlagsEndpoint, false, "")
			setOrUnset(t, otelflags.EnvGlobalTracing, tc.master.set, tc.master.value)
			setOrUnset(t, envWSTracingEnabled, tc.module.set, tc.module.value)

			var opts []Option
			if tc.option != nil {
				opts = []Option{WithTracingEnabled(*tc.option)}
			}
			cfg := resolveConnOptions(opts)

			gate, err := resolveGates(cfg)
			if err != nil {
				t.Fatalf("resolveGates: %v", err)
			}
			if got := gate.tracing(); got != tc.want {
				t.Fatalf("gate.tracing() = %v, want %v", got, tc.want)
			}

			c := &Conn{}
			configureConn(c, cfg, gate)
			if c.featureEnabled() != tc.want {
				t.Fatalf("featureEnabled = %v, want %v", c.featureEnabled(), tc.want)
			}
		})
	}
}

func ptr(b bool) *bool { return &b }

// setOrUnset makes an environment variable present with value, or absent,
// restoring the previous state when the test ends.
func setOrUnset(t *testing.T, name string, set bool, value string) {
	t.Helper()
	if set {
		t.Setenv(name, value)
		return
	}
	prev, existed := os.LookupEnv(name)
	_ = os.Unsetenv(name)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, prev)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

// TestNewConn_WithTracingEnabled_Traces is a full-stack proof that the option is
// usable on its own: with every environment variable unset, WithTracingEnabled
// (true) is the deciding rung and real spans reach a real WebSocket round trip.
func TestNewConn_WithTracingEnabled_Traces(t *testing.T) {
	optionDecidesEnv(t)

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

	conn, errNewConn := NewConn(rawConn, WithTracingEnabled(true))
	if errNewConn != nil {
		t.Fatalf("NewConn: %v", errNewConn)
	}
	if !conn.featureEnabled() {
		t.Fatal("expected featureEnabled true: the option decides when its variable is unset")
	}

	if err := conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if _, _, _, err := conn.ReadMessage(context.Background()); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	if len(sr.Ended()) == 0 {
		t.Fatal("expected recorded spans: WithTracingEnabled(true) must produce real spans with the environment silent")
	}
}

// TestUpgrader_Upgrade_WithTracingEnabled_OverridesEnvGate proves the option
// reaches the server-side Upgrader.Upgrade path too, not just NewConn/Dial.
func TestUpgrader_Upgrade_WithTracingEnabled_SuppliesGate1(t *testing.T) {
	optionDecidesEnv(t)

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
		if !conn.featureEnabled() {
			t.Error("expected featureEnabled true on the server-side Conn: the option must reach Upgrade")
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
	// The module variable is left unset so WithTracingEnabled(false) is the
	// deciding rung for the client. Setting it truthy would beat the option and
	// this test would assert the wrong thing.
	optionDecidesEnv(t)

	payload := `{"clean":"payload"}`
	up := Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server passes the option ON, so it would confirm otel-ws if offered.
		conn, err := up.Upgrade(w, r, nil, WithTracingEnabled(true))
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
	optionDecidesEnv(t)

	payload := `{"clean":"payload"}`
	up := Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This server WOULD confirm otel-ws if it were offered.
		conn, err := up.Upgrade(w, r, nil, WithTracingEnabled(true))
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

// mustGate resolves the gate state for a parsed option set, failing the test on
// an unreadable configuration.
func mustGate(t *testing.T, cfg connOptions) gateState {
	t.Helper()
	gate, err := resolveGates(cfg)
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	return gate
}
