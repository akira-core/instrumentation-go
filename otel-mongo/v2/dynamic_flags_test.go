package otelmongo

import (
	"context"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/traced"
)

// These tests prove the wrappers RE-READ the flags rather than caching a value
// at construction. The resolver reset stands in for "one TTL elapsed" — the TTL
// boundary itself is unit-tested in internal/flags, and sleeping a real second
// per case would be the slowest possible way to test the same thing.
//
// None of them need a live MongoDB: the observable is which implementation a
// wrapper dispatches to, which is decided before any driver call happens.
//
// Not parallel-safe: the OpenFeature provider, the process environment and
// mongoResolver are all process-global. No t.Parallel in this file.

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

// setRelay installs an in-memory provider serving the two Mongo flags and
// re-arms the resolver so the next read sees them. Calling it again inside the
// same test models an operator flipping a flag on the relay proxy.
func setRelay(t *testing.T, tracing, propagation bool) {
	t.Helper()
	err := openfeature.SetProviderAndWait(memprovider.NewInMemoryProvider(
		map[string]memprovider.InMemoryFlag{
			flagKeyMongoTracing:     boolFlag(tracing),
			flagKeyMongoPropagation: boolFlag(propagation),
		},
	))
	require.NoError(t, err)
	resetPropEnabledCacheForTest()
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetProviderAndWait(openfeature.NoopProvider{}))
		resetPropEnabledCacheForTest()
	})
}

// globalOn turns on the kill switch while leaving BOTH module env vars off, so
// every assertion below is attributable to the relay and not to the env.
func globalOn(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "false")
	t.Setenv(envMongoPropagationEnabled, "false")
	resetPropEnabledCacheForTest()
}

func TestRelayFlipsTracingOnALiveCollection(t *testing.T) {
	globalOn(t)
	setRelay(t, false, false)

	raw := &mongo.Collection{}
	tracer := noop.NewTracerProvider().Tracer("test")
	coll := NewCollection(raw, tracer, otel.GetTextMapPropagator())

	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"relay says off → passthrough")

	// Operator flips the flag. The SAME Collection must follow it.
	setRelay(t, true, false)
	assert.IsType(t, &traced.Collection{}, coll.impl(),
		"relay flipped on → instrumented, without rebuilding the Collection")

	setRelay(t, false, false)
	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"relay flipped back off → passthrough again")
}

func TestRelayFlipsPropagationIndependentlyOfTracing(t *testing.T) {
	globalOn(t)
	setRelay(t, true, false)

	raw := &mongo.Collection{}
	coll := NewCollection(raw, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())

	tc, ok := coll.impl().(*traced.Collection)
	require.True(t, ok)
	assert.False(t, tc.PropagationEnabled(), "propagation off on the relay")

	setRelay(t, true, true)
	assert.True(t, tc.PropagationEnabled(),
		"propagation flipped on → the same traced.Collection reports the new value")

	// Tracing off force-disables propagation regardless of the propagation flag.
	setRelay(t, false, true)
	assert.False(t, tc.PropagationEnabled(),
		"tracing off must force propagation off even with the propagation flag on")
}

func TestGlobalKillSwitchBeatsTheRelay(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "true")
	t.Setenv(envMongoPropagationEnabled, "true")
	resetPropEnabledCacheForTest()
	setRelay(t, true, true)

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())

	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"global kill switch off → passthrough regardless of the relay")
	assert.Nil(t, coll.traced,
		"instrumented impl must not even be constructed, so no OTel path is reachable")

	// Flipping the relay cannot revive it.
	setRelay(t, true, true)
	assert.IsType(t, &direct.Collection{}, coll.impl())
}

func TestWithTracingEnabledPinsAgainstTheRelay(t *testing.T) {
	globalOn(t)

	t.Run("option true stays on when the relay says off", func(t *testing.T) {
		setRelay(t, true, true)
		on := true
		c := &Client{tracingOverride: &on, tracedBuilt: true}
		assert.True(t, c.effectiveTracing())

		setRelay(t, false, false)
		assert.True(t, c.effectiveTracing(),
			"an overridden client is static — the relay cannot turn it off")
	})

	t.Run("option false stays off when the relay says on", func(t *testing.T) {
		setRelay(t, false, false)
		off := false
		c := &Client{tracingOverride: &off, tracedBuilt: true}
		assert.False(t, c.effectiveTracing())

		setRelay(t, true, true)
		assert.False(t, c.effectiveTracing(),
			"an overridden client is static — the relay cannot turn it on")
	})

	t.Run("no option follows the relay", func(t *testing.T) {
		setRelay(t, false, false)
		c := &Client{tracedBuilt: true}
		assert.False(t, c.effectiveTracing())

		setRelay(t, true, false)
		assert.True(t, c.effectiveTracing(),
			"without an override the client follows the relay")
	})
}

