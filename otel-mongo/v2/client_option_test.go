package otelmongo

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/traced"
)

// clearMongoTracingEnv unsets all three tracing env vars for the duration of
// the test, restoring their prior values on cleanup. mongoTracingEnabled is a
// function that resolves through the module Resolver (1s TTL). The resolver
// snapshot caches these vars, so the cache is reset
// here and again on cleanup (per the CLAUDE.md rule for tests that toggle
// them).
func clearMongoTracingEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envGlobalTracingEnabled, envMongoTracingEnabled, envMongoPropagationEnabled} {
		prev, existed := os.LookupEnv(name)
		_ = os.Unsetenv(name)
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, prev)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

// TestConnectWithOptions_TracingEnabledOption_True_SuppliesGate1 verifies
// WithTracingEnabled(true) is authoritative when all tracing env vars are
// unset: the Client traces and its Collections select the traced impl.
func TestConnectWithOptions_TracingEnabledOption_True_SuppliesGate1(t *testing.T) {
	clearMongoTracingEnv(t)
	// The option supplies gate1; OTEL_MONGO_TRACING_ENABLED is the separate,
	// conjunctive tier that decides whether the instrumented impls are built.
	t.Setenv(envMongoTracingEnabled, "1")
	uri := requireMongoDB(t)

	c, err := ConnectWithOptions([]ClientOption{WithTracingEnabled(true)}, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.True(t, c.effectiveTracing(), "option must override the disabled env gate")

	coll := c.Database("otelmongo_test").Collection("option_true_overrides_env")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })
	assert.IsType(t, &traced.Collection{}, coll.impl(), "Collection must select the traced impl")
}

// TestConnectWithOptions_TracingEnabledOption_False_BuildsPassthrough
// verifies WithTracingEnabled(false) is authoritative when all tracing env
// vars are truthy: the Client does not trace, its Collections select the
// direct impl, and WithTracePropagationEnabled(true) cannot enable
// propagation despite tracing being force-disabled by the option.
func TestConnectWithOptions_TracingEnabledOption_False_BuildsPassthrough(t *testing.T) {
	// Both env variables that have an option counterpart are left UNSET: since
	// D3 each pair is two spellings of one switch, and supplying both is a
	// configuration error.
	t.Setenv(envMongoTracingEnabled, "1")
	uri := requireMongoDB(t)

	c, err := ConnectWithOptions([]ClientOption{
		WithTracingEnabled(false),
		WithTracePropagationEnabled(true),
	}, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.False(t, c.effectiveTracing(), "gate1 off → no instrumented impl, whichever spelling supplied it")
	assert.False(t, c.effectivePropagation(), "propagation cannot be enabled when effective tracing is off, even via WithTracePropagationEnabled")

	coll := c.Database("otelmongo_test").Collection("option_false_overrides_env")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })
	assert.IsType(t, &direct.Collection{}, coll.impl(), "Collection must select the direct impl")
}

// TestConnectWithOptions_TracingEnabledOption_True_PropagationOverrideWorks
// is the critical regression test for the resolveDocumentPropagation fix:
// before parameterizing it on the caller's effective tracing state,
// WithTracePropagationEnabled(true) combined with WithTracingEnabled(true)
// (env unset) would silently stay disabled, because
// resolveDocumentPropagation re-checked the env-only mongoTracingEnabled()
// internally instead of the Client's actual effective decision.
func TestConnectWithOptions_TracingEnabledOption_True_PropagationOverrideWorks(t *testing.T) {
	clearMongoTracingEnv(t)
	// The option supplies gate1; OTEL_MONGO_TRACING_ENABLED is the separate,
	// conjunctive tier that decides whether the instrumented impls are built.
	t.Setenv(envMongoTracingEnabled, "1")
	uri := requireMongoDB(t)

	c, err := ConnectWithOptions([]ClientOption{
		WithTracingEnabled(true),
		WithTracePropagationEnabled(true),
	}, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.True(t, c.effectiveTracing())
	assert.True(t, c.effectivePropagation(), "WithTracePropagationEnabled supplies the propagation tier when its env var is unset")
}

// TestConnectWithOptions_TracingEnabledOption_Absent_MatchesEnvGate verifies
// omitting the option preserves existing env-gate-only behavior bit-for-bit,
// in both directions.
func TestConnectWithOptions_TracingEnabledOption_Absent_MatchesEnvGate(t *testing.T) {
	uri := requireMongoDB(t)

	t.Run("env disabled", func(t *testing.T) {
		clearMongoTracingEnv(t)
		c, err := ConnectWithOptions(nil, options.Client().ApplyURI(uri))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
		assert.False(t, c.effectiveTracing())
	})

	t.Run("env enabled", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "1")
		c, err := ConnectWithOptions(nil, options.Client().ApplyURI(uri))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
		assert.True(t, c.effectiveTracing())
	})
}

// TestConnectWithOptions_DoesNotMutateCallerOptions pins the v2-only
// MergeClientOptions short-circuit: given exactly one options struct, the
// driver returns the caller's own pointer, so SetMonitor on the merged value
// used to overwrite the caller's Monitor field in place (and re-wrap it on a
// second Connect with the same struct). Connect is lazy in v2, so this needs
// no live server.
func TestConnectWithOptions_DoesNotMutateCallerOptions(t *testing.T) {
	clearMongoTracingEnv(t)

	userMonitor := &event.CommandMonitor{}
	callerOpts := options.Client().ApplyURI("mongodb://127.0.0.1:27017").SetMonitor(userMonitor)

	c, err := ConnectWithOptions([]ClientOption{WithTracingEnabled(true)}, callerOpts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.Same(t, userMonitor, callerOpts.Monitor,
		"ConnectWithOptions must not overwrite the caller's Monitor field")
}

// TestContextFromDocument_IgnoresEveryFlag verifies D10: the package-level
// helper carries no gate at all, so it neither consults per-client options nor
// any switch. A revocation does not stop trace-context extraction.
func TestContextFromDocument_IgnoresEveryFlag(t *testing.T) {
	clearMongoTracingEnv(t)

	doc := bson.M{"_oteltrace": bson.M{"traceparent": "00-11111111111111111111111111111111-2222222222222222-01"}}
	_, ok := ContextFromDocument(context.Background(), doc)
	assert.True(t, ok,
		"extraction is ungated: it emits nothing, and the caller asked for it at the call site")
}

// TestNewClientConfig_SkipsNilOptions pins nil-tolerance of the option
// parser: nil entries are skipped, non-nil ones apply.
func TestNewClientConfig_SkipsNilOptions(t *testing.T) {
	cfg := newClientConfig([]ClientOption{nil, WithTracingEnabled(true), nil})
	require.NotNil(t, cfg.TracingEnabled)
	assert.True(t, *cfg.TracingEnabled)
}
