package otelgorillaws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// These tests cover the two halves of this module's flag handling, which are
// deliberately gated on different things:
//
//   - Span creation follows the relay per call, so a live connection stops and
//     starts emitting spans without being re-established.
//   - otel-ws subprotocol negotiation follows the GLOBAL switch only. It happens
//     during the handshake and cannot be revisited, so gating it on the dynamic
//     value would leave every connection established during an "off" window
//     permanently unable to propagate trace context.
//
// Not parallel-safe: the OpenFeature provider, the process environment and
// wsResolver are all process-global. No t.Parallel in this file.

// relayFlag builds an in-memory OpenFeature boolean flag with on/off variants.
//
// Deliberately duplicated per module rather than extracted into a shared test
// helper module: the four instrumentation modules are published independently,
// so importing a helper from the untagged otel-testkit module would put an
// unresolvable requirement in a released go.mod (`go mod tidy` in any consumer
// pulls test dependencies of imported packages).
func relayFlag(v bool) memprovider.InMemoryFlag {
	variant := "off"
	if v {
		variant = "on"
	}
	return memprovider.InMemoryFlag{
		State:          memprovider.Enabled,
		DefaultVariant: variant,
		Variants:       map[string]any{"on": true, "off": false},
	}
}

// setRelay installs an in-memory provider serving the WebSocket tracing flag and
// re-arms the resolver. Calling it again models an operator flipping the flag.
func setRelay(t *testing.T, tracing bool) {
	t.Helper()
	require.NoError(t, openfeature.SetProviderAndWait(memprovider.NewInMemoryProvider(
		map[string]memprovider.InMemoryFlag{flagKeyWSTracing: relayFlag(tracing)},
	)))
	resetWSGateForTest()
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetProviderAndWait(openfeature.NoopProvider{}))
		resetWSGateForTest()
	})
}

// globalOn turns on the kill switch while leaving the module env var off, so
// every assertion is attributable to the relay and not to the environment.
func globalOn(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "false")
	resetWSGateForTest()
}

func TestSpanGateFollowsTheRelayOnALiveConn(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	c := &Conn{}
	configureConn(c, resolveConnOptions(nil))

	assert.False(t, c.featureEnabled(), "relay says off → no spans")

	setRelay(t, true)
	assert.True(t, c.featureEnabled(),
		"relay flipped on → the same live Conn starts creating spans")

	setRelay(t, false)
	assert.False(t, c.featureEnabled(), "relay flipped back off → spans stop")
}

func TestNegotiationIgnoresTheRelay(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	cfg := resolveConnOptions(nil)
	assert.True(t, effectiveCapability(cfg),
		"the global switch is on, so otel-ws is still negotiated even though the relay says off — "+
			"otherwise a connection opened now could never propagate once the relay flips on")

	setRelay(t, true)
	assert.True(t, effectiveCapability(cfg))
}

func TestGlobalKillSwitchSuppressesNegotiationAndSpans(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envWSTracingEnabled, "true")
	resetWSGateForTest()
	setRelay(t, true)

	cfg := resolveConnOptions(nil)
	assert.False(t, effectiveCapability(cfg), "kill switch off → never negotiate otel-ws")

	c := &Conn{}
	configureConn(c, cfg)
	assert.False(t, c.featureEnabled(), "kill switch off → no spans regardless of the relay")
}

func TestWithTracingEnabledPinsAgainstTheRelay(t *testing.T) {
	globalOn(t)

	t.Run("option true stays on when the relay says off", func(t *testing.T) {
		setRelay(t, true)
		c := &Conn{}
		configureConn(c, resolveConnOptions([]Option{WithTracingEnabled(true)}))
		assert.True(t, c.featureEnabled())

		setRelay(t, false)
		assert.True(t, c.featureEnabled(),
			"an overridden Conn is static — the relay cannot turn it off")
	})

	t.Run("option false stays off when the relay says on", func(t *testing.T) {
		setRelay(t, false)
		cfg := resolveConnOptions([]Option{WithTracingEnabled(false)})
		c := &Conn{}
		configureConn(c, cfg)
		assert.False(t, c.featureEnabled())
		assert.False(t, effectiveCapability(cfg), "option false also suppresses negotiation")

		setRelay(t, true)
		assert.False(t, c.featureEnabled(),
			"an overridden Conn is static — the relay cannot turn it on")
	})
}

// TestUpgraderNegotiatesOTelWSWhileRelayFlagIsOff is the full-stack proof of the
// negotiation rule: the handshake commits to the envelope even though tracing is
// dynamically off, so a later flip can start producing linked spans on the very
// same connection.
func TestUpgraderNegotiatesOTelWSWhileRelayFlagIsOff(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	// The handler runs on the server's goroutine; hand the Conn back over a
	// channel rather than a shared variable so the read is ordered and race-free.
	conns := make(chan *Conn, 1)
	up := &Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns <- c
	}))
	t.Cleanup(srv.Close)

	dialer := websocket.Dialer{Subprotocols: []string{otelWSProtocol, "json"}}
	raw, resp, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	assert.True(t, strings.HasPrefix(resp.Header.Get("Sec-WebSocket-Protocol"), otelWSProtocol),
		"server confirms otel-ws on the global switch alone, not on the relay value")
	var srvConn *Conn
	select {
	case srvConn = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server never completed the upgrade")
	}
	require.NotNil(t, srvConn)
	assert.True(t, srvConn.tracingEnabled, "negotiation succeeded → envelopes are on the wire")
	assert.False(t, srvConn.featureEnabled(), "…while the relay keeps span creation off")

	// The flag flips; the already-established connection can now both trace and
	// propagate, because it negotiated the envelope up front.
	setRelay(t, true)
	assert.True(t, srvConn.featureEnabled())
	assert.True(t, srvConn.tracingEnabled)
}

