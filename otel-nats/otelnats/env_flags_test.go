package otelnats

import (
	"errors"
	"os"
	"strings"
	"testing"

	nats "github.com/nats-io/nats.go"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// No test in this file may call t.Parallel: the process environment and the
// OpenFeature provider registry are both global.
//
// These cases pin how this module composes the ladder — relay > env > option >
// default — and how it ANDs the master switch above it. The truthiness rules
// themselves live in otel-flags and are tested there.

// unsetFlagEnv clears every variable these tests touch, so a case starts from
// "the deployment expressed no opinion" whatever the surrounding environment is.
func unsetFlagEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		otelflags.EnvGlobalTracing,
		envNATSTracingEnabled,
		otelflags.EnvFlagsEndpoint,
	} {
		prev, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, prev)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

// localTracing resolves the module's effective tracing state from everything
// except a relay, which these tests deliberately leave absent.
func localTracing(t *testing.T, option *bool) bool {
	t.Helper()
	gate, err := resolveGates(option)
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	return gate.tracing()
}

func boolPtr(v bool) *bool { return &v }

func TestTracing_NothingConfiguredIsOff(t *testing.T) {
	unsetFlagEnv(t)
	if localTracing(t, nil) {
		t.Fatal("tracing on with nothing configured; the module default must be false")
	}
}

// TestTracing_ModuleVariableAloneEnables is the behaviour change against 0.7.0:
// the module variable used to be inert unless the global one was also set, and
// the master switch now defaults to enabled.
func TestTracing_ModuleVariableAloneEnables(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(envNATSTracingEnabled, "true")
	if !localTracing(t, nil) {
		t.Fatal("tracing off with the module variable truthy and the master unset; the master defaults to enabled")
	}
}

func TestTracing_MasterVariableAloneDoesNotEnable(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(otelflags.EnvGlobalTracing, "true")
	if localTracing(t, nil) {
		t.Fatal("tracing on with only the master variable set; the master is a veto, not an enabler")
	}
}

func TestTracing_MasterVetoBeatsEverythingLocal(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(otelflags.EnvGlobalTracing, "false")
	t.Setenv(envNATSTracingEnabled, "true")

	if localTracing(t, nil) {
		t.Fatal("the master veto did not stop a module its own variable enabled")
	}
	if localTracing(t, boolPtr(true)) {
		t.Fatal("the master veto did not stop a connection carrying WithTracingEnabled(true)")
	}
}

func TestTracing_OptionDecidesWhenTheEnvironmentIsSilent(t *testing.T) {
	unsetFlagEnv(t)
	if !localTracing(t, boolPtr(true)) {
		t.Fatal("WithTracingEnabled(true) did not enable tracing with the environment silent")
	}
	if localTracing(t, boolPtr(false)) {
		t.Fatal("WithTracingEnabled(false) did not disable tracing with the environment silent")
	}
}

// TestTracing_EnvironmentBeatsOption is the ordering that reverses 0.7.0, and
// the reason it exists: an operator must be able to disable one module without
// silencing the process and without a relay, even when the application's Go code
// asked for tracing.
func TestTracing_EnvironmentBeatsOption(t *testing.T) {
	t.Run("variable off overrides an enabling option", func(t *testing.T) {
		unsetFlagEnv(t)
		t.Setenv(envNATSTracingEnabled, "false")
		if localTracing(t, boolPtr(true)) {
			t.Fatal("the option won over the environment variable; the variable outranks it")
		}
	})

	t.Run("variable on overrides a disabling option", func(t *testing.T) {
		unsetFlagEnv(t)
		t.Setenv(envNATSTracingEnabled, "true")
		if !localTracing(t, boolPtr(false)) {
			t.Fatal("the option won over the environment variable; the variable outranks it")
		}
	})
}

func TestTracing_FalsyTokens(t *testing.T) {
	for _, v := range []string{"false", "0", "off", "no", "FALSE", " off "} {
		t.Run(v, func(t *testing.T) {
			unsetFlagEnv(t)
			t.Setenv(envNATSTracingEnabled, v)
			if localTracing(t, nil) {
				t.Fatalf("tracing on for value %q", v)
			}
		})
	}
}

func TestTracing_TruthyTokens(t *testing.T) {
	for _, v := range []string{"true", "1", "on", "yes", "TRUE", " On "} {
		t.Run(v, func(t *testing.T) {
			unsetFlagEnv(t)
			t.Setenv(envNATSTracingEnabled, v)
			if !localTracing(t, nil) {
				t.Fatalf("tracing off for value %q", v)
			}
		})
	}
}

