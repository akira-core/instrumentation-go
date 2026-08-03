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

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/flags"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/traced"
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
	require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain,
		memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{
				flagKeyMongoTracing:     boolFlag(tracing),
				flagKeyMongoPropagation: boolFlag(propagation),
			},
		)))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain, openfeature.NoopProvider{}))
	})
}

// capableEnv turns on every environment-derived tier, so each assertion that
// follows is attributable to the relay and not to the environment. All three
// are required: since D7 the module switches decide which implementations are
// constructed at all, and the relay can only revoke below them.
func capableEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
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

func TestGlobalKillSwitchBeatsTheRelay(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "true")
	t.Setenv(envMongoPropagationEnabled, "true")
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

// TestWithTracingEnabledSuppliesGate1AndDoesNotPin replaces the superseded
// "an overridden client is static" behaviour. The option is now one spelling of
// the first tier: it decides whether the instrumented implementations are built
// at all, and says nothing about the relay.
func TestWithTracingEnabledSuppliesGate1AndDoesNotPin(t *testing.T) {
	// Global variable left UNSET: since D3, setting it as well as passing the
	// option is a configuration error.
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	t.Run("option true still obeys a revocation", func(t *testing.T) {
		setRelay(t, true, true)
		on := true
		g, err := resolveGates(&on, nil)
		require.NoError(t, err)
		assert.True(t, g.tracedBuilt, "gate1 supplied by the option → instrumented impls are built")
		assert.True(t, g.effectiveTracing())

		setRelay(t, false, false)
		assert.False(t, g.effectiveTracing(),
			"the option supplies gate1 only — there is no way to opt a client out of a revocation")
	})

	t.Run("option false builds only the passthrough", func(t *testing.T) {
		setRelay(t, true, true)
		off := false
		g, err := resolveGates(&off, nil)
		require.NoError(t, err)
		assert.False(t, g.tracedBuilt, "gate1 off must not allocate the instrumented impls")
		assert.False(t, g.effectiveTracing())
	})
}

func TestConfigConflictsAreReportedTogether(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	on := true
	_, err := resolveGates(&on, &on)
	require.ErrorIs(t, err, ErrTracingConfigConflict)
	require.ErrorIs(t, err, ErrTracePropagationConfigConflict,
		"both checks must run so a caller violating both rules learns both at once")
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
		{name: "global kill switch off", global: "false", tracing: "false", prop: "false"},
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
	require.NoError(t, openfeature.SetNamedProviderAndWait(flags.FlagDomain, openfeature.NoopProvider{}))

	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	coll := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	tc, ok := coll.impl().(*traced.Collection)
	require.True(t, ok, "env vars on → instrumented")
	assert.True(t, tc.PropagationEnabled())

	// The module switch is read ONCE, at construction (D7/D8): no environment
	// variable is touched on a hot path any more, so a later change needs a new
	// wrapper. Only the relay verdict is observable without reconstructing.
	t.Setenv(envMongoTracingEnabled, "false")
	assert.IsType(t, &traced.Collection{}, coll.impl(),
		"the module env var is fixed at construction; changing it must not affect a live Collection")

	rebuilt := NewCollection(&mongo.Collection{}, noop.NewTracerProvider().Tracer("test"), otel.GetTextMapPropagator())
	assert.IsType(t, &direct.Collection{}, rebuilt.impl(),
		"a wrapper built after the change takes the passthrough path")
}

// TestKillSwitchOffIsUnreachableFromTheRelay is the C6 regression in its
// current form: with the global kill switch off, nothing the relay serves can
// reach this process — not tracing, and not the propagation that would write
// _oteltrace into the caller's documents.
func TestKillSwitchOffIsUnreachableFromTheRelay(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
	setRelay(t, true, true)

	g := envGateState(t)
	assert.False(t, g.tracedBuilt, "kill switch off → no instrumented impl is constructed")
	assert.False(t, g.effectiveTracing(), "…so no relay value can produce a span")
	assert.False(t, g.effectivePropagation(), "…nor a byte of _oteltrace")
}
