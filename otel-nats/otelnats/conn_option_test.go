package otelnats

import (
	"os"
	"testing"

	nats "github.com/nats-io/nats.go"
)

// clearTracingEnv unsets both tracing env vars for the duration of the test,
// restoring their prior values on cleanup, and resets the process-wide gate
// so the change is observed.
func clearTracingEnv(t *testing.T) {
	t.Helper()
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
}

// TestWithTracingEnabled_EnvOptionMatrix pins the full env × option decision
// table. Option is authoritative in either direction when present; when absent,
// the GLOBAL∧MODULE env gate decides (unset/falsy → off).
func TestWithTracingEnabled_EnvOptionMatrix(t *testing.T) {
	type envState string
	const (
		envUnset envState = "unset"
		envOn    envState = "on"
		envOff   envState = "off" // explicit falsy, not merely unset
	)
	type optState string
	const (
		optAbsent optState = "absent"
		optOn     optState = "on"
		optOff    optState = "off"
	)

	cases := []struct {
		name string
		env  envState
		opt  optState
		want bool
	}{
		{"env unset, option absent → off", envUnset, optAbsent, false},
		{"env unset, option on → on", envUnset, optOn, true},
		{"env unset, option off → off", envUnset, optOff, false},
		{"env on, option absent → on", envOn, optAbsent, true},
		{"env off, option absent → off", envOff, optAbsent, false},
		{"env on, option off → off", envOn, optOff, false},
		{"env off, option on → on", envOff, optOn, true},
		{"env on, option on → on", envOn, optOn, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.env {
			case envUnset:
				clearTracingEnv(t)
			case envOn:
				t.Setenv(envGlobalTracingEnabled, "1")
				t.Setenv(envNATSTracingEnabled, "1")
				resetNATSGateForTest()
				t.Cleanup(resetNATSGateForTest)
			case envOff:
				t.Setenv(envGlobalTracingEnabled, "false")
				t.Setenv(envNATSTracingEnabled, "false")
				resetNATSGateForTest()
				t.Cleanup(resetNATSGateForTest)
			}

			var opts []Option
			switch tc.opt {
			case optOn:
				opts = []Option{WithTracingEnabled(true)}
			case optOff:
				opts = []Option{WithTracingEnabled(false)}
			}

			conn := newConn(&nats.Conn{}, opts...)
			if got := conn.TracingEnabled(); got != tc.want {
				t.Fatalf("TracingEnabled() = %v, want %v", got, tc.want)
			}
			if tc.want {
				if _, ok := conn.impl.(*tracedConn); !ok {
					t.Fatalf("expected *tracedConn, got %T", conn.impl)
				}
			} else if _, ok := conn.impl.(*directConn); !ok {
				t.Fatalf("expected *directConn, got %T", conn.impl)
			}
		})
	}
}

// TestNewConnConfig_SkipsNilOptions pins the ConnectTLS/ConnectWithCredentials
// regression where a literal nil variadic Option panicked in newConnConfig on
// every successful connection: nil entries are skipped, non-nil ones apply.
func TestNewConnConfig_SkipsNilOptions(t *testing.T) {
	cfg := newConnConfig(nil, WithTraceDestination("dest"), nil)
	if cfg.TraceDest != "dest" {
		t.Fatalf("expected TraceDest %q, got %q", "dest", cfg.TraceDest)
	}
}
