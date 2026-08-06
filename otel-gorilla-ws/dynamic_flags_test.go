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

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// These tests cover the three things this module gates on, which are
// deliberately different:
//
//   - The effective tracing value (master AND module, down the whole ladder,
//     relay included) decides per call whether spans are created and trace
//     context injected. A live connection follows a relay change without being
//     re-established.
//   - The SAME expression, evaluated once immediately before the handshake,
//     decides whether otel-ws is negotiated. A handshake cannot be revisited, so
//     enabling the module reaches connections opened afterwards only.
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
	setRelayFlags(t, map[string]*bool{flagKeyWSTracing: &tracing})
}

// setRelayFlags binds an in-memory provider serving whichever keys are given,
// which is what lets a test put the MASTER key on the relay alongside this
// module's own. A nil entry models a key the relay simply does not define.
func setRelayFlags(t *testing.T, keys map[string]*bool) {
	t.Helper()
	flags := map[string]memprovider.InMemoryFlag{}
	for key, v := range keys {
		if v != nil {
			flags[key] = relayFlag(*v)
		}
	}
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
		memprovider.NewInMemoryProvider(flags)))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})
}

// moduleOnEnv turns this module's variable on and leaves the master switch
// unset, which under a default of enabled is the ordinary deployment shape.
func moduleOnEnv(t *testing.T) {
	t.Helper()
	clearWSFlagEnv(t)
	t.Setenv(envWSTracingEnabled, "1")
}

// mustTracing resolves the effective tracing value — the same expression the
// handshake uses to decide negotiation.
func mustTracing(t *testing.T, opts ...Option) bool {
	t.Helper()
	gate, err := resolveGates(resolveConnOptions(opts))
	require.NoError(t, err)
	return gate.tracing()
}

func newTestConn(t *testing.T, enveloped bool, opts ...Option) *Conn {
	t.Helper()
	c, err := newConn(nil, enveloped, opts...)
	require.NoError(t, err)
	return c
}

func TestSpanGateFollowsTheRelayOnALiveConn(t *testing.T) {
	moduleOnEnv(t)
	setRelay(t, false)

	c := newTestConn(t, false)
	assert.False(t, c.featureEnabled(), "relay says off → no spans")

	setRelay(t, true)
	assert.True(t, c.featureEnabled(),
		"relay flipped on → the same live Conn starts creating spans, with no reset hook and no waiting")

	setRelay(t, false)
	assert.False(t, c.featureEnabled(), "relay flipped back off → spans stop")
}

// TestNegotiationFollowsTheRelayAtHandshakeTime is the reversal of the
// superseded rule. The relay can enable now, so excluding it from negotiation
// would leave a connection unable to carry trace context in a process the
// operator has just switched on.
func TestNegotiationFollowsTheRelayAtHandshakeTime(t *testing.T) {
	clearWSFlagEnv(t)
	setRelay(t, false)
	assert.False(t, mustTracing(t),
		"relay off at handshake time → otel-ws is not offered")

	setRelay(t, true)
	assert.True(t, mustTracing(t),
		"relay on at handshake time → otel-ws is offered, even with the environment silent")
}

func TestMasterVetoStopsNegotiationAndSpans(t *testing.T) {
	setRelay(t, true)

	t.Run("master environment veto", func(t *testing.T) {
		clearWSFlagEnv(t)
		t.Setenv(otelflags.EnvGlobalTracing, "false")
		t.Setenv(envWSTracingEnabled, "1")

		assert.False(t, mustTracing(t), "master veto → never negotiate otel-ws")
		assert.False(t, newTestConn(t, false).featureEnabled(),
			"master veto → no spans regardless of the relay")
	})

	t.Run("module variable off with the relay silent", func(t *testing.T) {
		clearWSFlagEnv(t)
		t.Setenv(envWSTracingEnabled, "false")
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
			memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{})))
		t.Cleanup(func() {
			require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
		})

		assert.False(t, mustTracing(t), "module variable off and the relay silent → no negotiation")
		assert.False(t, newTestConn(t, false).featureEnabled())
	})
}

