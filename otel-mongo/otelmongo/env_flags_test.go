package otelmongo

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin how this module composes its tiers. The truthiness rules
// themselves live in internal/flags and are tested there.
//
// Not parallel-safe: the process environment and the OpenFeature provider are
// both process-global. No t.Parallel in this file.

// setOrUnset makes an environment variable present with value, or absent,
// restoring the previous state when the test ends.
func setOrUnset(t *testing.T, name string, set bool, value string) {
	t.Helper()
	if set {
		t.Setenv(name, value)
		return
	}
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

func boolPtr(b bool) *bool { return &b }

type envValue struct {
	set   bool
	value string
}

var (
	envUnset = envValue{}
	envOn    = envValue{set: true, value: "1"}
	envOff   = envValue{set: true, value: "false"}
)

// TestTracedBuilt_TierMatrix pins which configurations construct the
// instrumented implementations at all.
//
// gate1 has two spellings — OTEL_INSTRUMENTATION_GO_TRACING_ENABLED and
// WithTracingEnabled — and supplying both is a configuration error (D3), so the
// table has no "both set" success row. The module switch is a separate,
// conjunctive tier: gate1 alone is never enough.
func TestTracedBuilt_TierMatrix(t *testing.T) {
	cases := []struct {
		name         string
		global       envValue
		module       envValue
		option       *bool
		want         bool
		wantConflict bool
	}{
		{name: "nothing set", global: envUnset, module: envUnset, want: false},
		{name: "global on, module on", global: envOn, module: envOn, want: true},
		{name: "global on, module off", global: envOn, module: envOff, want: false},
		{name: "global on, module unset", global: envOn, module: envUnset, want: false},
		{name: "global off, module on", global: envOff, module: envOn, want: false},

		{name: "option on supplies gate1", global: envUnset, module: envOn, option: boolPtr(true), want: true},
		{name: "option off supplies gate1", global: envUnset, module: envOn, option: boolPtr(false), want: false},
		{name: "option on still needs the module tier", global: envUnset, module: envOff, option: boolPtr(true), want: false},

		{name: "both spellings of gate1 conflict", global: envOn, module: envOn, option: boolPtr(true), wantConflict: true},
		{name: "conflict even when they agree", global: envOff, module: envOn, option: boolPtr(false), wantConflict: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, envGlobalTracingEnabled, tc.global.set, tc.global.value)
			setOrUnset(t, envMongoTracingEnabled, tc.module.set, tc.module.value)
			setOrUnset(t, envMongoPropagationEnabled, false, "")

			g, err := resolveGates(tc.option, nil)
			if tc.wantConflict {
				require.ErrorIs(t, err, ErrTracingConfigConflict)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, g.tracedBuilt)
		})
	}
}

// TestPropagationTier_Matrix pins the second Mongo switch, which follows the
// same presence rule with its own sentinel.
func TestPropagationTier_Matrix(t *testing.T) {
	cases := []struct {
		name         string
		env          envValue
		option       *bool
		want         bool
		wantConflict bool
	}{
		{name: "unset", env: envUnset, want: false},
		{name: "env on", env: envOn, want: true},
		{name: "env off", env: envOff, want: false},
		{name: "option on, env unset", env: envUnset, option: boolPtr(true), want: true},
		{name: "option off, env unset", env: envUnset, option: boolPtr(false), want: false},
		{name: "env and option conflict", env: envOn, option: boolPtr(true), wantConflict: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envGlobalTracingEnabled, "1")
			t.Setenv(envMongoTracingEnabled, "1")
			setOrUnset(t, envMongoPropagationEnabled, tc.env.set, tc.env.value)

			g, err := resolveGates(nil, tc.option)
			if tc.wantConflict {
				require.ErrorIs(t, err, ErrTracePropagationConfigConflict)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, g.propagation)
		})
	}
}

// TestResolveGates_ReportsBothConflictsTogether pins that a caller violating
// both rules learns both at once, rather than fixing one and rediscovering the
// other on the next run.
func TestResolveGates_ReportsBothConflictsTogether(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	on := true
	_, err := resolveGates(&on, &on)
	require.ErrorIs(t, err, ErrTracingConfigConflict)
	require.ErrorIs(t, err, ErrTracePropagationConfigConflict)
	assert.Contains(t, err.Error(), envGlobalTracingEnabled)
	assert.Contains(t, err.Error(), envMongoPropagationEnabled)
}

func TestEnvGates_CannotFail(t *testing.T) {
	// Both env vars set, no options: a conflict needs an option to conflict with.
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	g := envGates()
	assert.True(t, g.tracedBuilt)
	assert.True(t, g.propagation)
}

// TestPropagationIsForceDisabledWhenTracingIsOff pins the single-kill-switch
// rule: Mongo tracing and Mongo document propagation cannot disagree, however
// the tracing "off" came about.
func TestPropagationIsForceDisabledWhenTracingIsOff(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "false")

	g := envGates()
	require.False(t, g.tracedBuilt)
	require.True(t, g.propagation, "the propagation tier itself is on…")
	assert.False(t, g.effectivePropagation(), "…but tracing off force-disables it")
}

func TestPropagationWhenTracing_DoesNotReResolveTracing(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	g := envGates()
	// propagationWhenTracing is what the instrumented impls hold. They are
	// reached only after the facade resolved tracing true for this operation, so
	// it must equal propagationGiven(true) exactly (design R5).
	assert.Equal(t, g.propagationGiven(true), g.propagationWhenTracing())
}

func TestConnectRejectsConflictingConfiguration(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")

	_, err := ConnectWithOptions(t.Context(), []ClientOption{WithTracingEnabled(true)})
	require.ErrorIs(t, err, ErrTracingConfigConflict,
		"the check must run before any connection is opened")
}