func TestWriteMessageEmitsSpansOnlyWhileTheRelaySaysOn(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	raw, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	conn := NewConn(raw)
	ctx := context.Background()

	require.NoError(t, conn.WriteMessage(ctx, websocket.TextMessage, []byte("a")))
	assert.Empty(t, sr.Ended(), "relay off → no send span")

	setRelay(t, true)
	require.NoError(t, conn.WriteMessage(ctx, websocket.TextMessage, []byte("b")))
	assert.NotEmpty(t, sr.Ended(),
		"relay on → the same live connection emits a send span")
}

func TestNoProviderReproducesEnvOnlyBehavior(t *testing.T) {
	require.NoError(t, openfeature.SetProviderAndWait(openfeature.NoopProvider{}))
	t.Cleanup(resetWSGateForTest)

	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "1")
	resetWSGateForTest()

	c := &Conn{}
	configureConn(c, resolveConnOptions(nil))
	assert.True(t, c.featureEnabled(), "env vars on and no provider → spans, exactly as before")

	t.Setenv(envWSTracingEnabled, "false")
	resetWSGateForTest()
	assert.False(t, c.featureEnabled(), "env var off → no spans")
}

// TestNegotiatedPairSurvivesAsymmetricDynamicFlags is the wire-corruption
// regression test for the envelope contract. One side is pinned on via
// WithTracingEnabled(true); the other follows the relay, which says off. Both
// negotiated otel-ws, so BOTH sides must keep speaking the envelope: the
// dynamic-off side still wraps on write (empty header) and still unwraps on
// read. The payload is deliberately shaped like an envelope — if either side
// dropped to raw passthrough, the pinned-on peer would dismember it (write
// direction) or the dynamic-off side would surface envelope bytes (read
// direction).
func TestNegotiatedPairSurvivesAsymmetricDynamicFlags(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	// Payload that a broken raw-write path would get dismembered by the peer's
	// probing unwrap.
	trap := []byte(`{"header":{"traceparent":"fake"},"data":"inner"}`)

	// Server: pinned ON.
	conns := make(chan *Conn, 1)
	up := &Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil, WithTracingEnabled(true))
		if err != nil {
			return
		}
		conns <- c
	}))
	t.Cleanup(srv.Close)

	// Client: dynamic, relay says off.
	client, resp, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil, []string{"json"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.True(t, strings.HasPrefix(resp.Header.Get("Sec-WebSocket-Protocol"), otelWSProtocol),
		"pair must negotiate otel-ws for this test to mean anything")
	require.True(t, client.tracingEnabled)
	require.False(t, client.featureEnabled(), "client's dynamic gate must be off")

	var server *Conn
	select {
	case server = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server never completed the upgrade")
	}

	// Write direction: dynamic-off client → pinned-on server.
	require.NoError(t, client.WriteMessage(context.Background(), websocket.TextMessage, trap))
	_, _, got, err := server.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(trap), string(got),
		"dynamic-off writer must envelope, or the pinned-on reader dismembers the payload")

	// Read direction: pinned-on server → dynamic-off client.
	// JSON payload: round-trips byte-identical through the envelope (non-JSON
	// payloads are JSON-string-encoded by the wire format — upstream behavior,
	// not under test here).
	reply := []byte(`{"msg":"plain"}`)
	require.NoError(t, server.WriteMessage(context.Background(), websocket.TextMessage, reply))
	_, _, got, err = client.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(reply), string(got),
		"dynamic-off reader must unwrap the peer's envelope, not surface raw envelope bytes")
}

// TestIncapableConnStaysRawPassthrough pins the pre-dynamic disabled behavior:
// with the kill switch off, NewConn must NOT start writing envelopes.
func TestIncapableConnStaysRawPassthrough(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envWSTracingEnabled, "false")
	resetWSGateForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, data) // echo raw bytes
		}
	}))
	t.Cleanup(srv.Close)

	raw, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	conn := NewConn(raw) // no otel-ws subprotocol; capable=false
	require.False(t, conn.capable)
	require.False(t, conn.tracingEnabled)

	payload := []byte(`{"x":1}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(payload), string(got),
		"incapable conn is raw passthrough — the echoed bytes must be the original payload, not an envelope")
}

// TestNewConn_GlobalOnModuleOff_RawPeerNoEnvelope is the PR #27 regression:
// capability on + module/relay off + NewConn without otel-ws must not rewrite
// the peer's payload into a JSON envelope.
func TestNewConn_GlobalOnModuleOff_RawPeerNoEnvelope(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "false")
	resetWSGateForTest()

	var peerSaw []byte
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		peerSaw = append([]byte(nil), data...)
		close(done)
	}))
	t.Cleanup(srv.Close)

	raw, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	conn := NewConn(raw)
	require.True(t, conn.capable, "global on ⇒ capable")
	require.False(t, conn.tracingEnabled, "no otel-ws subprotocol ⇒ no envelope")
	require.False(t, conn.featureEnabled(), "module flag off")

	payload := []byte("hello")
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peer did not receive message")
	}
	assert.Equal(t, string(payload), string(peerSaw),
		"raw peer must see original payload, not {\"header\":{},\"data\":...}")
}