// TestMasterRelayVetoStopsNegotiationAndSpans is the master switch's RELAY
// spelling — the rung above the environment veto the subtest in
// TestMasterVetoStopsNegotiationAndSpans covers. Both of this module's gated
// decisions must fall: the handshake never offers otel-ws, and no span is
// created.
func TestMasterRelayVetoStopsNegotiationAndSpans(t *testing.T) {
	moduleOnEnv(t)

	on, off := true, false
	setRelayFlags(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &off,
		flagKeyWSTracing:               &on,
	})

	c := newTestConn(t, false)

	assert.False(t, mustTracing(t),
		"the master key on the relay must stop otel-ws being negotiated, even though this module's key says on")
	assert.False(t, mustTracing(t, WithTracingEnabled(true)),
		"…including for a connection carrying WithTracingEnabled(true)")
	assert.False(t, c.featureEnabled(),
		"…and no span may be created on a connection already open")

	// Control. Everything else here says on — the module variable, the module
	// key, the master's local default — so lifting the veto must restore both
	// decisions. Without this, a build that ignored the master key entirely
	// would still pass the assertions above by never having been off.
	setRelayFlags(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &on,
		flagKeyWSTracing:               &on,
	})
	assert.True(t, mustTracing(t), "lifting the master veto restores negotiation")
	assert.True(t, c.featureEnabled(), "…and spans on the same live connection")
}

// TestMasterRelayTrueDoesNotEnable pins the master's asymmetry in its relay
// spelling: its local default is already true, so serving it true changes
// nothing and this module's key still has to say yes. Only false has an effect.
func TestMasterRelayTrueDoesNotEnable(t *testing.T) {
	clearWSFlagEnv(t)

	on, off := true, false
	setRelayFlags(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &on,
		flagKeyWSTracing:               &off,
	})

	assert.False(t, mustTracing(t),
		"the master saying on cannot enable a module whose own key says off")
	assert.False(t, newTestConn(t, false).featureEnabled())
}

// TestWithTracingEnabledDoesNotPin: the option supplies one rung and says
// nothing about the relay or the master.
func TestWithTracingEnabledDoesNotPin(t *testing.T) {
	t.Run("option true still follows a relay disable", func(t *testing.T) {
		clearWSFlagEnv(t)
		setRelay(t, true)
		c := newTestConn(t, false, WithTracingEnabled(true))
		assert.True(t, c.featureEnabled())

		setRelay(t, false)
		assert.False(t, c.featureEnabled(),
			"there is no way to opt a connection out of a relay decision")
	})

	t.Run("option false still follows a relay enable", func(t *testing.T) {
		clearWSFlagEnv(t)
		setRelay(t, false)
		c := newTestConn(t, false, WithTracingEnabled(false))
		assert.False(t, c.featureEnabled())

		setRelay(t, true)
		assert.True(t, c.featureEnabled(),
			"the relay is authoritative in both directions, over the option too")
	})
}

// TestEnvironmentBeatsOptionForNegotiation pins the ordering change where it is
// most visible: a deployment can stop this module negotiating otel-ws even
// though the application's Go code asked for tracing.
func TestEnvironmentBeatsOptionForNegotiation(t *testing.T) {
	clearWSFlagEnv(t)
	t.Setenv(envWSTracingEnabled, "false")
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
		memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{})))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})

	assert.False(t, mustTracing(t, WithTracingEnabled(true)),
		"OTEL_GORILLA_WS_TRACING_ENABLED=false must beat WithTracingEnabled(true)")
}

// TestUpgraderNegotiationIsDecidedAtHandshakeTime is the full-stack proof of the
// asymmetry: the relay decides the wire once, when the handshake runs. Turning
// the module off afterwards stops spans but leaves the envelope, because the
// peer is still parsing every frame as one.
func TestUpgraderNegotiationIsDecidedAtHandshakeTime(t *testing.T) {
	clearWSFlagEnv(t)
	setRelay(t, true)

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
		"the relay said on at handshake time, so the server confirms otel-ws")
	var srvConn *Conn
	select {
	case srvConn = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server never completed the upgrade")
	}
	require.NotNil(t, srvConn)
	assert.True(t, srvConn.enveloped, "negotiation succeeded → envelopes are on the wire")
	assert.True(t, srvConn.featureEnabled(), "…and the relay keeps span creation on")

	// The operator turns the module off. Spans stop immediately; the envelope
	// does not, because the peer committed to it at the handshake and removing it
	// mid-connection would desynchronise the wire.
	setRelay(t, false)
	assert.False(t, srvConn.featureEnabled())
	assert.True(t, srvConn.enveloped,
		"disabling stops spans and inject/extract, but never the envelope")
}

