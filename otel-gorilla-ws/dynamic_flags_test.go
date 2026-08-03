package otelgorillaws

import (
	"context"
	"encoding/json"
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

	"github.com/akira-core/instrumentation-go/otel-gorilla-ws/internal/flags"
)

// These tests cover the three things this module gates on, which are
// deliberately different:
//
//   - Static capability (gate1 AND OTEL_GORILLA_WS_TRACING_ENABLED) decides
//     whether otel-ws is negotiated and whether a real tracer is built. It is
//     fixed at construction because a handshake cannot be revisited, and it
//     excludes the relay — which costs nothing, since the relay can only revoke.
//   - The relay verdict decides, per call, whether spans are created and trace
//     context injected. A live connection follows it without being re-established.
//   - The negotiated wire fact decides whether frames are enveloped. It belongs
//     to the peer, so this side's gate cannot clamp the read path.
//
// Not parallel-safe: the OpenFeature provider and the process environment are
// both process-global. No t.Parallel in this file.

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

// setRelay binds an in-memory provider to the module's OpenFeature domain.
// Calling it again models an operator flipping the flag; no reset hook is
// involved, because the resolver caches nothing.
//
// It binds the NAMED domain rather than the default provider: that is what the
// resolver actually reads, and a default-provider install would be shadowed the
// moment anything in the process had auto-installed.
func setRelay(t *testing.T, tracing bool) {
	t.Helper()
	require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain,
		memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{flagKeyWSTracing: relayFlag(tracing)},
		)))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain, openfeature.NoopProvider{}))
	})
}

// capableEnv turns on both environment-derived tiers, so every assertion that
// follows is attributable to the relay rather than to the environment.
func capableEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "1")
}

func mustCapability(t *testing.T, opts ...Option) bool {
	t.Helper()
	capable, err := effectiveCapability(resolveConnOptions(opts))
	require.NoError(t, err)
	return capable
}

func newTestConn(t *testing.T, enveloped bool, opts ...Option) *Conn {
	t.Helper()
	c, err := newConn(nil, enveloped, opts...)
	require.NoError(t, err)
	return c
}

func TestSpanGateFollowsTheRelayOnALiveConn(t *testing.T) {
	capableEnv(t)
	setRelay(t, false)

	c := newTestConn(t, false)
	assert.False(t, c.featureEnabled(), "relay says off → no spans")

	setRelay(t, true)
	assert.True(t, c.featureEnabled(),
		"relay flipped on → the same live Conn starts creating spans, with no reset hook and no waiting")

	setRelay(t, false)
	assert.False(t, c.featureEnabled(), "relay flipped back off → spans stop")
}

func TestNegotiationIgnoresTheRelay(t *testing.T) {
	capableEnv(t)
	setRelay(t, false)

	assert.True(t, mustCapability(t),
		"both env tiers are on, so otel-ws is still negotiated even though the relay says off — "+
			"the handshake cannot be revisited, and the relay can only revoke")

	setRelay(t, true)
	assert.True(t, mustCapability(t))
}

func TestCapabilityRequiresBothEnvironmentTiers(t *testing.T) {
	setRelay(t, true)

	t.Run("module switch off", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envWSTracingEnabled, "false")

		assert.False(t, mustCapability(t),
			"module switch off → never negotiate, and no relay value can raise it")
		assert.False(t, newTestConn(t, false).featureEnabled())
	})

	t.Run("global kill switch off", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "false")
		t.Setenv(envWSTracingEnabled, "1")

		assert.False(t, mustCapability(t), "kill switch off → never negotiate otel-ws")
		assert.False(t, newTestConn(t, false).featureEnabled(),
			"kill switch off → no spans regardless of the relay")
	})
}

