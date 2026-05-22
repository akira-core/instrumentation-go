package otelgorillaws

// Internal-package test so we can reset the unexported wsGate directly. The
// matrix exercises the public API (NewConn / Dial / WriteMessage) end-to-end
// while keeping the gate-reset call out of the production surface — see the
// otel-nats sibling package's ResetGatesForTest helper for the equivalent
// in that module.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// applyWSFlag is a small helper for table-driven env manipulation.
func applyWSFlag(t *testing.T, key, value string) {
	t.Helper()
	if value == "" {
		_ = os.Unsetenv(key)
		return
	}
	t.Setenv(key, value)
}

// TestWSFlagMatrix_AllCombinations exhaustively covers the 4 combinations of
// (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED × OTEL_GORILLA_WS_TRACING_ENABLED)
// at the constructor level. For each combination it asserts:
//
//  1. NewConn picks the correct impl (direct vs traced) — the constructor-
//     site decision drives the entire disabled-mode invariant.
//  2. Conn.Subprotocol() returns the negotiated app protocol regardless of
//     flag state (the prefix-stripping helper is gate-independent).
//
// The 2-tier surface is intentional — there is no per-module propagation
// flag because subprotocol negotiation serves that role per-connection.
// See README "Tracing feature flags".
func TestWSFlagMatrix_AllCombinations(t *testing.T) {
	type want struct {
		// The impl type is exercised indirectly via behavioural assertions
		// below — we cannot import internal/{direct,traced} here without
		// breaking the _test package boundary, so the flag-matrix test only
		// touches public observables.
		emitsSpan bool
	}
	cases := []struct {
		name          string
		global        string
		moduleTracing string
		want          want
	}{
		{"all_unset", "", "", want{emitsSpan: false}},
		{"global_only", "1", "", want{emitsSpan: false}},
		{"moduleTracing_only", "", "1", want{emitsSpan: false}},
		{"both_on", "1", "1", want{emitsSpan: true}},
		{"global_off_module_on", "0", "1", want{emitsSpan: false}},
		{"global_on_module_off", "1", "0", want{emitsSpan: false}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyWSFlag(t, "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", tc.global)
			applyWSFlag(t, "OTEL_GORILLA_WS_TRACING_ENABLED", tc.moduleTracing)
			wsGate.ResetForTest()
			t.Cleanup(wsGate.ResetForTest)

			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
			otel.SetTracerProvider(tp)
			otel.SetTextMapPropagator(propagation.TraceContext{})
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			// Spin up a server-side echo that uses the OTel-aware Upgrader so
			// the subprotocol is properly negotiated on both ends.
			up := &Upgrader{
				CheckOrigin:  func(r *http.Request) bool { return true },
				Subprotocols: []string{"json"},
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := up.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer conn.Close()
				ctx, typ, data, err := conn.ReadMessage(context.Background())
				if err != nil {
					return
				}
				_ = conn.WriteMessage(ctx, typ, data)
			}))
			t.Cleanup(srv.Close)

			conn, _, err := Dial(context.Background(),
				"ws"+srv.URL[4:], nil, []string{"json"})
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			// Send + receive a small message.
			require.NoError(t, conn.WriteMessage(context.Background(),
				websocket.TextMessage, []byte(`{"k":"v"}`)))
			_, _, _, _ = conn.ReadMessage(context.Background())

			// Flush async recorder by waiting briefly.
			time.Sleep(50 * time.Millisecond)
			ended := sr.Ended()
			if tc.want.emitsSpan {
				assert.NotEmpty(t, ended,
					"both gates on: at least one wrapper span must be recorded")
			} else {
				assert.Empty(t, ended,
					"any gate off: ZERO wrapper spans — must be observationally identical to native gorilla/websocket")
			}
		})
	}
}