func TestWriteMessageEmitsSpansOnlyWhileTheRelaySaysOn(t *testing.T) {
	moduleOnEnv(t)
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
	clearWSFlagEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))

	t.Setenv(envWSTracingEnabled, "1")
	assert.True(t, newTestConn(t, false).featureEnabled(),
		"module variable on and no provider → spans")

	t.Setenv(envWSTracingEnabled, "false")
	assert.False(t, newTestConn(t, false).featureEnabled(), "module variable off → no spans")
}

// TestNegotiatedPairSurvivesADisable is the wire-corruption regression test for
// the envelope contract. The pair negotiates otel-ws while the relay says on,
// the operator then turns the module off, and BOTH sides must keep speaking the
// envelope: the write path still wraps (empty header) and the read path still
// unwraps. The payload is deliberately shaped like an envelope — if either side
// dropped to raw passthrough, the peer would dismember it.
//
// This is the asymmetry stated in the module docs: disabling stops spans and
// inject/extract immediately, but never the envelope, because the peer committed
// to it at a handshake that cannot be revisited.
func TestNegotiatedPairSurvivesADisable(t *testing.T) {
	moduleOnEnv(t)
	setRelay(t, true)

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
	require.True(t, client.featureEnabled(), "the pair negotiated while the relay said on")

	var server *Conn
	select {
	case server = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server never completed the upgrade")
	}

	// The operator turns the module off AFTER both sides committed to the wire.
	setRelay(t, false)
	require.False(t, client.featureEnabled(), "spans must stop")
	require.True(t, client.enveloped, "…while the envelope must not")

	require.NoError(t, client.WriteMessage(context.Background(), websocket.TextMessage, trap))
	_, _, got, err := server.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(trap), string(got),
		"a disabled writer must still envelope, or the reader dismembers the payload")

	reply := []byte(`{"msg":"plain"}`)
	require.NoError(t, server.WriteMessage(context.Background(), websocket.TextMessage, reply))
	_, _, got, err = client.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(reply), string(got),
		"a disabled reader must still unwrap, or the application sees envelope bytes")
}

// TestIncapableWrapperOfNegotiatedConnStillUnwraps pins the R7 amendment: the
// clamp applies to the WRITE path only. Whether the peer envelopes is a fact of
// the handshake, so a capability-off wrapper must still unwrap on read — keying
// that on capability is what handed raw {"header":...,"data":...} bytes to the
// application.
func TestIncapableWrapperOfNegotiatedConnStillUnwraps(t *testing.T) {
	// Nothing configured and no relay possible ⇒ capable == false.
	clearWSFlagEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
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

// TestWrapperBuiltBeforeTheProviderStaysStatic pins the ordering rule an
// application is most likely to trip over. relayPossible is resolved once, at
// construction; a Conn built while no relay could exist resolves from its
// environment and options for the rest of its life, and installing a provider
// afterwards never reaches it.
func TestWrapperBuiltBeforeTheProviderStaysStatic(t *testing.T) {
	clearWSFlagEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	t.Setenv(envWSTracingEnabled, "1")

	c := newTestConn(t, false)
	require.False(t, c.gate.relayPossible, "no endpoint and no provider at construction time")
	require.True(t, c.featureEnabled(), "the environment enabled it")

	setRelay(t, false) // installed too late

	assert.True(t, c.featureEnabled(),
		"a Conn built before any relay existed never consults one")
	assert.False(t, newTestConn(t, false).featureEnabled(),
		"a Conn built after the install does observe it — the fix is ordering, not a reset hook")
}

// TestTwoConnectionsCanDiffer is what the option is uniquely able to express,
// and it survives the option moving below the environment variable — on the
// condition that the deployment leaves that variable unset.
func TestTwoConnectionsCanDiffer(t *testing.T) {
	clearWSFlagEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))

	on := newTestConn(t, false, WithTracingEnabled(true))
	off := newTestConn(t, false, WithTracingEnabled(false))

	assert.True(t, on.featureEnabled(), "the connection built with WithTracingEnabled(true) must trace")
	assert.False(t, off.featureEnabled(), "…and the one built with (false) must not")
}
