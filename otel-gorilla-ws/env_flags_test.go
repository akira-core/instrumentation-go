package otelgorillaws

import (
	"os"
	"testing"
)

func resetWSGateForTest() {
	wsGate.ResetForTest()
}

// TestWSTracingEnabled_TruthTable exhaustively covers the two-tier tracing
// gate decision matrix (global × module). Universal default-OFF posture:
// any unset env reads false. The gate requires BOTH tiers truthy to enable.
func TestWSTracingEnabled_TruthTable(t *testing.T) {
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
				t.Setenv(envWSTracingEnabled, tc.moduleValue)
			} else {
				_ = os.Unsetenv(envWSTracingEnabled)
			}
			resetWSGateForTest()
			t.Cleanup(resetWSGateForTest)
			if got := wsTracingEnabled(); got != tc.expected {
				t.Fatalf("wsTracingEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}
