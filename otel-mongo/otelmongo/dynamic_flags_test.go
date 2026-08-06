package otelmongo

import (
	"context"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// These tests prove the wrappers RE-READ the flags rather than caching a value
// at construction, and that the relay decides in BOTH directions. Nothing is
// cached and there is no reset hook: a rebound provider is observed on the very
// next operation.
//
// Every test binds the provider BEFORE building the wrapper. relayPossible is
// resolved at construction, so a wrapper built while no relay could exist
// resolves statically for its whole life.
//
// None of them need a live MongoDB: the observable is which implementation a
// wrapper dispatches to, which is decided before any driver call happens.
//
// Not parallel-safe: the OpenFeature provider, the process environment and
// mongoResolver are all process-global. No t.Parallel in this file.

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

// setRelay binds an in-memory provider serving the two Mongo flags to the
// module's OpenFeature domain. Calling it again inside the same test models an
// operator flipping a flag on the relay proxy; no reset hook is involved,
// because the resolver caches nothing.
//
// It binds the NAMED domain rather than the default provider: that is what the
// resolver reads, and a default-provider install would be shadowed by any named
// binding left behind elsewhere in this binary.
func setRelay(t *testing.T, tracing, propagation bool) {
	t.Helper()
	setRelayFlags(t, map[string]*bool{
		flagKeyMongoTracing:     &tracing,
		flagKeyMongoPropagation: &propagation,
	})
}

// setRelayFlags binds an in-memory provider serving whichever keys are given,
// which is what lets a test put the MASTER key on the relay alongside this
// module's two. A nil entry models a key the relay simply does not define.
func setRelayFlags(t *testing.T, keys map[string]*bool) {
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

// capableEnv turns on both module switches and leaves the master unset, which
// under a default of enabled is the ordinary deployment shape. Each assertion
// that follows is then attributable to the relay.
func capableEnv(t *testing.T) {
	t.Helper()
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
}

// silentRelayEnv leaves every switch unset, so the relay alone decides.
func silentRelayEnv(t *testing.T) {
	t.Helper()
	silentFlagEnv(t)
}

// envGateState is the gateState a client built in capableEnv would carry.
func envGateState(t *testing.T) gateState {
	t.Helper()
	g, err := resolveGates(nil, nil)
	require.NoError(t, err)
	return g
}

func TestRelayFlipsTracingOnALiveCollection(t *testing.T) {
	capableEnv(t)
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
	capableEnv(t)
	setRelay(t, true, false)

	raw := &mongo.Collection{}
	coll := NewCollection(raw, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())

	tc, ok := coll.impl().(*traced.Collection)
	require.True(t, ok)
	assert.False(t, tc.PropagationEnabled(), "propagation off on the relay")

	setRelay(t, true, true)
	assert.True(t, tc.PropagationEnabled(),
		"propagation flipped on → the same traced.Collection reports the new value")

	// The instrumented impl's PropagationEnabled is propagationWhenTracing: it is
	// reached only after the facade resolved tracing true for this operation, so
	// it must NOT re-resolve tracing (design R5). Tracing off is expressed by
	// impl() selecting the passthrough instead, asserted below.
	setRelay(t, false, true)
	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"tracing revoked → the facade selects the passthrough, which writes no _oteltrace")
}

// TestRelayEnablesWhatTheDeploymentLeftOff is the capability the revoke-only
// model did not have, and the reason this revision exists.
func TestRelayEnablesWhatTheDeploymentLeftOff(t *testing.T) {
	silentRelayEnv(t)
	setRelay(t, true, true)

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	require.NotNil(t, coll.traced,
		"the instrumented impl must be allocated whenever a relay could enable it")
	assert.IsType(t, &traced.Collection{}, coll.impl(),
		"the relay enabled a module whose environment says nothing")
}

// TestMasterVetoBeatsTheRelay: the master switch is ANDed above everything, in
// either spelling.
func TestMasterVetoBeatsTheRelay(t *testing.T) {
	capableEnv(t)
	t.Setenv(envGlobalTracingEnabled, "false")
	setRelay(t, true, true)

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())

	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"master veto → passthrough regardless of the relay")

	g := envGateState(t)
	assert.False(t, g.effectiveTracing(), "…so no relay value can produce a span")
	assert.False(t, g.effectivePropagation(), "…nor a byte of _oteltrace")
}

