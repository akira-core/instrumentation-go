package otelmongo

import (
	"os"
	"testing"
)

func TestMongoTracingEnabled_DefaultFalse(t *testing.T) {
	prev, existed := os.LookupEnv(envMongoTracingEnabled)
	_ = os.Unsetenv(envMongoTracingEnabled)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envMongoTracingEnabled, prev)
		} else {
			_ = os.Unsetenv(envMongoTracingEnabled)
		}
	})
	if mongoTracingEnabled() {
		t.Fatal("expected tracing disabled when env var is unset")
	}
}

func TestMongoTracingEnabled_EmptyStringIsEnabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "")
	t.Setenv(envMongoTracingEnabled, "")
	if !mongoTracingEnabled() {
		t.Fatal("expected empty string to mean enabled")
	}
}

func TestMongoTracingEnabled_FalseTokens(t *testing.T) {
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Setenv(envMongoTracingEnabled, v)
		if mongoTracingEnabled() {
			t.Fatalf("expected disabled for value %q", v)
		}
	}
}

func TestMongoTracingEnabled_GlobalOffOverridesModule(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "true")
	if mongoTracingEnabled() {
		t.Fatal("expected global flag to disable mongo tracing")
	}
}

func TestMongoPropagationEnabled(t *testing.T) {
	t.Run("unset global and propagation -> false", func(t *testing.T) {
		_ = os.Unsetenv(envGlobalTracingEnabled)
		_ = os.Unsetenv(envMongoPropagationEnabled)
		if mongoPropagationEnabled() {
			t.Fatal("expected propagation disabled when env vars are unset")
		}
	})

	t.Run("global on tracing off propagation on -> false", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "false")
		t.Setenv(envMongoPropagationEnabled, "true")
		if mongoPropagationEnabled() {
			t.Fatal("expected propagation disabled when mongo tracing is off")
		}
	})

	t.Run("global on tracing on propagation unset -> false", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		_ = os.Unsetenv(envMongoPropagationEnabled)
		if mongoPropagationEnabled() {
			t.Fatal("expected propagation disabled when module flag is unset")
		}
	})

	t.Run("global on tracing on propagation on -> true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		if !mongoPropagationEnabled() {
			t.Fatal("expected propagation enabled")
		}
	})
}

func TestResolveFlag(t *testing.T) {
	t.Run("override true wins env false", func(t *testing.T) {
		override := true
		if !resolveFlag(&override, false) {
			t.Fatal("expected override true to win")
		}
	})

	t.Run("override false wins env true", func(t *testing.T) {
		override := false
		if resolveFlag(&override, true) {
			t.Fatal("expected override false to win")
		}
	})
}

func TestConnectPropagationResolution(t *testing.T) {
	t.Run("global off cannot enable propagation via WithTracePropagationEnabled", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "false")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if resolveDocumentPropagation(mongoTracingEnabled(), cfg.PropagationEnabled) {
			t.Fatal("expected propagation disabled when global tracing is off")
		}
	})

	t.Run("mongo tracing off cannot enable propagation via WithTracePropagationEnabled", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "false")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if resolveDocumentPropagation(mongoTracingEnabled(), cfg.PropagationEnabled) {
			t.Fatal("expected propagation disabled when mongo tracing is off")
		}
	})

	t.Run("tracing on option false wins over env propagation true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(false)})
		if resolveDocumentPropagation(mongoTracingEnabled(), cfg.PropagationEnabled) {
			t.Fatal("expected WithTracePropagationEnabled(false) to disable propagation")
		}
	})

	t.Run("tracing on option true enables when env propagation unset", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		_ = os.Unsetenv(envMongoPropagationEnabled)
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if !resolveDocumentPropagation(mongoTracingEnabled(), cfg.PropagationEnabled) {
			t.Fatal("expected WithTracePropagationEnabled(true) to enable propagation when tracing is on")
		}
	})
}

// TestResolveDocumentPropagation_OptionDrivenTracing_NoEnv is the unskippable
// (no-Docker) guard for the CLAUDE.md-documented regression:
// resolveDocumentPropagation must trust the caller's already-resolved
// effective tracing state and must NOT recompute the env-only
// mongoTracingEnabled() internally — otherwise WithTracingEnabled(true) +
// WithTracePropagationEnabled(true) with all env gates unset silently stays
// disabled. The Docker-gated ConnectWithOptions variant of this test skips on
// machines without a MongoDB container; this one always runs.
func TestResolveDocumentPropagation_OptionDrivenTracing_NoEnv(t *testing.T) {
	clearMongoTracingEnv(t)

	on, off := true, false
	if !resolveDocumentPropagation(true, &on) {
		t.Fatal("effective tracing on + override true must enable propagation even with all env vars unset")
	}
	if resolveDocumentPropagation(true, nil) {
		t.Fatal("effective tracing on + no override must fall back to the (unset → disabled) env default")
	}
	if resolveDocumentPropagation(true, &off) {
		t.Fatal("override false must disable propagation")
	}
	if resolveDocumentPropagation(false, &on) {
		t.Fatal("effective tracing off must veto propagation regardless of the override")
	}
}

// effectiveClientFlags mirrors ConnectWithOptions' tracing/propagation
// resolution without opening a Mongo connection — keep in lockstep with
// client.go ConnectWithOptions.
func effectiveClientFlags(opts []ClientOption) (tracing, prop bool) {
	cfg := newClientConfig(opts)
	enabled := mongoTracingEnabled()
	if cfg.TracingEnabled != nil {
		enabled = *cfg.TracingEnabled
	}
	if !enabled {
		return false, false
	}
	return true, resolveDocumentPropagation(enabled, cfg.PropagationEnabled)
}

