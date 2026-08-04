package otelnats

import (
	"os"
	"testing"

	nats "github.com/nats-io/nats.go"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// TestTracingTiers_EnvOptionMatrix pins how the ladder and the master switch
// compose, with no relay configured so the local answer is final.
//
// Two things this table encodes that reverse earlier releases:
//
//   - The module variable alone is enough. The master switch defaults to
//     enabled, so it no longer has to be set for the module variable to work.
//   - The environment variable beats the option. There is no conflict row: an
//     option alongside its variable is ordinary configuration now, and the
//     variable wins.
func TestTracingTiers_EnvOptionMatrix(t *testing.T) {
	type v struct {
		set   bool
		value string
	}
	unset := v{}
	on := v{set: true, value: "1"}
	off := v{set: true, value: "false"}

	cases := []struct {
		name   string
		master v
		module v
		option *bool
		want   bool
	}{
		{name: "nothing set", master: unset, module: unset, want: false},

		// The master is a veto: setting it truthy changes nothing on its own.
		{name: "master on alone", master: on, module: unset, want: false},
		{name: "master on, module on", master: on, module: on, want: true},
		{name: "master on, module off", master: on, module: off, want: false},

		// The master vetoes everything below it, whatever spelling enabled it.
		{name: "master off, module on", master: off, module: on, want: false},
		{name: "master off, option on", master: off, module: unset, option: boolPtr(true), want: false},

		// The module variable alone now decides — the change from 0.7.0.
		{name: "module on alone", master: unset, module: on, want: true},

		// The option decides only when its variable is silent.
		{name: "option on, variable unset", master: unset, module: unset, option: boolPtr(true), want: true},
		{name: "option off, variable unset", master: unset, module: unset, option: boolPtr(false), want: false},
		{name: "variable off beats option on", master: unset, module: off, option: boolPtr(true), want: false},
		{name: "variable on beats option off", master: unset, module: on, option: boolPtr(false), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, otelflags.EnvFlagsEndpoint, false, "")
			setOrUnset(t, otelflags.EnvGlobalTracing, tc.master.set, tc.master.value)
			setOrUnset(t, envNATSTracingEnabled, tc.module.set, tc.module.value)

			var opts []Option
			if tc.option != nil {
				opts = []Option{WithTracingEnabled(*tc.option)}
			}

			conn, err := newConn(&nats.Conn{}, opts...)
			if err != nil {
				t.Fatalf("newConn: %v", err)
			}

			if got := conn.TracingEnabled(); got != tc.want {
				t.Fatalf("TracingEnabled() = %v, want %v", got, tc.want)
			}
			if tc.want {
				if _, ok := conn.impl().(*tracedConn); !ok {
					t.Fatalf("expected *tracedConn, got %T", conn.impl())
				}
			} else if _, ok := conn.impl().(*directConn); !ok {
				t.Fatalf("expected *directConn, got %T", conn.impl())
			}
		})
	}
}

// TestTwoConnectionsCanDiffer is what the option is uniquely able to express,
// and it survives the option moving below the environment variable — on the
// condition that the deployment leaves that variable unset, which under a
// default of false is the ordinary state.
func TestTwoConnectionsCanDiffer(t *testing.T) {
	setOrUnset(t, otelflags.EnvFlagsEndpoint, false, "")
	setOrUnset(t, otelflags.EnvGlobalTracing, false, "")
	setOrUnset(t, envNATSTracingEnabled, false, "")

	traced, err := newConn(&nats.Conn{}, WithTracingEnabled(true))
	if err != nil {
		t.Fatalf("newConn(traced): %v", err)
	}
	untraced, err := newConn(&nats.Conn{}, WithTracingEnabled(false))
	if err != nil {
		t.Fatalf("newConn(untraced): %v", err)
	}

	if !traced.TracingEnabled() {
		t.Error("the connection built with WithTracingEnabled(true) is not tracing")
	}
	if untraced.TracingEnabled() {
		t.Error("the connection built with WithTracingEnabled(false) is tracing")
	}
}

// TestOperatorCanDisableWhatTheCodeAskedFor is the reason the option sits below
// the environment variable.
func TestOperatorCanDisableWhatTheCodeAskedFor(t *testing.T) {
	setOrUnset(t, otelflags.EnvFlagsEndpoint, false, "")
	setOrUnset(t, otelflags.EnvGlobalTracing, false, "")
	t.Setenv(envNATSTracingEnabled, "false")

	conn, err := newConn(&nats.Conn{}, WithTracingEnabled(true))
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	if conn.TracingEnabled() {
		t.Fatal("a deployment could not disable a module the application hardcoded on")
	}
}

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

// TestNewConnConfig_SkipsNilOptions pins the ConnectTLS/ConnectWithCredentials
// regression where a literal nil variadic Option panicked in newConnConfig on
// every successful connection: nil entries are skipped, non-nil ones apply.
func TestNewConnConfig_SkipsNilOptions(t *testing.T) {
	cfg := newConnConfig(nil, WithTraceDestination("dest"), nil)
	if cfg.TraceDest != "dest" {
		t.Fatalf("expected TraceDest %q, got %q", "dest", cfg.TraceDest)
	}
}
