package harness

import (
	"testing"
	"time"
)

// addSpan appends a span to the sink as if it had been exported, so timing tests
// need not stand up the full OTLP path.
func (s *Sink) addSpan(sp Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, sp)
}

// TestWaitForStableReturnsRunSpans checks WaitForStable returns the run's app
// spans once the anchor service is present and the count goes quiet.
func TestWaitForStableReturnsRunSpans(t *testing.T) {
	sink := StartSink(t)
	sink.addSpan(Span{ServiceName: "svc0", TraceID: "aaaa", SpanID: "a1",
		Attributes: map[string]string{RunAttr: "run1"}})
	sink.addSpan(Span{ServiceName: "other", TraceID: "zzzz", SpanID: "z1",
		Attributes: map[string]string{RunAttr: "run2"}}) // different run, excluded

	got := WaitForStable(t, sink, "run1", "svc0", 100*time.Millisecond, 5*time.Second)
	if len(got) != 1 || got[0].ServiceName != "svc0" {
		t.Fatalf("WaitForStable(run1) = %v, want one svc0 span", names(got))
	}
}

// TestWaitForStableTimeoutEmpty checks that a run whose anchor never arrives
// returns empty after the timeout instead of failing the test.
func TestWaitForStableTimeoutEmpty(t *testing.T) {
	sink := StartSink(t)
	got := WaitForStable(t, sink, "absent", "svc0", 50*time.Millisecond, 150*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("WaitForStable(absent) = %v, want empty", names(got))
	}
}

// TestWaitForStableWaitsForAnchor checks WaitForStable does not return on a
// non-anchor span alone: a downstream span present without the anchor must not
// satisfy it (the bug that dropped the last-to-export head span).
func TestWaitForStableWaitsForAnchor(t *testing.T) {
	sink := StartSink(t)
	sink.addSpan(Span{ServiceName: "svc1", TraceID: "bbbb", SpanID: "b1",
		Attributes: map[string]string{RunAttr: "run1"}}) // downstream only, no anchor

	// Anchor "svc0" never arrives → must time out and return what is there.
	got := WaitForStable(t, sink, "run1", "svc0", 50*time.Millisecond, 200*time.Millisecond)
	if len(got) != 1 || got[0].ServiceName != "svc1" {
		t.Fatalf("WaitForStable(run1) = %v, want the lone svc1 span after timeout", names(got))
	}
}
