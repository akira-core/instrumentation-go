package otelnats

import (
	"errors"
	"os"
	"testing"

	nats "github.com/nats-io/nats.go"
)

// TestTracingTiers_EnvOptionMatrix pins how the three tiers compose.
//
// gate1 has two spellings — OTEL_INSTRUMENTATION_GO_TRACING_ENABLED and
// WithTracingEnabled — and supplying both is a configuration error (D3), so the
// table has no "both set" success row. The module switch is a separate,
// conjunctive tier: gate1 alone is never enough, and with it off only the
// passthrough implementation is constructed.
func TestTracingTiers_EnvOptionMatrix(t *testing.T) {
	type v struct {
		set   bool
		value string
	}
	unset := v{}
	on := v{set: true, value: "1"}
	off := v{set: true, value: "false"}

	cases := []struct {
		name         string
		global       v
		module       v
		option       *bool
		want         bool
		wantConflict bool
	}{
		{name: "nothing set", global: unset, module: unset, want: false},
		{name: "global on, module on", global: on, module: on, want: true},
		{name: "global on, module off", global: on, module: off, want: false},
		{name: "global on, module unset", global: on, module: unset, want: false},
		{name: "global off, module on", global: off, module: on, want: false},

		{name: "option on supplies gate1", global: unset, module: on, option: boolPtr(true), want: true},
		{name: "option off supplies gate1", global: unset, module: on, option: boolPtr(false), want: false},
		{name: "option on still needs the module tier", global: unset, module: off, option: boolPtr(true), want: false},

		{name: "both spellings of gate1 conflict", global: on, module: on, option: boolPtr(true), wantConflict: true},
		{name: "conflict even when they agree", global: off, module: on, option: boolPtr(false), wantConflict: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, envGlobalTracingEnabled, tc.global.set, tc.global.value)
			setOrUnset(t, envNATSTracingEnabled, tc.module.set, tc.module.value)

			var opts []Option
			if tc.option != nil {
				opts = []Option{WithTracingEnabled(*tc.option)}
			}

			conn, err := newConn(&nats.Conn{}, opts...)
			if tc.wantConflict {
				if !errors.Is(err, ErrTracingConfigConflict) {
					t.Fatalf("error = %v, want ErrTracingConfigConflict", err)
				}
				return
			}
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

func boolPtr(b bool) *bool { return &b }

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
