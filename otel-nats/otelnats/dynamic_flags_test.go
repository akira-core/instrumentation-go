package otelnats

import (
	"testing"

	nats "github.com/nats-io/nats.go"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// These tests prove the Conn RE-READS the relay on every operation rather than
// caching a value at construction, and that the relay decides in BOTH
// directions. No reset hook is involved: the resolver caches nothing, so a
// rebound provider is observed on the very next operation.
//
// Every test installs the provider BEFORE constructing the Conn. That ordering
// is load-bearing, not stylistic: relayPossible is resolved at construction, and
// a Conn built while no relay could exist resolves statically for its whole life.
//
// Not parallel-safe: the OpenFeature provider and the process environment are
// both process-global. No t.Parallel in this file.

// boolFlag builds an in-memory OpenFeature boolean flag with on/off variants.
//
// Deliberately duplicated per module rather than imported from otel-testkit: the
// instrumentation modules are published independently, so importing a helper
// from the untagged testkit would put an unresolvable requirement in a released
// go.mod (`go mod tidy` in any consumer pulls test dependencies of imported
// packages).
func boolFlag(v bool) memprovider.InMemoryFlag {
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

// setRelay installs an in-memory provider serving whichever keys are given.
// Calling it again inside the same test models an operator changing the relay
// configuration; a nil entry models a key the relay simply does not define.
func setRelay(t *testing.T, keys map[string]*bool) {
	t.Helper()
	flags := map[string]memprovider.InMemoryFlag{}
	for key, v := range keys {
		if v != nil {
			flags[key] = boolFlag(*v)
		}
	}
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
		memprovider.NewInMemoryProvider(flags)))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})
}

// setTracingRelay is the common case: only this module's key is configured.
func setTracingRelay(t *testing.T, tracing bool) {
	t.Helper()
	setRelay(t, map[string]*bool{flagKeyNATSTracing: &tracing})
}

// mustConn builds a Conn, failing the test on a configuration error.
func mustConn(t *testing.T, opts ...Option) *Conn {
	t.Helper()
	c, err := newConn(&nats.Conn{}, opts...)
	require.NoError(t, err)
	return c
}

// silentEnv leaves every switch unset, so each assertion is attributable to the
// relay alone. Under a module default of false that is the ordinary deployment
// state, not a special case.
func silentEnv(t *testing.T) {
	t.Helper()
	setOrUnset(t, otelflags.EnvGlobalTracing, false, "")
	setOrUnset(t, envNATSTracingEnabled, false, "")
	setOrUnset(t, otelflags.EnvFlagsEndpoint, false, "")
}

func TestRelayFlipsTracingOnALiveConn(t *testing.T) {
	silentEnv(t)
	setTracingRelay(t, false)

	conn := mustConn(t)
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "relay says off → passthrough impl")
	assert.False(t, conn.TracingEnabled())

	// Operator flips the flag. The SAME Conn must follow it.
	setTracingRelay(t, true)
	_, isTraced := conn.impl().(*tracedConn)
	assert.True(t, isTraced, "relay flipped on → instrumented, without reconnecting")
	assert.True(t, conn.TracingEnabled())

	setTracingRelay(t, false)
	_, isDirect = conn.impl().(*directConn)
	assert.True(t, isDirect, "relay flipped back off → passthrough again")
}

// TestRelayEnablesWhatTheDeploymentLeftOff is the capability the revoke-only
// model did not have, and the reason this revision exists.
func TestRelayEnablesWhatTheDeploymentLeftOff(t *testing.T) {
	silentEnv(t)
	setTracingRelay(t, true)

	conn := mustConn(t)
	require.NotNil(t, conn.traced,
		"the instrumented impl must be allocated whenever a relay could enable it")
	assert.True(t, conn.TracingEnabled(),
		"the relay enabled a module whose environment says nothing")
}

// TestMasterRelayVetoBeatsEverything: the master key stops a module its own key
// enables, and stops a connection carrying WithTracingEnabled(true).
func TestMasterRelayVetoBeatsEverything(t *testing.T) {
	silentEnv(t)
	t.Setenv(envNATSTracingEnabled, "true")

	on, off := true, false
	setRelay(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &off,
		flagKeyNATSTracing:             &on,
	})

	conn := mustConn(t)
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "the master veto must stop a module its own key enables")

	optioned := mustConn(t, WithTracingEnabled(true))
	assert.False(t, optioned.TracingEnabled(),
		"the master veto must stop a connection carrying WithTracingEnabled(true)")
}

// TestMasterEnvVetoBeatsTheRelay: the environment spelling of the master switch
// is ANDed above the relay, so it holds even while the relay says on.
func TestMasterEnvVetoBeatsTheRelay(t *testing.T) {
	silentEnv(t)
	t.Setenv(otelflags.EnvGlobalTracing, "false")
	setTracingRelay(t, true)

	conn := mustConn(t)
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "the master environment veto must beat an enabling relay")
	assert.False(t, conn.TracingEnabled())
}

// TestWithTracingEnabledDoesNotPin: the option supplies one rung and says
// nothing about the relay or the master.
func TestWithTracingEnabledDoesNotPin(t *testing.T) {
	t.Run("option true still follows a relay disable", func(t *testing.T) {
		silentEnv(t)
		setTracingRelay(t, true)

		conn := mustConn(t, WithTracingEnabled(true))
		assert.True(t, conn.TracingEnabled())

		setTracingRelay(t, false)
		assert.False(t, conn.TracingEnabled(),
			"there is no way to opt a connection out of a relay decision")
	})

	t.Run("option false still follows a relay enable", func(t *testing.T) {
		silentEnv(t)
		setTracingRelay(t, false)

		conn := mustConn(t, WithTracingEnabled(false))
		assert.False(t, conn.TracingEnabled())

		setTracingRelay(t, true)
		assert.True(t, conn.TracingEnabled(),
			"the relay is authoritative in both directions, over the option too")
	})
}

