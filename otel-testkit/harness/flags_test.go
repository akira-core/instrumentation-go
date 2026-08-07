package harness

import (
	"os"
	"testing"
)

// TestEnvValueMatchesModuleFlags is the golden table pinning the harness's
// per-variable read against otelflags.Lookup, which the modules use directly.
//
// Three rows changed with the ladder, and each one used to make the matrix
// assert the wrong branch:
//
//   - set-but-empty and whitespace are no longer truthy. They are unreadable,
//     and a module carrying one fails at construction — so the harness treats
//     them as disabled rather than as an enthusiastic yes.
//   - an arbitrary value like "x" is unreadable too, not truthy.
//   - unset now falls through to the caller's default, which is ENABLED for the
//     master switch and DISABLED for a module switch.
func TestEnvValueMatchesModuleFlags(t *testing.T) {
	const key = "OTEL_TESTKIT_ENVVALUE_PROBE"
	cases := []struct {
		name  string
		value string
		set   bool
		def   bool
		want  bool
	}{
		{name: "unset takes the default false", set: false, def: false, want: false},
		{name: "unset takes the default true", set: false, def: true, want: true},

		{name: "set-but-empty is unreadable", value: "", set: true, def: true, want: false},
		{name: "whitespace is unreadable", value: "  ", set: true, def: true, want: false},
		{name: "arbitrary is unreadable", value: "x", set: true, def: true, want: false},

		{name: "zero", value: "0", set: true, def: true, want: false},
		{name: "false", value: "false", set: true, def: true, want: false},
		{name: "FALSE", value: "FALSE", set: true, def: true, want: false},
		{name: "no", value: "no", set: true, def: true, want: false},
		{name: "off", value: "off", set: true, def: true, want: false},
		{name: "padded-zero", value: " 0 ", set: true, def: true, want: false},

		{name: "one", value: "1", set: true, def: false, want: true},
		{name: "true", value: "true", set: true, def: false, want: true},
		{name: "on", value: "on", set: true, def: false, want: true},
		{name: "yes", value: "yes", set: true, def: false, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(key, "")
			if c.set {
				t.Setenv(key, c.value)
			} else {
				if err := os.Unsetenv(key); err != nil {
					t.Fatalf("unset %s: %v", key, err)
				}
			}
			if got := envValue(key, "", c.def); got != c.want {
				t.Errorf("envValue(%s=%q set=%v def=%v) = %v, want %v", key, c.value, c.set, c.def, got, c.want)
			}
		})
	}
}

// TestEnvValueEmptyKey pins that an empty GateEnv field (a transport with no
// independent propagation gate) never reads the environment and takes the
// default.
func TestEnvValueEmptyKey(t *testing.T) {
	if envValue("", "", false) {
		t.Error("envValue(\"\", \"\", false) = true, want false")
	}
	if !envValue("", "", true) {
		t.Error("envValue(\"\", \"\", true) = false, want true")
	}
}

// TestExpectedSampled checks the threshold boundary: a span is sampled iff
// rv >= threshold(p) ≈ (1-p)·2^56. The rv ladder used by the integration tests
// relies on these edges holding exactly.
func TestExpectedSampled(t *testing.T) {
	const maxRV = (uint64(1) << 56) - 1
	cases := []struct {
		p    float64
		rv   uint64
		want bool
	}{
		{1.0, 0, true},               // p=1 → threshold 0 → always sampled
		{1.0, maxRV, true},           //
		{0.0, maxRV, false},          // p=0 → threshold 2^56 → never sampled
		{0.0, 0, false},              //
		{0.5, 1 << 55, true},         // threshold = 2^55, rv == threshold → sampled
		{0.5, (1 << 55) - 1, false},  // just below threshold → dropped
		{0.25, 3 << 54, true},        // threshold = 0.75·2^56 = 3·2^54, rv == threshold
		{0.25, (3 << 54) - 1, false}, // just below
	}
	for _, c := range cases {
		if got := ExpectedSampled(c.p, c.rv); got != c.want {
			t.Errorf("ExpectedSampled(%.2f, %#x) = %v, want %v", c.p, c.rv, got, c.want)
		}
	}
}

// TestCountSampled checks the per-rate sampled count used to size WaitForAppSpans
// and the presence assertions.
func TestCountSampled(t *testing.T) {
	rates := []float64{0.9, 0.5, 0.1}
	cases := []struct {
		rv   uint64
		want int
	}{
		{0, 0},             // no rate samples rv=0 (none is 1.0)
		{1 << 55, 2},       // 0.9 and 0.5 sample 0.5·2^56; 0.1 does not
		{(1 << 56) - 1, 3}, // max rv → every positive rate samples
	}
	for _, c := range cases {
		if got := CountSampled(rates, c.rv); got != c.want {
			t.Errorf("CountSampled(%v, %#x) = %d, want %d", rates, c.rv, got, c.want)
		}
	}
}