// TestMasterRelayVetoBeatsEverything is the master switch's RELAY spelling —
// the rung above the environment veto TestMasterVetoBeatsTheRelay covers, and
// the one that makes "one relay flag stops every module" true rather than
// aspirational. It must stop a client whose own key the relay enables, and one
// whose Go code asked for tracing and propagation outright.
func TestMasterRelayVetoBeatsEverything(t *testing.T) {
	capableEnv(t)

	on, off := true, false
	setRelayFlags(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &off,
		flagKeyMongoTracing:            &on,
		flagKeyMongoPropagation:        &on,
	})

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"the master key on the relay must stop a module its own key enables")

	optioned, err := resolveGates(boolPtr(true), boolPtr(true))
	require.NoError(t, err)
	assert.False(t, optioned.effectiveTracing(),
		"…including a client carrying WithTracingEnabled(true)")
	assert.False(t, optioned.effectivePropagation(),
		"…and not one byte of _oteltrace may reach the operator's documents")

	// Control. Everything else here says on — the module variables, the module
	// keys, the master's local default — so lifting the veto must restore
	// tracing. Without this, a build that ignored the master key entirely would
	// still pass the assertions above by never having been off.
	setRelayFlags(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &on,
		flagKeyMongoTracing:            &on,
		flagKeyMongoPropagation:        &on,
	})
	assert.IsType(t, &traced.Collection{}, coll.impl(),
		"lifting the master veto restores tracing on the same live Collection")
}

// TestMasterRelayTrueDoesNotEnable pins the master's asymmetry in its relay
// spelling: its local default is already true, so serving it true changes
// nothing and the module tier still has to say yes. Only false has an effect.
func TestMasterRelayTrueDoesNotEnable(t *testing.T) {
	silentRelayEnv(t)

	on, off := true, false
	setRelayFlags(t, map[string]*bool{
		otelflags.FlagKeyGlobalTracing: &on,
		flagKeyMongoTracing:            &off,
		flagKeyMongoPropagation:        &off,
	})

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	assert.IsType(t, &direct.Collection{}, coll.impl(),
		"the master saying on cannot enable a module whose own key says off")
}

// TestWithTracingEnabledDoesNotPin: the option supplies one rung and says
// nothing about the relay or the master.
func TestWithTracingEnabledDoesNotPin(t *testing.T) {
	t.Run("option true still follows a relay disable", func(t *testing.T) {
		silentRelayEnv(t)
		setRelay(t, true, true)

		g, err := resolveGates(boolPtr(true), boolPtr(true))
		require.NoError(t, err)
		require.True(t, g.tracedPossible())
		assert.True(t, g.effectiveTracing())

		setRelay(t, false, false)
		assert.False(t, g.effectiveTracing(),
			"there is no way to opt a client out of a relay decision")
	})

	t.Run("option false still follows a relay enable", func(t *testing.T) {
		silentRelayEnv(t)
		setRelay(t, false, false)

		g, err := resolveGates(boolPtr(false), boolPtr(false))
		require.NoError(t, err)
		assert.False(t, g.effectiveTracing())

		setRelay(t, true, true)
		assert.True(t, g.effectiveTracing(),
			"the relay is authoritative in both directions, over the option too")
	})
}

// TestEnvironmentBeatsOptionUnderASilentRelay pins the ordering that reverses
// 0.7.0, with a provider installed but defining no key.
func TestEnvironmentBeatsOptionUnderASilentRelay(t *testing.T) {
	silentRelayEnv(t)
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "false")
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
		memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{})))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})

	g, err := resolveGates(boolPtr(false), boolPtr(true))
	require.NoError(t, err)
	assert.True(t, g.effectiveTracing(), "the tracing variable beats the option")
	assert.False(t, g.effectivePropagation(),
		"the propagation variable beats the option — the operator can stop document writes")
}

func TestWithTracePropagationEnabledStillCannotBypassRelayTracingOff(t *testing.T) {
	capableEnv(t)
	setRelay(t, false, false)

	g := envGateState(t)
	assert.False(t, g.effectivePropagation(),
		"propagation cannot be enabled while tracing resolves off")

	setRelay(t, true, true)
	assert.True(t, g.effectivePropagation(),
		"once tracing resolves on, the propagation tier and its relay flag decide")

	setRelay(t, true, false)
	assert.False(t, g.effectivePropagation(),
		"the relay revokes propagation independently of tracing")
}