// TestResolveGates_InvalidValueFailsConstruction covers the BREAKING change that
// can stop a process from starting. The empty string is included deliberately:
// an unexpanded ${SOMETHING} in a manifest reaches exactly that case.
func TestResolveGates_InvalidValueFailsConstruction(t *testing.T) {
	for _, v := range []string{"", "   ", "enabled", "2", "y", "t", "hello"} {
		t.Run("value="+v, func(t *testing.T) {
			unsetFlagEnv(t)
			t.Setenv(envNATSTracingEnabled, v)

			_, err := resolveGates(nil)
			if !errors.Is(err, otelflags.ErrInvalidFlagValue) {
				t.Fatalf("resolveGates err = %v, want ErrInvalidFlagValue", err)
			}
			if !strings.Contains(err.Error(), envNATSTracingEnabled) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}

// TestResolveGates_InvalidValueFailsEvenWithAnOption pins that the option does
// not excuse an unreadable variable that outranks it.
func TestResolveGates_InvalidValueFailsEvenWithAnOption(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(envNATSTracingEnabled, "enabled")

	if _, err := resolveGates(boolPtr(true)); !errors.Is(err, otelflags.ErrInvalidFlagValue) {
		t.Fatalf("resolveGates err = %v, want ErrInvalidFlagValue", err)
	}
}

// TestResolveGates_ReportsEveryBadValueAtOnce keeps a caller from fixing one
// variable and rediscovering the next on the following run.
func TestResolveGates_ReportsEveryBadValueAtOnce(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(otelflags.EnvGlobalTracing, "maybe")
	t.Setenv(envNATSTracingEnabled, "perhaps")

	_, err := resolveGates(nil)
	if !errors.Is(err, otelflags.ErrInvalidFlagValue) {
		t.Fatalf("resolveGates err = %v, want ErrInvalidFlagValue", err)
	}
	for _, name := range []string{otelflags.EnvGlobalTracing, envNATSTracingEnabled} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("joined error does not name %s: %v", name, err)
		}
	}
}

// TestTracedPossible_NoRelayKeepsTheZeroCostPath pins the allocation rule: with
// no relay configurable, a switched-off connection builds nothing instrumented.
func TestTracedPossible_NoRelayKeepsTheZeroCostPath(t *testing.T) {
	unsetFlagEnv(t)

	gate, err := resolveGates(nil)
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	if gate.relayPossible {
		t.Fatal("relayPossible with no endpoint and no provider bound")
	}
	if gate.tracedPossible() {
		t.Fatal("the instrumented implementation would be allocated for a connection that can never trace")
	}
}

// TestTracedPossible_RelayForcesAllocation is the other half: the relay can
// enable, so the instrumented implementation must exist even though the
// environment says off.
func TestTracedPossible_RelayForcesAllocation(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(otelflags.EnvFlagsEndpoint, "http://127.0.0.1:1")

	gate, err := resolveGates(nil)
	if err != nil {
		t.Fatalf("resolveGates: %v", err)
	}
	if !gate.relayPossible {
		t.Fatal("relayPossible = false with an endpoint configured")
	}
	if !gate.tracedPossible() {
		t.Fatal("no instrumented implementation would be allocated; a later relay enable could never take effect")
	}
}

// TestNewConn_TracingDisabled_UsesDirectConn covers the disabled-mode invariant
// at the Conn level: with everything off and no relay possible, newConn must
// select directConn — no spans, no propagator, no OTel SDK path at all.
func TestNewConn_TracingDisabled_UsesDirectConn(t *testing.T) {
	unsetFlagEnv(t)

	conn, err := newConn(&nats.Conn{})
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	if _, ok := conn.impl().(*directConn); !ok {
		t.Fatalf("expected *directConn impl when tracing is off, got %T", conn.impl())
	}
	if conn.traced != nil {
		t.Fatal("an instrumented implementation was allocated for a connection that can never trace")
	}
	if conn.TracingEnabled() {
		t.Fatal("expected TracingEnabled() false")
	}
}

// TestNewConn_OptionAloneTraces is the 0.7.0 usage that must keep working: no
// environment variables, one option, tracing on.
func TestNewConn_OptionAloneTraces(t *testing.T) {
	unsetFlagEnv(t)

	conn, err := newConn(&nats.Conn{}, WithTracingEnabled(true))
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	if !conn.TracingEnabled() {
		t.Fatal("WithTracingEnabled(true) with no environment variables did not trace")
	}
}

// TestNewConn_OptionAndVariableTogetherIsLegal covers the deleted
// mutual-exclusion rule: it is ordinary configuration now, and the variable wins.
func TestNewConn_OptionAndVariableTogetherIsLegal(t *testing.T) {
	unsetFlagEnv(t)
	t.Setenv(envNATSTracingEnabled, "false")

	conn, err := newConn(&nats.Conn{}, WithTracingEnabled(true))
	if err != nil {
		t.Fatalf("newConn returned an error for an option alongside its variable: %v", err)
	}
	if conn.TracingEnabled() {
		t.Fatal("the option won over the environment variable")
	}
}
