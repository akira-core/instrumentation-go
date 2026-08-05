package otelmongo

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// These tests pin how this module composes the ladder — relay > env > option >
// default — and the master switch ANDed above it. The truthiness rules
// themselves live in otel-flags and are tested there.
//
// Not parallel-safe: the process environment and the OpenFeature provider are
// both process-global. No t.Parallel in this file.

// envGlobalTracingEnabled aliases the master switch's variable for the tests
// that were written against it. The production code no longer names it: it is
// process-scoped, so it belongs to otel-flags.
const envGlobalTracingEnabled = otelflags.EnvGlobalTracing

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

// silentFlagEnv clears every switch these tests touch, including the endpoint,
// so a case starts from "no relay, no opinion".
func silentFlagEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envGlobalTracingEnabled,
		envMongoTracingEnabled,
		envMongoPropagationEnabled,
		otelflags.EnvFlagsEndpoint,
		otelflags.EnvFlagsPollInterval,
	} {
		setOrUnset(t, name, false, "")
	}
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

// TestTracing_TierMatrix pins how the master switch and this module's tracing
// switch compose, with no relay configured so the local answer is final.
//
// Two rows reverse earlier releases: the module variable alone is now enough
// (the master defaults to enabled), and the environment variable beats the
// option (so there is no conflict row).
func TestTracing_TierMatrix(t *testing.T) {
	cases := []struct {
		name   string
		master envValue
		module envValue
		option *bool
		want   bool
	}{
		{name: "nothing set", master: envUnset, module: envUnset, want: false},

		{name: "master on alone", master: envOn, module: envUnset, want: false},
		{name: "master on, module on", master: envOn, module: envOn, want: true},
		{name: "master on, module off", master: envOn, module: envOff, want: false},
		{name: "master off, module on", master: envOff, module: envOn, want: false},
		{name: "master off, option on", master: envOff, module: envUnset, option: boolPtr(true), want: false},

		{name: "module on alone", master: envUnset, module: envOn, want: true},

		{name: "option on, variable unset", master: envUnset, module: envUnset, option: boolPtr(true), want: true},
		{name: "option off, variable unset", master: envUnset, module: envUnset, option: boolPtr(false), want: false},
		{name: "variable off beats option on", master: envUnset, module: envOff, option: boolPtr(true), want: false},
		{name: "variable on beats option off", master: envUnset, module: envOn, option: boolPtr(false), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			silentFlagEnv(t)
			setOrUnset(t, envGlobalTracingEnabled, tc.master.set, tc.master.value)
			setOrUnset(t, envMongoTracingEnabled, tc.module.set, tc.module.value)

			g, err := resolveGates(tc.option, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, g.effectiveTracing())
		})
	}
}

// TestPropagationTier_Matrix pins the second Mongo switch, which follows the
// same ladder with its own default of false.
func TestPropagationTier_Matrix(t *testing.T) {
	cases := []struct {
		name   string
		env    envValue
		option *bool
		want   bool
	}{
		{name: "unset", env: envUnset, want: false},
		{name: "env on", env: envOn, want: true},
		{name: "env off", env: envOff, want: false},
		{name: "option on, env unset", env: envUnset, option: boolPtr(true), want: true},
		{name: "option off, env unset", env: envUnset, option: boolPtr(false), want: false},
		{name: "env off beats option on", env: envOff, option: boolPtr(true), want: false},
		{name: "env on beats option off", env: envOn, option: boolPtr(false), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			silentFlagEnv(t)
			t.Setenv(envMongoTracingEnabled, "1")
			setOrUnset(t, envMongoPropagationEnabled, tc.env.set, tc.env.value)

			g, err := resolveGates(nil, tc.option)
			require.NoError(t, err)
			assert.Equal(t, tc.want, g.effectivePropagation())
		})
	}
}

// TestOperatorCanStopDocumentWrites is why the option sits below the environment
// variable. Every other switch only produces or withholds telemetry; this one
// appends a permanent field to the operator's own documents, so the operator
// needs a setting the application's Go code cannot override.
func TestOperatorCanStopDocumentWrites(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "false")

	g, err := resolveGates(nil, boolPtr(true))
	require.NoError(t, err)
	assert.True(t, g.effectiveTracing(), "tracing itself is unaffected")
	assert.False(t, g.effectivePropagation(),
		"WithTracePropagationEnabled(true) must not bypass OTEL_MONGO_PROPAGATION_ENABLED=false")
}

// TestAbsenceNeverEnablesPropagation: nothing may write _oteltrace unless some
// source says so explicitly.
func TestAbsenceNeverEnablesPropagation(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "1")

	g, err := resolveGates(nil, nil)
	require.NoError(t, err)
	require.True(t, g.effectiveTracing())
	assert.False(t, g.effectivePropagation(),
		"the propagation default must be false, so absence can never start writing into documents")
}

