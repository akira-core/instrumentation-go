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
