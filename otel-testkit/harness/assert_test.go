package harness

import (
	"sort"
	"testing"
)

// TestSpansOfRun checks the run-expansion logic: anchor on RunAttr spans, pull in
// same-trace spans (library-emitted children), follow span links to other traces,
// and exclude unrelated traces and other runs.
func TestSpansOfRun(t *testing.T) {
	spans := []Span{
		{Name: "svc0", TraceID: "aaaa", SpanID: "a1", Attributes: map[string]string{RunAttr: "run1"}},
		{Name: "insert", TraceID: "aaaa", SpanID: "a2", ParentSpanID: "a1", Links: []Link{{TraceID: "bbbb"}}},
		{Name: "linked", TraceID: "bbbb", SpanID: "b1"}, // reached via the link from trace aaaa
		{Name: "unrelated", TraceID: "cccc", SpanID: "c1"},
		{Name: "otherrun", TraceID: "dddd", SpanID: "d1", Attributes: map[string]string{RunAttr: "run2"}},
	}
	got := names(SpansOfRun(spans, "run1"))
	want := []string{"insert", "linked", "svc0"}
	if !equalStrings(got, want) {
		t.Errorf("SpansOfRun(run1) = %v, want %v", got, want)
	}
}

func TestSpansOfRunNoAnchor(t *testing.T) {
	spans := []Span{{Name: "x", TraceID: "aaaa", SpanID: "a1"}}
	if got := SpansOfRun(spans, "missing"); len(got) != 0 {
		t.Errorf("SpansOfRun with no anchor = %v, want empty", names(got))
	}
}

// TestDistinctRVs checks rv parsing/dedup/sort and that rv-less spans are ignored.
func TestDistinctRVs(t *testing.T) {
	spans := []Span{
		{TraceState: "ot=rv:0000000000002a"}, // 42
		{TraceState: "ot=rv:0000000000002a"}, // dup
		{TraceState: ""},                     // none
		{TraceState: "ot=rv:00000000000010"}, // 16
	}
	got := DistinctRVs(spans)
	if len(got) != 2 || got[0] != 0x10 || got[1] != 0x2a {
		t.Errorf("DistinctRVs = %x, want [10 2a]", got)
	}
}

// TestAssertConsistentRV checks the happy path returns the single shared rv.
func TestAssertConsistentRV(t *testing.T) {
	spans := []Span{
		{TraceState: "ot=rv:0000000000002a"},
		{TraceState: "ot=rv:0000000000002a"},
	}
	if got := AssertConsistentRV(t, spans); got != 0x2a {
		t.Errorf("AssertConsistentRV = %x, want 2a", got)
	}
}

// TestAssertLinkedTrace checks the happy path: toService lives in a different
// trace and carries a link to fromService's trace.
func TestAssertLinkedTrace(t *testing.T) {
	spans := []Span{
		{ServiceName: "svc0", TraceID: "aaaa", SpanID: "a1"},
		{ServiceName: "svc1", TraceID: "bbbb", SpanID: "b1", Links: []Link{{TraceID: "aaaa"}}},
	}
	AssertLinkedTrace(t, spans, "svc0", "svc1")
}

// TestTraceIDOf checks lookup of a service's trace ID, present and absent.
func TestTraceIDOf(t *testing.T) {
	spans := []Span{
		{ServiceName: "svc0", TraceID: "aaaa"},
		{ServiceName: "svc1", TraceID: ""}, // no trace ID — skipped
		{ServiceName: "svc1", TraceID: "bbbb"},
	}
	if id, ok := TraceIDOf(spans, "svc0"); !ok || id != "aaaa" {
		t.Errorf("TraceIDOf(svc0) = %q,%v want aaaa,true", id, ok)
	}
	if id, ok := TraceIDOf(spans, "svc1"); !ok || id != "bbbb" {
		t.Errorf("TraceIDOf(svc1) = %q,%v want bbbb,true (first non-empty)", id, ok)
	}
	if _, ok := TraceIDOf(spans, "missing"); ok {
		t.Errorf("TraceIDOf(missing) ok = true, want false")
	}
}

// TestAssertTraceContinued checks the happy path: upstream and downstream share
// a trace ID (propagation continued the trace).
func TestAssertTraceContinued(t *testing.T) {
	spans := []Span{
		{ServiceName: "svc0", TraceID: "aaaa", SpanID: "a1"},
		{ServiceName: "svc1", TraceID: "aaaa", SpanID: "a2", ParentSpanID: "a1"},
	}
	AssertTraceContinued(t, spans, "svc0", "svc1")
}