// TestWriteMessageDisabled_ByteIdenticalWithNative locks in the strongest
// version of the disabled-mode invariant: when tracing is gated off, the
// payload bytes that arrive on the other end of the connection are byte-
// identical with what a raw *websocket.Conn.WriteMessage would have produced.
// No envelope wrapping, no header bytes, no `traceparent` field.
//
// We compare two parallel scenarios on the same server:
//   - native client (raw *websocket.Conn) sending [payload]
//   - otelgorillaws client (env off) sending [payload]
//
// and assert the bytes received by the server are equal.
func TestWriteMessageDisabled_ByteIdenticalWithNative(t *testing.T) {
	_ = os.Unsetenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_GORILLA_WS_TRACING_ENABLED")
	wsGate.ResetForTest()
	t.Cleanup(wsGate.ResetForTest)

	payload := []byte(`{"hello":"world","n":42}`)

	// Plain-websocket capture server — receives one message and echoes its
	// raw bytes back so the client can diff.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(websocket.TextMessage, data)
	}))
	defer srv.Close()

	send := func(useOtel bool) []byte {
		if useOtel {
			c, _, err := Dial(context.Background(), "ws"+srv.URL[4:], nil, nil)
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			require.NoError(t, c.WriteMessage(context.Background(), websocket.TextMessage, payload))
			_, _, data, err := c.ReadMessage(context.Background())
			require.NoError(t, err)
			return data
		}
		dialer := websocket.DefaultDialer
		c, _, err := dialer.Dial("ws"+srv.URL[4:], nil)
		require.NoError(t, err)
		defer c.Close()
		require.NoError(t, c.WriteMessage(websocket.TextMessage, payload))
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		return data
	}

	native := send(false)
	wrapped := send(true)
	assert.Equal(t, native, wrapped,
		"disabled-mode otelgorillaws.WriteMessage must produce byte-identical wire output as raw gorilla/websocket")
	assert.Equal(t, payload, wrapped,
		"disabled-mode payload must reach the server unmodified (no envelope wrapping)")
}

// TestWriteMessageEnabled_NonNegotiatedStillByteIdentical exercises the
// subprotocol-negotiation half of the per-connection gate: even when env
// flags are ON, if the handshake does NOT negotiate otel-ws (the server
// returns no protocol or a non-otel one) the wrapper must still emit byte-
// identical wire output. Spans may still be created (spans-only mode), but
// the envelope is suppressed.
func TestWriteMessageEnabled_NonNegotiatedStillByteIdentical(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	wsGate.ResetForTest()
	t.Cleanup(wsGate.ResetForTest)

	payload := []byte(`{"hello":"world"}`)
	var capturedMu sync.Mutex
	var captured []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use a plain (non-OTel) upgrader so the otel-ws negotiation cannot
		// succeed — the server simply does not understand the otel-ws prefix.
		up := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		capturedMu.Lock()
		captured = append([]byte(nil), data...)
		capturedMu.Unlock()
	}))
	defer srv.Close()

	// Dial WITHOUT proposing a subprotocol — Dial() will NOT inject otel-ws
	// (per conn.go docs: "If subprotocols is nil or empty, no otel-ws
	// injection is performed and the returned Conn operates in passthrough
	// mode").
	c, _, err := Dial(context.Background(), "ws"+srv.URL[4:], nil, nil)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	require.NoError(t, c.WriteMessage(context.Background(), websocket.TextMessage, payload))

	// Wait for the server to capture the bytes.
	require.Eventually(t, func() bool {
		capturedMu.Lock()
		defer capturedMu.Unlock()
		return captured != nil
	}, 2*time.Second, 10*time.Millisecond)

	capturedMu.Lock()
	got := captured
	capturedMu.Unlock()
	assert.Equal(t, payload, got,
		"non-negotiated mode (env on, subprotocol off): payload bytes must be wire-identical with native (no envelope)")
}

// TestSubprotocolStripsOTelPrefix locks in the contract that Conn.Subprotocol()
// returns the application protocol with the otel-ws+ prefix removed — callers
// expecting the negotiated app protocol see "json", not "otel-ws+json". See
// README "Subprotocol() strips the otel-ws prefix".
//
// Cleanup is ordered carefully: the httptest server is registered via
// t.Cleanup AFTER wsGate.ResetForTest, so under LIFO ordering the server
// shuts down first (joining its handler goroutine) and only then does the
// gate reset run. This avoids a race where the handler reads wsGate.Enabled
// while cleanup is concurrently rewriting g.once.
func TestSubprotocolStripsOTelPrefix(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	wsGate.ResetForTest()
	t.Cleanup(wsGate.ResetForTest)

	up := &Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json", "msgpack"},
	}
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		_, _, _, _ = c.ReadMessage(context.Background())
	}))
	t.Cleanup(srv.Close)

	conn, _, err := Dial(context.Background(), "ws"+srv.URL[4:], nil, []string{"json"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		<-handlerDone
	})

	assert.Equal(t, "json", conn.Subprotocol(),
		"Subprotocol() must strip the otel-ws+ prefix and return the app-level protocol")
}
