package harness

import (
	"os"
	"testing"
)

// TestEnvTruthyMatchesModuleFlags is the golden table pinning envTruthy against
// the modules' internal flags.EnvEnabled. The modules vendor internal copies
// that cannot be imported, so the semantics can only be held together by this
// table. The set-but-empty row is the one that used to diverge: an empty value
// is TRUTHY (docker `-e VAR`, or a mistyped GitHub Actions matrix key, produces
// exactly that), and reading it as falsy makes the whole matrix assert the
// disabled branch against an enabled wrapper.
func TestEnvTruthyMatchesModuleFlags(t *testing.T) {
	const key = "OTEL_TESTKIT_ENVTRUTHY_PROBE"
	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset", set: false, want: false},
		{name: "set-but-empty", value: "", set: true, want: true},
		{name: "whitespace", value: "  ", set: true, want: true},
		{name: "zero", value: "0", set: true, want: false},
		{name: "false", value: "false", set: true, want: false},
		{name: "FALSE", value: "FALSE", set: true, want: false},
		{name: "no", value: "no", set: true, want: false},
		{name: "off", value: "off", set: true, want: false},
		{name: "padded-zero", value: " 0 ", set: true, want: false},
		{name: "one", value: "1", set: true, want: true},
		{name: "true", value: "true", set: true, want: true},
		{name: "arbitrary", value: "x", set: true, want: true},
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
			if got := envTruthy(key); got != c.want {
				t.Errorf("envTruthy(%s=%q set=%v) = %v, want %v", key, c.value, c.set, got, c.want)
			}
		})
	}
}

// TestEnvTruthyEmptyKey pins that an empty GateEnv field (a transport with no
// independent propagation gate) never reads the environment.
func TestEnvTruthyEmptyKey(t *testing.T) {
	if envTruthy("") {
		t.Error("envTruthy(\"\") = true, want false")
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