// TestAssertTraceNotContinued checks the happy path: downstream is in a different
// trace with no link back to upstream (propagation severed).
func TestAssertTraceNotContinued(t *testing.T) {
	spans := []Span{
		{ServiceName: "svc0", TraceID: "aaaa", SpanID: "a1"},
		{ServiceName: "svc1", TraceID: "bbbb", SpanID: "b1"}, // fresh root, no link
	}
	AssertTraceNotContinued(t, spans, "svc0", "svc1")
}

func names(spans []Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSpansOfRunFollowsInboundLinks pins the real async direction: only the
// producer carries RunAttr, and the consumer's span in a new trace links back
// to it. Following outbound links alone would miss the consumer entirely,
// silently shrinking the span set every "whole picture" assertion runs on.
func TestSpansOfRunFollowsInboundLinks(t *testing.T) {
	spans := []Span{
		{Name: "producer", TraceID: "aaaa", SpanID: "a1", Attributes: map[string]string{RunAttr: "run1"}},
		{Name: "consumer", TraceID: "bbbb", SpanID: "b1", Links: []Link{{TraceID: "aaaa"}}},
		{Name: "consumer-child", TraceID: "bbbb", SpanID: "b2", ParentSpanID: "b1"},
		{Name: "unrelated", TraceID: "cccc", SpanID: "c1"},
	}
	got := names(SpansOfRun(spans, "run1"))
	want := []string{"consumer", "consumer-child", "producer"}
	if !equalStrings(got, want) {
		t.Errorf("SpansOfRun(run1) = %v, want %v", got, want)
	}
}

// TestSpansOfRunFollowsInboundLinksTransitively pins a multi-hop link chain
// where every hop points backwards (consumer → producer), as NATS/Mongo
// span-link consumers do.
func TestSpansOfRunFollowsInboundLinksTransitively(t *testing.T) {
	spans := []Span{
		{Name: "hop0", TraceID: "aaaa", SpanID: "a1", Attributes: map[string]string{RunAttr: "run1"}},
		{Name: "hop1", TraceID: "bbbb", SpanID: "b1", Links: []Link{{TraceID: "aaaa"}}},
		{Name: "hop2", TraceID: "cccc", SpanID: "c1", Links: []Link{{TraceID: "bbbb"}}},
		{Name: "unrelated", TraceID: "dddd", SpanID: "d1"},
	}
	got := names(SpansOfRun(spans, "run1"))
	want := []string{"hop0", "hop1", "hop2"}
	if !equalStrings(got, want) {
		t.Errorf("SpansOfRun(run1) = %v, want %v", got, want)
	}
}

// TestAssertAllSpansCarryRV checks the happy path: every span carries the rv.
func TestAssertAllSpansCarryRV(t *testing.T) {
	spans := []Span{
		{Name: "a", TraceState: "ot=rv:0000000000002a"},
		{Name: "b", TraceState: "ot=th:8;rv:0000000000002a"},
	}
	AssertAllSpansCarryRV(t, spans, 0x2a)
}

// TestCountRVCoverage checks the with/without split reported in the
// AssertConsistentRV failure message — the signal that tells "rv lost on one
// hop" apart from "two different rv values".
func TestCountRVCoverage(t *testing.T) {
	spans := []Span{
		{TraceState: "ot=rv:0000000000002a"},
		{TraceState: ""},
		{TraceState: "ot=th:8"},
	}
	with, without := countRVCoverage(spans)
	if with != 1 || without != 2 {
		t.Errorf("countRVCoverage = %d with, %d without; want 1, 2", with, without)
	}
}

// TestUniformRVsCoversRateDeterministically pins that one run per UniformRVs
// value makes the sampled fraction match the rate within 1/n — the property
// that lets the E2E rate check drop its statistical tolerance.
func TestUniformRVsCoversRateDeterministically(t *testing.T) {
	const n = 40
	rvs := UniformRVs(n)
	if len(rvs) != n {
		t.Fatalf("UniformRVs(%d) returned %d values", n, len(rvs))
	}
	for _, rate := range []float64{0.1, 0.25, 0.5, 0.9, 1.0} {
		sampled := 0
		for _, rv := range rvs {
			if ExpectedSampled(rate, rv) {
				sampled++
			}
		}
		frac := float64(sampled) / float64(n)
		if diff := frac - rate; diff > 1.0/n || diff < -1.0/n {
			t.Errorf("rate %.2f: sampled fraction %.3f differs by more than 1/%d", rate, frac, n)
		}
	}
}

// TestUniformRVsEmpty pins the n <= 0 guard.
func TestUniformRVsEmpty(t *testing.T) {
	if got := UniformRVs(0); got != nil {
		t.Errorf("UniformRVs(0) = %v, want nil", got)
	}
	if got := UniformRVs(-1); got != nil {
		t.Errorf("UniformRVs(-1) = %v, want nil", got)
	}
}