func TestWithTracePropagationEnabledStillCannotBypassRelayTracingOff(t *testing.T) {
	globalOn(t)
	setRelay(t, false, false)

	propOn := true
	c := &Client{propagationOverride: &propOn, tracedBuilt: true}
	assert.False(t, c.effectivePropagation(),
		"WithTracePropagationEnabled(true) cannot enable propagation while tracing resolves off")

	setRelay(t, true, false)
	assert.True(t, c.effectivePropagation(),
		"once tracing resolves on, the propagation override wins over the relay's propagation flag")
}

func TestContextFromDocumentFollowsTheRelay(t *testing.T) {
	globalOn(t)
	setRelay(t, true, true)

	doc := map[string]any{
		shared.TraceMetadataKey: map[string]any{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
	}

	_, ok := ContextFromDocument(context.Background(), doc)
	assert.True(t, ok, "both flags on → extraction succeeds")

	setRelay(t, true, false)
	_, ok = ContextFromDocument(context.Background(), doc)
	assert.False(t, ok,
		"relay disabled propagation → the package-level helper stops extracting, matching the Collection path")

	setRelay(t, false, true)
	_, ok = ContextFromDocument(context.Background(), doc)
	assert.False(t, ok, "tracing off force-disables propagation here too")
}

func TestChangeStreamSwitchesImplMidIteration(t *testing.T) {
	globalOn(t)
	setRelay(t, true, true)

	raw := &mongo.Collection{}
	coll := NewCollection(raw, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())

	// Build the facade ChangeStream the way Collection.Watch does, without
	// needing a server to open a real change stream.
	cs := coll.newChangeStream(&mongo.ChangeStream{})
	require.NotNil(t, cs.traced, "instrumented impl must be built while the kill switch is on")

	assert.IsType(t, &traced.ChangeStream{}, cs.impl(),
		"relay says on → instrumented")

	// A change stream can stay open for days; a flag change must reach it.
	setRelay(t, false, false)
	assert.IsType(t, &direct.ChangeStream{}, cs.impl(),
		"relay flipped off mid-iteration → passthrough, without reopening the stream")

	setRelay(t, true, true)
	assert.IsType(t, &traced.ChangeStream{}, cs.impl())
}

func TestCursorSwitchesImplMidIteration(t *testing.T) {
	globalOn(t)
	setRelay(t, true, true)

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	cur := coll.newCursor(&mongo.Cursor{})
	require.NotNil(t, cur.traced)

	assert.IsType(t, &traced.Cursor{}, cur.impl())

	setRelay(t, false, false)
	assert.IsType(t, &direct.Cursor{}, cur.impl(),
		"relay flipped off mid-iteration → passthrough for the remaining documents")
}

func TestNoProviderReproducesEnvOnlyBehavior(t *testing.T) {
	// No provider installed anywhere: every flag must fall back to its env var,
	// i.e. behave exactly as the release before dynamic flags.
	require.NoError(t, openfeature.SetProviderAndWait(openfeature.NoopProvider{}))
	t.Cleanup(resetPropEnabledCacheForTest)

	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
	resetPropEnabledCacheForTest()

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	tc, ok := coll.impl().(*traced.Collection)
	require.True(t, ok, "env vars on → instrumented")
	assert.True(t, tc.PropagationEnabled())

	t.Setenv(envMongoTracingEnabled, "false")
	resetPropEnabledCacheForTest()
	assert.IsType(t, &direct.Collection{}, coll.impl(), "env var off → passthrough")
}

// TestStaticClientNeverEvaluatesOpenFeature pins the C6 regression: a client
// pinned by WithTracingEnabled must not let the relay supply its propagation
// default — especially when the pin is what carried tracing past a disabled
// global kill switch, where a resolver read would be the only path by which a
// relay value reaches a kill-switched process.
func TestStaticClientNeverEvaluatesOpenFeature(t *testing.T) {
	// Kill switch OFF; relay would enable everything.
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "false")
	t.Setenv(envMongoPropagationEnabled, "false")
	resetPropEnabledCacheForTest()
	setRelay(t, true, true)

	on := true
	c := &Client{tracingOverride: &on, tracedBuilt: true}

	assert.True(t, c.effectiveTracing(), "the override alone carries tracing")
	assert.False(t, c.effectivePropagation(),
		"relay must not supply the propagation default for a static client — kill switch is off")

	// The env var alone is the static client's propagation default.
	t.Setenv(envMongoPropagationEnabled, "1")
	resetPropEnabledCacheForTest()
	assert.True(t, c.effectivePropagation(),
		"env var decides the static client's propagation default")

	// And an explicit propagation override still wins over the env var.
	off := false
	c2 := &Client{tracingOverride: &on, propagationOverride: &off, tracedBuilt: true}
	assert.False(t, c2.effectivePropagation())
}