// unsetEnvRestore clears name for the rest of the test and restores its prior
// value (or absence) on cleanup. Plain os.Unsetenv has no t.Cleanup
// counterpart, and t.Setenv(name, "") is not equivalent to actually unsetting
// it here — see TestMongoTracingEnabled_EmptyStringIsEnabled.
func unsetEnvRestore(t *testing.T, name string) {
	t.Helper()
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

type envState string

const (
	envUnset envState = "unset"
	envOn    envState = "on"
	envOff   envState = "off"
)

type optState string

const (
	optAbsent optState = "absent"
	optOn     optState = "on"
	optOff    optState = "off"
)

// TestWithTracingEnabled_EnvOptionMatrix pins the full env × WithTracingEnabled
// decision table (option authoritative in either direction).
func TestWithTracingEnabled_EnvOptionMatrix(t *testing.T) {
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
				clearMongoTracingEnv(t)
			case envOn:
				enableTracing(t)
				unsetEnvRestore(t, envMongoPropagationEnabled)
			case envOff:
				t.Setenv(envGlobalTracingEnabled, "false")
				t.Setenv(envMongoTracingEnabled, "false")
				unsetEnvRestore(t, envMongoPropagationEnabled)
				resetPropEnabledCacheForTest()
				t.Cleanup(resetPropEnabledCacheForTest)
			}

			var opts []ClientOption
			switch tc.opt {
			case optOn:
				opts = []ClientOption{WithTracingEnabled(true)}
			case optOff:
				opts = []ClientOption{WithTracingEnabled(false)}
			}

			got, _ := effectiveClientFlags(opts)
			if got != tc.want {
				t.Fatalf("effective tracing = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWithTracePropagationEnabled_EnvOptionMatrix pins propagation resolution
// once effective tracing is known: prop option overrides OTEL_MONGO_PROPAGATION_ENABLED
// only while effective tracing is on; tracing off force-disables propagation.
func TestWithTracePropagationEnabled_EnvOptionMatrix(t *testing.T) {
	cases := []struct {
		name        string
		tracingEnv  envState
		propEnv     envState
		tracingOpt  optState
		propOpt     optState
		wantTracing bool
		wantProp    bool
	}{
		// effective tracing off → prop always off
		{"tracing env off, prop env on, prop opt on → tracing off, prop off", envOff, envOn, optAbsent, optOn, false, false},
		{"tracing opt off (env on), prop opt on → tracing off, prop off", envOn, envOn, optOff, optOn, false, false},
		{"tracing unset, no opts → both off", envUnset, envUnset, optAbsent, optAbsent, false, false},

		// effective tracing on via env; prop follows env/option
		{"tracing env on, prop unset, no prop opt → prop off", envOn, envUnset, optAbsent, optAbsent, true, false},
		{"tracing env on, prop on, no prop opt → prop on", envOn, envOn, optAbsent, optAbsent, true, true},
		{"tracing env on, prop off, no prop opt → prop off", envOn, envOff, optAbsent, optAbsent, true, false},
		{"tracing env on, prop on, prop opt off → prop off", envOn, envOn, optAbsent, optOff, true, false},
		{"tracing env on, prop unset, prop opt on → prop on", envOn, envUnset, optAbsent, optOn, true, true},
		{"tracing env on, prop off, prop opt on → prop on", envOn, envOff, optAbsent, optOn, true, true},
		{"tracing env on, prop on, prop opt on → prop on", envOn, envOn, optAbsent, optOn, true, true},

		// effective tracing on via option (env off/unset); prop option still works
		{"tracing opt on (env unset), prop opt on → both on", envUnset, envUnset, optOn, optOn, true, true},
		{"tracing opt on (env off), prop opt on → both on", envOff, envOff, optOn, optOn, true, true},
		{"tracing opt on (env unset), no prop opt → prop off", envUnset, envUnset, optOn, optAbsent, true, false},
		{"tracing opt on (env unset), prop opt off → prop off", envUnset, envOn, optOn, optOff, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setMongoEnv := func(tracing, prop envState) {
				switch tracing {
				case envUnset:
					unsetEnvRestore(t, envGlobalTracingEnabled)
					unsetEnvRestore(t, envMongoTracingEnabled)
				case envOn:
					t.Setenv(envGlobalTracingEnabled, "1")
					t.Setenv(envMongoTracingEnabled, "1")
				case envOff:
					t.Setenv(envGlobalTracingEnabled, "false")
					t.Setenv(envMongoTracingEnabled, "false")
				}
				switch prop {
				case envUnset:
					unsetEnvRestore(t, envMongoPropagationEnabled)
				case envOn:
					t.Setenv(envMongoPropagationEnabled, "1")
				case envOff:
					t.Setenv(envMongoPropagationEnabled, "false")
				}
				resetPropEnabledCacheForTest()
				t.Cleanup(resetPropEnabledCacheForTest)
			}
			setMongoEnv(tc.tracingEnv, tc.propEnv)

			var opts []ClientOption
			switch tc.tracingOpt {
			case optOn:
				opts = append(opts, WithTracingEnabled(true))
			case optOff:
				opts = append(opts, WithTracingEnabled(false))
			}
			switch tc.propOpt {
			case optOn:
				opts = append(opts, WithTracePropagationEnabled(true))
			case optOff:
				opts = append(opts, WithTracePropagationEnabled(false))
			}

			gotTracing, gotProp := effectiveClientFlags(opts)
			if gotTracing != tc.wantTracing || gotProp != tc.wantProp {
				t.Fatalf("got tracing=%v prop=%v, want tracing=%v prop=%v",
					gotTracing, gotProp, tc.wantTracing, tc.wantProp)
			}
		})
	}
}