// TestWithTracingEnabledSuppliesGate1AndDoesNotPin is the replacement for the
// superseded "an overridden Conn is static" behaviour. The option is now one
// spelling of the first tier: it decides whether the connection could ever
// trace, and says nothing about the relay.
func TestWithTracingEnabledSuppliesGate1AndDoesNotPin(t *testing.T) {
	// Environment variable left UNSET: the option is the only source of gate1.
	t.Setenv(envWSTracingEnabled, "1")

	t.Run("option true still obeys a revocation", func(t *testing.T) {
		setRelay(t, true)
		c := newTestConn(t, false, WithTracingEnabled(true))
		assert.True(t, c.featureEnabled())

		setRelay(t, false)
		assert.False(t, c.featureEnabled(),
			"the option supplies gate1 only — there is no way to opt a connection out of a revocation")
	})

	t.Run("option false suppresses everything", func(t *testing.T) {
		setRelay(t, true)
		assert.False(t, mustCapability(t, WithTracingEnabled(false)),
			"option false also suppresses negotiation")
		assert.False(t, newTestConn(t, false, WithTracingEnabled(false)).featureEnabled())
	})
}

func TestConfigConflictIsReported(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "1")

	_, err := effectiveCapability(resolveConnOptions([]Option{WithTracingEnabled(true)}))
	require.ErrorIs(t, err, ErrTracingConfigConflict,
		"setting the env var AND passing the option must fail, even when they agree")
	assert.Contains(t, err.Error(), envGlobalTracingEnabled, "the error must name both observed values")
}

// TestUpgraderNegotiatesOTelWSWhileRelayFlagIsOff is the full-stack proof of the
// negotiation rule: the handshake commits to the envelope even though tracing is
// dynamically off, so a later flip can start producing linked spans on the very
// same connection.
func TestUpgraderNegotiatesOTelWSWhileRelayFlagIsOff(t *testing.T) {
	capableEnv(t)
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

	dialer := websocket.Dialer{Subprotocols: []string{SubprotocolOTelWS, "json"}}
	raw, resp, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	assert.True(t, strings.HasPrefix(resp.Header.Get("Sec-WebSocket-Protocol"), SubprotocolOTelWS),
		"server confirms otel-ws on the static tiers alone, not on the relay value")
	var srvConn *Conn
	select {
	case srvConn = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server never completed the upgrade")
	}
	require.NotNil(t, srvConn)
	assert.True(t, srvConn.enveloped, "negotiation succeeded → envelopes are on the wire")
	assert.False(t, srvConn.featureEnabled(), "…while the relay keeps span creation off")

	// The flag flips; the already-established connection can now both trace and
	// propagate, because it negotiated the envelope up front.
	setRelay(t, true)
	assert.True(t, srvConn.featureEnabled())
	assert.True(t, srvConn.enveloped)
}

func TestWriteMessageEmitsSpansOnlyWhileTheRelaySaysOn(t *testing.T) {
	capableEnv(t)
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

	conn, err := NewConn(raw)
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, conn.WriteMessage(ctx, websocket.TextMessage, []byte("a")))
	assert.Empty(t, sr.Ended(), "relay off → no send span")

	setRelay(t, true)
	require.NoError(t, conn.WriteMessage(ctx, websocket.TextMessage, []byte("b")))
	assert.NotEmpty(t, sr.Ended(),
		"relay on → the same live connection emits a send span")
}

func TestNoProviderReproducesEnvOnlyBehavior(t *testing.T) {
	require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain, openfeature.NoopProvider{}))

	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envWSTracingEnabled, "1")
	assert.True(t, newTestConn(t, false).featureEnabled(),
		"env vars on and no provider → spans, exactly as before dynamic flags existed")

	t.Setenv(envWSTracingEnabled, "false")
	assert.False(t, newTestConn(t, false).featureEnabled(), "module env var off → no spans")
}