// TestEnvironmentBeatsOptionUnderARelay pins that moving the option below the
// environment variable did not disturb the relay's position above both.
func TestEnvironmentBeatsOptionUnderARelay(t *testing.T) {
	silentEnv(t)
	t.Setenv(envNATSTracingEnabled, "false")
	setRelay(t, map[string]*bool{}) // a provider exists, but defines no key

	conn := mustConn(t, WithTracingEnabled(true))
	assert.False(t, conn.TracingEnabled(),
		"with the relay silent, the environment variable must beat the option")
}

func TestNoProviderReproducesEnvOnlyBehavior(t *testing.T) {
	silentEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	t.Setenv(envNATSTracingEnabled, "1")

	conn := mustConn(t)
	_, isTraced := conn.impl().(*tracedConn)
	assert.True(t, isTraced, "module variable on and no provider → instrumented")

	// Environment variables are read ONCE, at construction: no hot path touches
	// one, so a later change needs a new connection. Only the relay is
	// observable without reconstructing.
	t.Setenv(envNATSTracingEnabled, "false")
	_, stillTraced := conn.impl().(*tracedConn)
	assert.True(t, stillTraced,
		"the module variable is fixed at construction; changing it must not affect a live Conn")

	rebuilt := mustConn(t)
	_, isDirect := rebuilt.impl().(*directConn)
	assert.True(t, isDirect, "a connection built after the change takes the passthrough path")
}

// TestNoRelayPossibleAllocatesNothingInstrumented is the zero-cost path: with no
// endpoint and no provider, a switched-off connection must not even build the
// instrumented implementation.
func TestNoRelayPossibleAllocatesNothingInstrumented(t *testing.T) {
	silentEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))

	conn := mustConn(t)
	assert.False(t, conn.gate.relayPossible)
	assert.Nil(t, conn.traced,
		"no relay can exist and the local answer is off → no OTel path may be reachable")
}

// TestSubscriptionHandlerFollowsTheRelay pins the C3 regression: a subscription
// handler is bound once at Subscribe time, so it must re-resolve per message
// rather than freeze impl()'s answer at bind time.
func TestSubscriptionHandlerFollowsTheRelay(t *testing.T) {
	silentEnv(t)
	setTracingRelay(t, false)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn := mustConn(t)
	var seen int
	h := conn.msgHandler("subj", "", func(m Msg) { seen++ })

	// Bound while the relay says off: no span for this message.
	h(&nats.Msg{Subject: "subj"})
	require.Equal(t, 1, seen)
	assert.Empty(t, sr.Ended(), "relay off at delivery → no process span")

	// Same bound handler, relay flips on: the next message must trace.
	setTracingRelay(t, true)
	h(&nats.Msg{Subject: "subj"})
	require.Equal(t, 2, seen)
	assert.NotEmpty(t, sr.Ended(), "relay on at delivery → process span from the SAME subscription")

	before := len(sr.Ended())
	setTracingRelay(t, false)
	h(&nats.Msg{Subject: "subj"})
	assert.Len(t, sr.Ended(), before, "relay back off → spans stop again")
}

// TestMasterVetoStopsASubscription: a bound handler observes the master veto per
// message, like every other path.
func TestMasterVetoStopsASubscription(t *testing.T) {
	silentEnv(t)
	t.Setenv(envNATSTracingEnabled, "1")
	on := true
	setRelay(t, map[string]*bool{otelflags.FlagKeyGlobalTracing: &on})

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn := mustConn(t)
	h := conn.msgHandler("subj", "", func(m Msg) {})

	h(&nats.Msg{Subject: "subj"})
	require.NotEmpty(t, sr.Ended(), "master on → spans")

	before := len(sr.Ended())
	off := false
	setRelay(t, map[string]*bool{otelflags.FlagKeyGlobalTracing: &off})
	h(&nats.Msg{Subject: "subj"})
	assert.Len(t, sr.Ended(), before, "master vetoed → the same subscription stops emitting")
}

// TestWrapperBuiltBeforeTheProviderStaysStatic pins the ordering rule an
// application is most likely to trip over. relayPossible is resolved once, at
// construction; a Conn built while no relay could exist resolves from its
// environment and options for the rest of its life, and installing a provider
// afterwards never reaches it.
//
// This is why feature-flags.md tells applications to install their provider
// BEFORE constructing any wrapper, and why every relay test in this file does
// exactly that.
func TestWrapperBuiltBeforeTheProviderStaysStatic(t *testing.T) {
	silentEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	t.Setenv(envNATSTracingEnabled, "1")

	conn := mustConn(t)
	require.False(t, conn.gate.relayPossible, "no endpoint and no provider at construction time")
	require.True(t, conn.TracingEnabled(), "the environment enabled it")

	// The application installs its provider too late, and revokes the module.
	setTracingRelay(t, false)

	assert.True(t, conn.TracingEnabled(),
		"a Conn built before any relay existed never consults one; the relay's false is not observed")

	rebuilt := mustConn(t)
	assert.False(t, rebuilt.TracingEnabled(),
		"a Conn built after the install does observe it — the fix is ordering, not a reset hook")
}
