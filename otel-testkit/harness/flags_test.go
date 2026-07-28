package harness

import "testing"

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