// TestContextFromDocumentIgnoresEveryFlag pins D10: the package-level document
// helpers carry no gate at all. They start no span, build no attributes,
// initialise nothing in the OTel SDK and write nothing — they read a field out
// of a value the caller already holds. A revocation therefore does NOT stop
// trace-context extraction, which is what makes this pair the supported way to
// keep linking while the library is silenced.
func TestContextFromDocumentIgnoresEveryFlag(t *testing.T) {
	doc := map[string]any{
		shared.TraceMetadataKey: map[string]any{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
	}

	for _, tc := range []struct {
		name                  string
		global, tracing, prop string
		relayTracing          bool
		relayProp             bool
	}{
		{name: "everything on", global: "1", tracing: "1", prop: "1", relayTracing: true, relayProp: true},
		{name: "relay revoked propagation", global: "1", tracing: "1", prop: "1", relayTracing: true},
		{name: "relay revoked tracing", global: "1", tracing: "1", prop: "1"},
		{name: "module switches off", global: "1", tracing: "false", prop: "false", relayTracing: true, relayProp: true},
		{name: "master veto", global: "false", tracing: "false", prop: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envGlobalTracingEnabled, tc.global)
			t.Setenv(envMongoTracingEnabled, tc.tracing)
			t.Setenv(envMongoPropagationEnabled, tc.prop)
			setRelay(t, tc.relayTracing, tc.relayProp)

			_, ok := ContextFromDocument(context.Background(), doc)
			assert.True(t, ok, "extraction is ungated and must succeed in every configuration")
		})
	}
}

func TestChangeStreamSwitchesImplMidIteration(t *testing.T) {
	capableEnv(t)
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
	capableEnv(t)
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
	silentRelayEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))

	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	tc, ok := coll.impl().(*traced.Collection)
	require.True(t, ok, "env vars on → instrumented")
	assert.True(t, tc.PropagationEnabled())

	// Environment variables are read ONCE, at construction: none is touched on a
	// hot path, so a later change needs a new wrapper. Only the relay is
	// observable without reconstructing.
	t.Setenv(envMongoTracingEnabled, "false")
	assert.IsType(t, &traced.Collection{}, coll.impl(),
		"the module env var is fixed at construction; changing it must not affect a live Collection")

	rebuilt := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	assert.IsType(t, &direct.Collection{}, rebuilt.impl(),
		"a wrapper built after the change takes the passthrough path")
}

// TestNoRelayPossibleAllocatesNothingInstrumented is the zero-cost path: with no
// endpoint and no provider, a switched-off client must not even build the
// instrumented implementations, nor register the command monitor.
func TestNoRelayPossibleAllocatesNothingInstrumented(t *testing.T) {
	silentRelayEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))

	g := envGateState(t)
	require.False(t, g.relayPossible)
	assert.False(t, g.tracedPossible(),
		"no relay can exist and the local answer is off → no OTel path may be reachable")

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	assert.Nil(t, coll.traced)
}

// TestWrapperBuiltBeforeTheProviderStaysStatic pins the ordering rule an
// application is most likely to trip over. relayPossible is resolved once, at
// construction; a wrapper built while no relay could exist resolves from its
// environment and options for the rest of its life, and installing a provider
// afterwards never reaches it.
func TestWrapperBuiltBeforeTheProviderStaysStatic(t *testing.T) {
	silentRelayEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	t.Setenv(envMongoTracingEnabled, "1")

	tracer := noop.NewTracerProvider().Tracer("test")
	coll := NewCollection(&mongo.Collection{}, tracer, otel.GetTextMapPropagator())
	require.IsType(t, &traced.Collection{}, coll.impl(), "the environment enabled it")

	setRelay(t, false, false) // installed too late

	assert.IsType(t, &traced.Collection{}, coll.impl(),
		"a Collection built before any relay existed never consults one")
	rebuilt := NewCollection(&mongo.Collection{}, tracer, otel.GetTextMapPropagator())
	assert.IsType(t, &direct.Collection{}, rebuilt.impl(),
		"one built after the install does observe it — the fix is ordering, not a reset hook")
}

// TestTwoClientsCanDiffer is what the option is uniquely able to express, and it
// survives the option moving below the environment variable — on the condition
// that the deployment leaves that variable unset.
func TestTwoClientsCanDiffer(t *testing.T) {
	silentRelayEnv(t)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))

	on, err := resolveGates(boolPtr(true), nil)
	require.NoError(t, err)
	off, err := resolveGates(boolPtr(false), nil)
	require.NoError(t, err)

	assert.True(t, on.effectiveTracing(), "the client built with WithTracingEnabled(true) must trace")
	assert.False(t, off.effectiveTracing(), "…and the one built with (false) must not")
}
