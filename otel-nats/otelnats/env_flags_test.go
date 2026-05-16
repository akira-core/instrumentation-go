package otelnats

import (
	"os"
	"testing"
)

func resetNATSGateForTest() {
	natsGate.ResetForTest()
	natsPropagationGate.ResetForTest()
}

// TestNATSTracingEnabled_TruthTable exhaustively covers the two-tier tracing
// gate decision matrix (global × module). Universal default-OFF posture:
// any unset env reads false. The gate requires BOTH tiers truthy to enable.
func TestNATSTracingEnabled_TruthTable(t *testing.T) {
	cases := []struct {
		name        string
		globalValue string // "" means unset
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
		// Both truthy → on. Covers truthy synonyms.
		{"both_true", "true", true, "true", true, true},
		{"both_one", "1", true, "1", true, true},
		{"both_on", "on", true, "on", true, true},
		{"both_yes", "yes", true, "yes", true, true},
		{"both_empty_string", "", true, "", true, true},
		{"arbitrary_string_is_truthy", "hello", true, "world", true, true},
		// Either gate explicitly falsy → off.
		{"global_false", "false", true, "true", true, false},
		{"global_zero", "0", true, "true", true, false},
		{"global_off", "OFF", true, "true", true, false},
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
				t.Setenv(envNATSTracingEnabled, tc.moduleValue)
			} else {
				_ = os.Unsetenv(envNATSTracingEnabled)
			}
			resetNATSGateForTest()
			t.Cleanup(resetNATSGateForTest)
			if got := natsTracingEnabled(); got != tc.expected {
				t.Fatalf("natsTracingEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// TestNATSPropagationEnabled_TruthTable exhaustively covers the three-tier
// propagation gate decision matrix (global × module-tracing × module-
// propagation). Universal default-OFF posture. Tracing is the hard
// prerequisite — when off, propagation is forced false regardless of the
// propagation env var.
func TestNATSPropagationEnabled_TruthTable(t *testing.T) {
	cases := []struct {
		name     string
		global   string // "" = unset, otherwise t.Setenv value
		tracing  string
		propEnv  string
		propSet  bool
		expected bool
	}{
		// Tracing gate off — propagation always disabled.
		{"all_unset", "", "", "", false, false},
		{"only_propagation_truthy", "", "", "true", true, false},
		{"global_off_module_on_prop_on", "false", "true", "true", true, false},
		{"global_on_module_off_prop_on", "true", "false", "true", true, false},
		// Tracing gate on — propagation env decides.
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
				_ = os.Unsetenv(envNATSTracingEnabled)
			} else {
				t.Setenv(envNATSTracingEnabled, tc.tracing)
			}
			if tc.propSet {
				t.Setenv(envNATSPropagationEnabled, tc.propEnv)
			} else {
				_ = os.Unsetenv(envNATSPropagationEnabled)
			}
			resetNATSGateForTest()
			t.Cleanup(resetNATSGateForTest)
			if got := natsPropagationEnabled(); got != tc.expected {
				t.Fatalf("natsPropagationEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}
