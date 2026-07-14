package otelmongo

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// clearMongoTracingEnv unsets all three tracing env vars for the duration of
// the test, restoring their prior values on cleanup. mongoTracingEnabled is a
// plain, uncached function (unlike otel-nats/otel-gorilla-ws's Gate), but the
// process-wide propEnabledGate does cache these vars, so the cache is reset
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
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)
}

// TestConnectWithOptions_TracingEnabledOption_True_OverridesEnvUnset verifies
// WithTracingEnabled(true) is authoritative when all tracing env vars are
// unset: the Client traces and its Collections select the traced impl.
func TestConnectWithOptions_TracingEnabledOption_True_OverridesEnvUnset(t *testing.T) {
	clearMongoTracingEnv(t)
	uri := requireMongoDB(t)

	c, err := ConnectWithOptions(context.Background(), []ClientOption{WithTracingEnabled(true)}, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.True(t, c.tracingEnabled, "option must override the disabled env gate")

	coll := c.Database("otelmongo_test").Collection("option_true_overrides_env")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })
	assert.IsType(t, &traced.Collection{}, coll.impl, "Collection must select the traced impl")
}

// TestConnectWithOptions_TracingEnabledOption_False_OverridesEnvTruthy
// verifies WithTracingEnabled(false) is authoritative when all tracing env
// vars are truthy: the Client does not trace, its Collections select the
// direct impl, and WithTracePropagationEnabled(true) cannot enable
// propagation despite tracing being force-disabled by the option.
func TestConnectWithOptions_TracingEnabledOption_False_OverridesEnvTruthy(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)
	uri := requireMongoDB(t)

	c, err := ConnectWithOptions(context.Background(), []ClientOption{
		WithTracingEnabled(false),
		WithTracePropagationEnabled(true),
	}, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.False(t, c.tracingEnabled, "option must override the truthy env gates")
	assert.False(t, c.propagationEnabled, "propagation cannot be enabled when effective tracing is off, even via WithTracePropagationEnabled")

	coll := c.Database("otelmongo_test").Collection("option_false_overrides_env")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })
	assert.IsType(t, &direct.Collection{}, coll.impl, "Collection must select the direct impl")
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
	uri := requireMongoDB(t)

	c, err := ConnectWithOptions(context.Background(), []ClientOption{
		WithTracingEnabled(true),
		WithTracePropagationEnabled(true),
	}, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.True(t, c.tracingEnabled)
	assert.True(t, c.propagationEnabled, "WithTracePropagationEnabled must take effect when WithTracingEnabled(true) supplies the effective tracing state, even though the env gates are unset")
}

// TestConnectWithOptions_TracingEnabledOption_Absent_MatchesEnvGate verifies
// omitting the option preserves existing env-gate-only behavior bit-for-bit,
// in both directions.
func TestConnectWithOptions_TracingEnabledOption_Absent_MatchesEnvGate(t *testing.T) {
	uri := requireMongoDB(t)

	t.Run("env disabled", func(t *testing.T) {
		clearMongoTracingEnv(t)
		c, err := ConnectWithOptions(context.Background(), nil, options.Client().ApplyURI(uri))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
		assert.False(t, c.tracingEnabled)
	})

	t.Run("env enabled", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "1")
		resetPropEnabledCacheForTest()
		t.Cleanup(resetPropEnabledCacheForTest)
		c, err := ConnectWithOptions(context.Background(), nil, options.Client().ApplyURI(uri))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
		assert.True(t, c.tracingEnabled)
	})
}

// TestConnectWithOptions_DoesNotMutateCallerOptions pins v1/v2 parity for the
// v2 MergeClientOptions short-circuit fix: v1's MergeClientOptions always
// builds a fresh struct, so the caller's Monitor field must stay untouched
// here too. Connect is lazy in v1, so this needs no live server.
func TestConnectWithOptions_DoesNotMutateCallerOptions(t *testing.T) {
	clearMongoTracingEnv(t)

	userMonitor := &event.CommandMonitor{}
	callerOpts := options.Client().ApplyURI("mongodb://127.0.0.1:27017").SetMonitor(userMonitor)

	c, err := ConnectWithOptions(context.Background(), []ClientOption{WithTracingEnabled(true)}, callerOpts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })

	assert.Same(t, userMonitor, callerOpts.Monitor,
		"ConnectWithOptions must not overwrite the caller's Monitor field")
}

// TestContextFromDocument_IgnoresPerClientOption verifies the package-level
// ContextFromDocument gate stays env-only: a document written by a client
// tracing via WithTracingEnabled(true) (env gates off) still fails
// extraction, because ContextFromDocument resolves its own independent,
// process-wide cached gate rather than any per-client state.
func TestContextFromDocument_IgnoresPerClientOption(t *testing.T) {
	clearMongoTracingEnv(t)
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)

	doc := bson.M{"_oteltrace": bson.M{"traceparent": "00-11111111111111111111111111111111-2222222222222222-01"}}
	_, ok := ContextFromDocument(context.Background(), doc)
	assert.False(t, ok, "ContextFromDocument must stay disabled per the env-only cached gate regardless of any per-client WithTracingEnabled option")
}

// TestNewClientConfig_SkipsNilOptions pins nil-tolerance of the option
// parser: nil entries are skipped, non-nil ones apply.
func TestNewClientConfig_SkipsNilOptions(t *testing.T) {
	cfg := newClientConfig([]ClientOption{nil, WithTracingEnabled(true), nil})
	require.NotNil(t, cfg.TracingEnabled)
	assert.True(t, *cfg.TracingEnabled)
}
