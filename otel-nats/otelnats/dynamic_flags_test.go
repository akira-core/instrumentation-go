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
)

// These tests prove the Conn RE-READS the tracing flag rather than caching a
// value at construction. The resolver reset stands in for "one TTL elapsed" —
// the TTL boundary itself is unit-tested in internal/flags.
//
// Not parallel-safe: the OpenFeature provider, the process environment and
// natsResolver are all process-global. No t.Parallel in this file.

// boolFlag builds an in-memory OpenFeature boolean flag with on/off variants.
//
// Deliberately duplicated per module rather than extracted into a shared test
// helper module: the four instrumentation modules are published independently,
// so importing a helper from the untagged otel-testkit module would put an
// unresolvable requirement in a released go.mod (`go mod tidy` in any consumer
// pulls test dependencies of imported packages).
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

// setRelay installs an in-memory provider serving the NATS tracing flag and
// re-arms the resolver. Calling it again inside the same test models an operator
// flipping the flag on the relay proxy.
func setRelay(t *testing.T, tracing bool) {
	t.Helper()
	require.NoError(t, openfeature.SetProviderAndWait(memprovider.NewInMemoryProvider(
		map[string]memprovider.InMemoryFlag{flagKeyNATSTracing: boolFlag(tracing)},
	)))
	resetNATSGateForTest()
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetProviderAndWait(openfeature.NoopProvider{}))
		resetNATSGateForTest()
	})
}

// globalOn turns on the kill switch while leaving the module env var off, so
// every assertion is attributable to the relay and not to the environment.
func globalOn(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envNATSTracingEnabled, "false")
	resetNATSGateForTest()
}

func TestRelayFlipsTracingOnALiveConn(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	conn := newConn(&nats.Conn{})
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "relay says off → passthrough impl")
	assert.False(t, conn.TracingEnabled())

	// Operator flips the flag. The SAME Conn must follow it.
	setRelay(t, true)
	_, isTraced := conn.impl().(*tracedConn)
	assert.True(t, isTraced, "relay flipped on → instrumented, without reconnecting")
	assert.True(t, conn.TracingEnabled())

	setRelay(t, false)
	_, isDirect = conn.impl().(*directConn)
	assert.True(t, isDirect, "relay flipped back off → passthrough again")
}

func TestGlobalKillSwitchBeatsTheRelay(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envNATSTracingEnabled, "true")
	resetNATSGateForTest()
	setRelay(t, true)

	conn := newConn(&nats.Conn{})
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "global kill switch off → passthrough regardless of the relay")
	assert.Nil(t, conn.traced,
		"instrumented impl must not even be constructed, so no OTel path is reachable")

	setRelay(t, true)
	_, isDirect = conn.impl().(*directConn)
	assert.True(t, isDirect, "flipping the relay cannot revive it")
}

func TestWithTracingEnabledPinsAgainstTheRelay(t *testing.T) {
	globalOn(t)

	t.Run("option true stays on when the relay says off", func(t *testing.T) {
		setRelay(t, true)
		conn := newConn(&nats.Conn{}, WithTracingEnabled(true))
		assert.True(t, conn.TracingEnabled())

		setRelay(t, false)
		assert.True(t, conn.TracingEnabled(),
			"an overridden Conn is static — the relay cannot turn it off")
	})

	t.Run("option false stays off when the relay says on", func(t *testing.T) {
		setRelay(t, false)
		conn := newConn(&nats.Conn{}, WithTracingEnabled(false))
		assert.False(t, conn.TracingEnabled())

		setRelay(t, true)
		assert.False(t, conn.TracingEnabled(),
			"an overridden Conn is static — the relay cannot turn it on")
		assert.Nil(t, conn.traced, "option false must not build the instrumented impl")
	})
}

func TestNoProviderReproducesEnvOnlyBehavior(t *testing.T) {
	require.NoError(t, openfeature.SetProviderAndWait(openfeature.NoopProvider{}))
	t.Cleanup(resetNATSGateForTest)

	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envNATSTracingEnabled, "1")
	resetNATSGateForTest()

	conn := newConn(&nats.Conn{})
	_, isTraced := conn.impl().(*tracedConn)
	assert.True(t, isTraced, "env vars on and no provider → instrumented, exactly as before")

	t.Setenv(envNATSTracingEnabled, "false")
	resetNATSGateForTest()
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "env var off → passthrough")
}

// TestSubscriptionHandlerFollowsTheRelay pins the C3 regression: a subscription
// handler is bound once at Subscribe time, so it must re-resolve the flag per
// message rather than freeze impl()'s answer at bind time.
func TestSubscriptionHandlerFollowsTheRelay(t *testing.T) {
	globalOn(t)
	setRelay(t, false)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn := newConn(&nats.Conn{})
	var seen int
	h := conn.msgHandler("subj", "", func(m Msg) { seen++ })

	// Bound while the relay says off: no span for this message.
	h(&nats.Msg{Subject: "subj"})
	require.Equal(t, 1, seen)
	assert.Empty(t, sr.Ended(), "relay off at delivery → no process span")

	// Same bound handler, relay flips on: the next message must trace.
	setRelay(t, true)
	h(&nats.Msg{Subject: "subj"})
	require.Equal(t, 2, seen)
	assert.NotEmpty(t, sr.Ended(), "relay on at delivery → process span from the SAME subscription")

	before := len(sr.Ended())
	setRelay(t, false)
	h(&nats.Msg{Subject: "subj"})
	assert.Len(t, sr.Ended(), before, "relay back off → spans stop again")
}

// TestStaticConnSubscriptionIgnoresTheRelay: an overridden Conn's subscription
// handler is pinned, both directions.
func TestStaticConnSubscriptionIgnoresTheRelay(t *testing.T) {
	globalOn(t)
	setRelay(t, true)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn := newConn(&nats.Conn{}, WithTracingEnabled(false))
	h := conn.msgHandler("subj", "", func(m Msg) {})

	h(&nats.Msg{Subject: "subj"})
	setRelay(t, true)
	h(&nats.Msg{Subject: "subj"})
	assert.Empty(t, sr.Ended(), "WithTracingEnabled(false) pins the subscription off regardless of the relay")
}
