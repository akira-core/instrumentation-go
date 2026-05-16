package otelmongo

import (
	"os"
	"testing"
)

// TestMongoTracingEnabled_TruthTable exhaustively covers the two-tier
// tracing gate decision matrix (global × module). Universal default-OFF
// posture: any unset env reads false. The gate requires BOTH tiers truthy
// to enable.
func TestMongoTracingEnabled_TruthTable(t *testing.T) {
	cases := []struct {
		name        string
		globalValue string
		globalSet   bool
		moduleValue string
		moduleSet   bool
		expected    bool
	}{
		// Default: both unset → off.
		{"both_unset", "", false, "", false, false},
		// Either gate alone is insufficient.
		{"only_global_truthy", "true", true, "", false, false},
		{"only_module_truthy", "", false, "true", true, false},
		// Both truthy → on. Truthy synonyms covered.
		{"both_true", "true", true, "true", true, true},
		{"both_one", "1", true, "1", true, true},
		{"both_on", "on", true, "on", true, true},
		{"both_yes", "yes", true, "yes", true, true},
		{"both_empty_string", "", true, "", true, true},
		{"arbitrary_string_is_truthy", "hello", true, "world", true, true},
		// Either gate explicitly falsy → off. Falsy synonyms.
		{"global_false", "false", true, "true", true, false},
		{"global_zero", "0", true, "true", true, false},
		{"global_off_caps", "OFF", true, "true", true, false},
		{"global_no", "no", true, "true", true, false},
		{"global_padded_zero", " 0 ", true, "true", true, false},
		{"module_false", "true", true, "false", true, false},
		{"module_zero", "true", true, "0", true, false},
		{"module_off_caps", "true", true, "OFF", true, false},
		{"module_no", "true", true, "no", true, false},
		{"module_padded_zero", "true", true, " 0 ", true, false},
		// Both explicitly falsy.
		{"both_false", "false", true, "false", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.globalSet {
				t.Setenv(envGlobalTracingEnabled, tc.globalValue)
			} else {
				_ = os.Unsetenv(envGlobalTracingEnabled)
			}
			if tc.moduleSet {
				t.Setenv(envMongoTracingEnabled, tc.moduleValue)
			} else {
				_ = os.Unsetenv(envMongoTracingEnabled)
			}
			if got := mongoTracingEnabled(); got != tc.expected {
				t.Fatalf("mongoTracingEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// TestMongoPropagationEnabled_TruthTable exhaustively covers the three-tier
// propagation gate decision matrix (global × module-tracing × module-
// propagation). Universal default-OFF posture. Both tracing tiers are hard
// prerequisites — propagation env on its own cannot enable propagation.
func TestMongoPropagationEnabled_TruthTable(t *testing.T) {
	cases := []struct {
		name     string
		global   string // "" = unset, otherwise t.Setenv value
		tracing  string
		propEnv  string
		propSet  bool
		expected bool
	}{
		// Tracing gates off — propagation always disabled.
		{"all_unset", "", "", "", false, false},
		{"only_propagation_truthy", "", "", "true", true, false},
		{"global_off_module_on_prop_on", "false", "true", "true", true, false},
		{"global_on_module_off_prop_on", "true", "false", "true", true, false},
		// Both tracing on — propagation env decides.
		{"tracing_on_prop_unset", "true", "true", "", false, false},
		{"tracing_on_prop_true", "true", "true", "true", true, true},
		{"tracing_on_prop_one", "true", "true", "1", true, true},
		{"tracing_on_prop_on", "true", "true", "on", true, true},
		{"tracing_on_prop_enabled_word", "true", "true", "enabled", true, true},
		{"tracing_on_prop_false", "true", "true", "false", true, false},
		{"tracing_on_prop_zero", "true", "true", "0", true, false},
		{"tracing_on_prop_off_caps", "true", "true", "OFF", true, false},
		{"tracing_on_prop_no", "true", "true", "no", true, false},
		{"tracing_on_prop_padded_zero", "true", "true", " 0 ", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.global == "" {
				_ = os.Unsetenv(envGlobalTracingEnabled)
			} else {
				t.Setenv(envGlobalTracingEnabled, tc.global)
			}
			if tc.tracing == "" {
				_ = os.Unsetenv(envMongoTracingEnabled)
			} else {
				t.Setenv(envMongoTracingEnabled, tc.tracing)
			}
			if tc.propSet {
				t.Setenv(envMongoPropagationEnabled, tc.propEnv)
			} else {
				_ = os.Unsetenv(envMongoPropagationEnabled)
			}
			if got := mongoPropagationEnabled(); got != tc.expected {
				t.Fatalf("mongoPropagationEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// TestResolveFlag covers the helper that lets a functional option override an
// env-derived default. The override pointer wins whenever non-nil.
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
	t.Run("nil override returns env default true", func(t *testing.T) {
		if !resolveFlag(nil, true) {
			t.Fatal("nil override must defer to env default")
		}
	})
	t.Run("nil override returns env default false", func(t *testing.T) {
		if resolveFlag(nil, false) {
			t.Fatal("nil override must defer to env default")
		}
	})
}

// TestConnectPropagationResolution covers the interaction between the
// functional option (WithTracePropagationEnabled) and the three-tier env
// gate. Tracing gates are hard prerequisites; even an explicit option=true
// cannot bypass them. When tracing is on the option overrides the env.
func TestConnectPropagationResolution(t *testing.T) {
	t.Run("global off blocks option=true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "false")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("global tracing off must disable propagation regardless of option")
		}
	})
	t.Run("mongo tracing off blocks option=true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "false")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("mongo tracing off must disable propagation regardless of option")
		}
	})
	t.Run("tracing on: option=false overrides env=true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(false)})
		if resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("option=false must override env=true")
		}
	})
	t.Run("tracing on: option=true enables when env unset", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		_ = os.Unsetenv(envMongoPropagationEnabled)
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if !resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("option=true must enable when tracing on and env unset")
		}
	})
	t.Run("tracing on: nil option defers to env=true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig(nil)
		if !resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("nil option must defer to env=true")
		}
	})
	t.Run("tracing on: nil option defers to env=false", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "false")
		cfg := newClientConfig(nil)
		if resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("nil option must defer to env=false")
		}
	})
}