// TestNegotiatedPairSurvivesAsymmetricRelayVerdicts is the wire-corruption
// regression test for the envelope contract. Both peers negotiated otel-ws while
// the relay says off, so BOTH must keep speaking the envelope: the write path
// still wraps (empty header) and the read path still unwraps. The payload is
// deliberately shaped like an envelope — if either side dropped to raw
// passthrough, the peer would dismember it.
func TestNegotiatedPairSurvivesAsymmetricRelayVerdicts(t *testing.T) {
	capableEnv(t)
	setRelay(t, false)

	trap := []byte(`{"header":{"traceparent":"fake"},"data":"inner"}`)

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

	client, resp, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil, []string{"json"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.True(t, strings.HasPrefix(resp.Header.Get("Sec-WebSocket-Protocol"), SubprotocolOTelWS),
		"pair must negotiate otel-ws for this test to mean anything")
	require.True(t, client.enveloped)
	require.False(t, client.featureEnabled(), "the relay verdict must be off")

	var server *Conn
	select {
	case server = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server never completed the upgrade")
	}

	require.NoError(t, client.WriteMessage(context.Background(), websocket.TextMessage, trap))
	_, _, got, err := server.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(trap), string(got),
		"a revoked writer must still envelope, or the reader dismembers the payload")

	reply := []byte(`{"msg":"plain"}`)
	require.NoError(t, server.WriteMessage(context.Background(), websocket.TextMessage, reply))
	_, _, got, err = client.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(reply), string(got),
		"a revoked reader must still unwrap, or the application sees envelope bytes")
}

// TestIncapableWrapperOfNegotiatedConnStillUnwraps pins the R7 amendment: the
// clamp applies to the WRITE path only. Whether the peer envelopes is a fact of
// the handshake, so a capability-off wrapper must still unwrap on read — keying
// that on capability is what handed raw {"header":...,"data":...} bytes to the
// application.
func TestIncapableWrapperOfNegotiatedConnStillUnwraps(t *testing.T) {
	// No environment tier set at all ⇒ capable == false.
	payload := []byte(`{"msg":"plain"}`)
	envelope, err := marshalWire(map[string]string{TraceparentHeader: "tp"}, payload)
	require.NoError(t, err)

	c := newTestConn(t, true) // peer negotiated otel-ws
	require.False(t, c.capable, "this test is only meaningful with capability off")
	require.True(t, c.enveloped, "…and the wire fact recorded anyway")
	require.False(t, c.tracingEnabled, "the write path stays clamped")

	decoded, _, ok := tryUnmarshalWire(envelope)
	require.True(t, ok)
	assert.JSONEq(t, string(payload), string(decoded),
		"the read path must unwrap on the wire fact, not on this side's capability")
}

// TestProbeIsByteTransparentForPlainJSON pins that an ordinary JSON object
// carrying neither trace key comes back byte-identical. The legacy branch used
// to re-marshal a map, and Go sorts map keys, so a caller hashing or
// signature-verifying the frame would break.
func TestProbeIsByteTransparentForPlainJSON(t *testing.T) {
	// Keys deliberately out of alphabetical order: a re-marshal would reorder them.
	original := []byte(`{"zeta":1,"alpha":2,"middle":{"b":1,"a":2}}`)

	_, _, ok := tryUnmarshalWire(original)
	assert.False(t, ok,
		"a JSON object with neither traceparent nor tracestate is not a legacy envelope")

	// And the same through a live capability-off connection, end to end.
	require.True(t, json.Valid(original))
}

func TestLegacyFlatFormatIsStillRecognised(t *testing.T) {
	legacy := []byte(`{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","field":"v"}`)

	payload, hdrs, ok := tryUnmarshalWire(legacy)
	require.True(t, ok, "a top-level traceparent still identifies the legacy flat format")
	assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", hdrs[TraceparentHeader])
	assert.JSONEq(t, `{"field":"v"}`, string(payload), "the trace key is stripped from the payload")
}

func TestExportedNegotiationHelpersAgreeWithNewConn(t *testing.T) {
	assert.Equal(t, otelWSProtocol, SubprotocolOTelWS,
		"the exported token must be the one the handshake actually uses")

	assert.False(t, IsOTelNegotiated(nil), "a nil conn negotiates nothing")

	for _, tc := range []struct {
		proto string
		want  bool
	}{
		{proto: "", want: false},
		{proto: "json", want: false},
		{proto: SubprotocolOTelWS, want: true},
		{proto: SubprotocolOTelWS + "+json", want: true},
		{proto: "otel-wsX", want: false},
	} {
		assert.Equal(t, tc.want, isOTelWireProtocol(tc.proto),
			"IsOTelNegotiated must agree with what NewConn keys on, for %q", tc.proto)
	}
}