// TestResolveGates_ReportsEveryBadValueAtOnce pins that a deployment carrying
// several unreadable values learns about all of them in one run.
func TestResolveGates_ReportsEveryBadValueAtOnce(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envGlobalTracingEnabled, "maybe")
	t.Setenv(envMongoTracingEnabled, "perhaps")
	t.Setenv(envMongoPropagationEnabled, "")

	_, err := resolveGates(nil, nil)
	require.ErrorIs(t, err, otelflags.ErrInvalidFlagValue)
	for _, name := range []string{envGlobalTracingEnabled, envMongoTracingEnabled, envMongoPropagationEnabled} {
		assert.Contains(t, err.Error(), name, "the joined error must name every bad variable")
	}
}

// TestResolveGates_InvalidProviderConfigFailsConstruction extends the same rule
// to the variables otel-flags owns.
//
// They are process-scoped and this module never names them, but they are read
// during construction, so a value nobody can interpret must stop the constructor
// here as surely as one of this module's own does.
func TestResolveGates_InvalidProviderConfigFailsConstruction(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{name: "a bare integer poll interval", env: otelflags.EnvFlagsPollInterval, value: "60"},
		{name: "an endpoint with no scheme", env: otelflags.EnvFlagsEndpoint, value: "relay:1031"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			silentFlagEnv(t)
			t.Setenv(tc.env, tc.value)

			_, err := resolveGates(nil, nil)
			require.ErrorIs(t, err, otelflags.ErrInvalidFlagValue)
			assert.Contains(t, err.Error(), tc.env)
		})
	}
}

// TestResolveGates_InvalidValueFailsEvenWithAnOption pins that the option does
// not excuse an unreadable variable that outranks it.
func TestResolveGates_InvalidValueFailsEvenWithAnOption(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoPropagationEnabled, "enabled")

	_, err := resolveGates(nil, boolPtr(true))
	require.ErrorIs(t, err, otelflags.ErrInvalidFlagValue)
	assert.Contains(t, err.Error(), envMongoPropagationEnabled)
}

func TestEnvGates_ResolvesFromTheEnvironmentAlone(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	g := envGates()
	assert.True(t, g.effectiveTracing())
	assert.True(t, g.effectivePropagation())
}

// TestEnvGates_FallsBackToDisabledOnAnInvalidValue pins the one place an
// unreadable value cannot be reported: NewCollection accepts no options and has
// no error return, so it degrades to fully disabled rather than guessing.
func TestEnvGates_FallsBackToDisabledOnAnInvalidValue(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "enabled")

	g := envGates()
	assert.False(t, g.effectiveTracing())
	assert.False(t, g.tracedPossible())
}

// TestPropagationIsForceDisabledWhenTracingIsOff pins the single-switch rule:
// Mongo tracing and Mongo document propagation cannot disagree, however the
// tracing "off" came about.
func TestPropagationIsForceDisabledWhenTracingIsOff(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoPropagationEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "false")

	g := envGates()
	require.False(t, g.effectiveTracing())
	require.True(t, g.propLocal, "the propagation tier itself is on…")
	assert.False(t, g.effectivePropagation(), "…but tracing off force-disables it")
}

func TestPropagationWhenTracing_DoesNotReResolveTracing(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")

	g := envGates()
	// propagationWhenTracing is what the instrumented impls hold. They are
	// reached only after the facade resolved tracing true for this operation, so
	// it must equal propagationGiven(true) exactly (design R5).
	assert.Equal(t, g.propagationGiven(true), g.propagationWhenTracing())
}

// TestConnectRejectsUnreadableConfiguration pins that the check runs before any
// connection is opened.
func TestConnectRejectsUnreadableConfiguration(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(envMongoTracingEnabled, "yes please")

	_, err := ConnectWithOptions([]ClientOption{WithTracingEnabled(true)})
	require.ErrorIs(t, err, otelflags.ErrInvalidFlagValue)
}

// TestTracedPossible_NoRelayKeepsTheZeroCostPath pins the allocation rule.
func TestTracedPossible_NoRelayKeepsTheZeroCostPath(t *testing.T) {
	silentFlagEnv(t)

	g, err := resolveGates(nil, nil)
	require.NoError(t, err)
	require.False(t, g.relayPossible)
	assert.False(t, g.tracedPossible(),
		"no relay can exist and the local answer is off → nothing instrumented, and no command monitor")
}

// TestTracedPossible_RelayForcesAllocation is the other half: the relay can
// enable, so the instrumented implementations must exist even with the
// environment silent.
func TestTracedPossible_RelayForcesAllocation(t *testing.T) {
	silentFlagEnv(t)
	t.Setenv(otelflags.EnvFlagsEndpoint, "http://127.0.0.1:1")

	g, err := resolveGates(nil, nil)
	require.NoError(t, err)
	require.True(t, g.relayPossible)
	assert.True(t, g.tracedPossible(),
		"a later relay enable could never take effect if nothing instrumented were built")
}
