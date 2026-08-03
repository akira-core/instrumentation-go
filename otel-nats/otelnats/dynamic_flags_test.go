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

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats/internal/flags"
)

// These tests prove the Conn RE-READS the relay verdict rather than caching a
// value at construction. No reset hook is involved: the resolver caches nothing,
// so a rebound provider is observed on the very next operation.
//
// Not parallel-safe: the OpenFeature provider and the process environment are
// both process-global. No t.Parallel in this file.

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
	require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain,
		memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{flagKeyNATSTracing: boolFlag(tracing)},
		)))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain, openfeature.NoopProvider{}))
	})
}

// mustConn builds a Conn, failing the test on a configuration conflict.
func mustConn(t *testing.T, opts ...Option) *Conn {
	t.Helper()
	c, err := newConn(&nats.Conn{}, opts...)
	require.NoError(t, err)
	return c
}

// capableEnv turns on both environment-derived tiers, so every assertion that
// follows is attributable to the relay and not to the environment. Both are
// required: since D7 the module switch decides whether the instrumented
// implementation is built at all.
func capableEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envNATSTracingEnabled, "1")
}

func TestRelayFlipsTracingOnALiveConn(t *testing.T) {
	capableEnv(t)
	setRelay(t, false)

	conn := mustConn(t)
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
	setRelay(t, true)

	conn := mustConn(t)
	_, isDirect := conn.impl().(*directConn)
	assert.True(t, isDirect, "global kill switch off → passthrough regardless of the relay")
	assert.Nil(t, conn.traced,
		"instrumented impl must not even be constructed, so no OTel path is reachable")

	setRelay(t, true)
	_, isDirect = conn.impl().(*directConn)
	assert.True(t, isDirect, "flipping the relay cannot revive it")
}

// TestWithTracingEnabledSuppliesGate1AndDoesNotPin replaces the superseded
// "an overridden Conn is static" behaviour. The option is now one spelling of
// the first tier: it decides whether the connection could ever trace, and says
// nothing about the relay.
//
// The global environment variable is left UNSET throughout, because since D3
// setting it as well as passing the option is a configuration error.
func TestWithTracingEnabledSuppliesGate1AndDoesNotPin(t *testing.T) {
	t.Setenv(envNATSTracingEnabled, "1")

	t.Run("option true still obeys a revocation", func(t *testing.T) {
		setRelay(t, true)
		conn := mustConn(t, WithTracingEnabled(true))
		assert.True(t, conn.TracingEnabled())

		setRelay(t, false)
		assert.False(t, conn.TracingEnabled(),
			"the option supplies gate1 only — there is no way to opt a connection out of a revocation")
	})

	t.Run("option false builds only the passthrough", func(t *testing.T) {
		setRelay(t, true)
		conn := mustConn(t, WithTracingEnabled(false))
		assert.False(t, conn.TracingEnabled())
		assert.Nil(t, conn.traced,
			"gate1 off must not allocate the instrumented impl, whichever spelling supplied it")

		setRelay(t, false)
		assert.False(t, conn.TracingEnabled())
	})
}

func TestConfigConflictIsReported(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envNATSTracingEnabled, "1")

	_, err := newConn(&nats.Conn{}, WithTracingEnabled(true))
	require.ErrorIs(t, err, ErrTracingConfigConflict,
		"setting the env var AND passing the option must fail, even when they agree")
	assert.Contains(t, err.Error(), envGlobalTracingEnabled, "the error must name both observed values")
}

func TestNoProviderReproducesEnvOnlyBehavior(t *testing.T) {
	require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain, openfeature.NoopProvider{}))

	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envNATSTracingEnabled, "1")

	conn := mustConn(t)
	_, isTraced := conn.impl().(*tracedConn)
	assert.True(t, isTraced, "env vars on and no provider → instrumented, exactly as before")

	// The module switch is read ONCE, at construction (D7/D8): no environment
	// variable is touched on a hot path any more, so a later change needs a new
	// connection. Only the relay verdict is observable without reconstructing.
	t.Setenv(envNATSTracingEnabled, "false")
	_, stillTraced := conn.impl().(*tracedConn)
	assert.True(t, stillTraced,
		"the module env var is fixed at construction; changing it must not affect a live Conn")

	rebuilt := mustConn(t)
	_, isDirect := rebuilt.impl().(*directConn)
	assert.True(t, isDirect, "a connection built after the change takes the passthrough path")
}

// TestSubscriptionHandlerFollowsTheRelay pins the C3 regression: a subscription
// handler is bound once at Subscribe time, so it must re-resolve the flag per
// message rather than freeze impl()'s answer at bind time.
func TestSubscriptionHandlerFollowsTheRelay(t *testing.T) {
	capableEnv(t)
	setRelay(t, false)

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
	setRelay(t, true)
	h(&nats.Msg{Subject: "subj"})
	require.Equal(t, 2, seen)
	assert.NotEmpty(t, sr.Ended(), "relay on at delivery → process span from the SAME subscription")

	before := len(sr.Ended())
	setRelay(t, false)
	h(&nats.Msg{Subject: "subj"})
	assert.Len(t, sr.Ended(), before, "relay back off → spans stop again")
}

// TestGate1OffSubscriptionNeverTraces: with the first tier off the instrumented
// implementation is never built, so no relay value can make a subscription trace.
func TestGate1OffSubscriptionNeverTraces(t *testing.T) {
	t.Setenv(envNATSTracingEnabled, "1")
	setRelay(t, true)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn := mustConn(t, WithTracingEnabled(false))
	require.Nil(t, conn.traced, "gate1 off must not allocate the instrumented impl")
	h := conn.msgHandler("subj", "", func(m Msg) {})

	h(&nats.Msg{Subject: "subj"})
	setRelay(t, true)
	h(&nats.Msg{Subject: "subj"})
	assert.Empty(t, sr.Ended(), "gate1 off → no spans, and nothing on the relay can change that")
}
