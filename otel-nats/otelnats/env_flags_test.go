package otelnats

import (
	"os"
	"testing"

	nats "github.com/nats-io/nats.go"
)

// natsCapable resolves the module's static ceiling from the environment: gate1
// AND OTEL_NATS_TRACING_ENABLED. The truthiness rules themselves live in
// internal/flags and are tested there; these cases pin how this module composes
// the two tiers.
func natsCapable(t *testing.T) bool {
	t.Helper()
	possible, err := tracedPossible(nil)
	if err != nil {
		t.Fatalf("tracedPossible: %v", err)
	}
	return possible
}

func TestNATSCapability_DefaultFalse(t *testing.T) {
	prev, existed := os.LookupEnv(envNATSTracingEnabled)
	_ = os.Unsetenv(envNATSTracingEnabled)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envNATSTracingEnabled, prev)
		} else {
			_ = os.Unsetenv(envNATSTracingEnabled)
		}
	})
	if natsCapable(t) {
		t.Fatal("expected tracing disabled when the module env var is unset")
	}
}

// TestNATSCapability_EmptyStringIsDisabled is the BREAKING truthiness change:
// `export VAR=` used to open the gate and now closes it.
func TestNATSCapability_EmptyStringIsDisabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "")
	t.Setenv(envNATSTracingEnabled, "")
	if natsCapable(t) {
		t.Fatal("expected an empty string to mean disabled")
	}
}

func TestNATSCapability_FalseTokens(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Setenv(envNATSTracingEnabled, v)
		if natsCapable(t) {
			t.Fatalf("expected disabled for value %q", v)
		}
	}
}

func TestNATSCapability_GlobalOffOverridesModule(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envNATSTracingEnabled, "true")
	if natsCapable(t) {
		t.Fatal("expected the global kill switch to disable nats tracing")
	}
}

func TestNATSCapability_RequiresBothTiers(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envNATSTracingEnabled, "1")
	if !natsCapable(t) {
		t.Fatal("expected capability with both tiers on")
	}
}

// TestNewConn_TracingDisabled_UsesDirectConn covers the disabled-mode
// invariant at the Conn level: with the tracing gate off, newConn must
// select directConn (no spans, no propagator, no deliver TracerProvider —
// the latter no longer exists in the package at all).
func TestNewConn_TracingDisabled_UsesDirectConn(t *testing.T) {
	prevGlobal, globalExisted := os.LookupEnv(envGlobalTracingEnabled)
	prevNATS, natsExisted := os.LookupEnv(envNATSTracingEnabled)
	_ = os.Unsetenv(envGlobalTracingEnabled)
	_ = os.Unsetenv(envNATSTracingEnabled)
	t.Cleanup(func() {
		if globalExisted {
			_ = os.Setenv(envGlobalTracingEnabled, prevGlobal)
		} else {
			_ = os.Unsetenv(envGlobalTracingEnabled)
		}
		if natsExisted {
			_ = os.Setenv(envNATSTracingEnabled, prevNATS)
		} else {
			_ = os.Unsetenv(envNATSTracingEnabled)
		}
	})

	conn, err := newConn(&nats.Conn{})
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	if _, ok := conn.impl().(*directConn); !ok {
		t.Fatalf("expected *directConn impl when tracing gate is off, got %T", conn.impl())
	}
	if conn.TracingEnabled() {
		t.Fatal("expected TracingEnabled() false")
	}
}
