package otelnats

import (
	"os"
	"testing"

	nats "github.com/nats-io/nats.go"
)

func resetNATSGateForTest() {
	natsGate.ResetForTest()
}

func TestNATSTracingEnabled_DefaultFalse(t *testing.T) {
	prev, existed := os.LookupEnv(envNATSTracingEnabled)
	_ = os.Unsetenv(envNATSTracingEnabled)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envNATSTracingEnabled, prev)
		} else {
			_ = os.Unsetenv(envNATSTracingEnabled)
		}
	})
	resetNATSGateForTest()
	t.Cleanup(resetNATSGateForTest)
	if natsTracingEnabled() {
		t.Fatal("expected tracing disabled when env var is unset")
	}
}

func TestNATSTracingEnabled_EmptyStringIsEnabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "")
	t.Setenv(envNATSTracingEnabled, "")
	resetNATSGateForTest()
	t.Cleanup(resetNATSGateForTest)
	if !natsTracingEnabled() {
		t.Fatal("expected empty string to mean enabled")
	}
}

func TestNATSTracingEnabled_FalseTokens(t *testing.T) {
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Setenv(envNATSTracingEnabled, v)
		resetNATSGateForTest()
		if natsTracingEnabled() {
			t.Fatalf("expected disabled for value %q", v)
		}
	}
	t.Cleanup(resetNATSGateForTest)
}

func TestNATSTracingEnabled_GlobalOffOverridesModule(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envNATSTracingEnabled, "true")
	resetNATSGateForTest()
	t.Cleanup(resetNATSGateForTest)
	if natsTracingEnabled() {
		t.Fatal("expected global flag to disable nats tracing")
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
	resetNATSGateForTest()
	t.Cleanup(resetNATSGateForTest)

	conn := newConn(&nats.Conn{})
	if _, ok := conn.impl.(*directConn); !ok {
		t.Fatalf("expected *directConn impl when tracing gate is off, got %T", conn.impl)
	}
	if conn.TracingEnabled() {
		t.Fatal("expected TracingEnabled() false")
	}
}
