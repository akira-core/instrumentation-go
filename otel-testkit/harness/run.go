package harness

import (
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RunAttrOption returns the span-start option that tags a span with RunAttr ==
// runID. Use it on every application span you start so SpansByAttr / (*Sink).ByRun
// can select one logical run's spans.
//
//	_, span := tracer.Start(ctx, name, harness.RunAttrOption(runID))
func RunAttrOption(runID string) trace.SpanStartOption {
	return trace.WithAttributes(attribute.String(RunAttr, runID))
}

// CountSampled returns how many of the given per-service rates sample randomness
// rv (i.e. predicts how many services should appear for a chosen rv). It is the
// expected app-span count to wait for and assert against.
func CountSampled(rates []float64, rv uint64) int {
	n := 0
	for _, r := range rates {
		if ExpectedSampled(r, rv) {
			n++
		}
	}
	return n
}

// WaitForAppSpans blocks until at least want application spans tagged with
// RunAttr == runID arrive at the sink, then returns them. On timeout it dumps
// every span the sink received and fails the test with a got/want message —
// replacing a silent partial return that makes downstream assertions confusing.
//
// want may be 0 (when the seeded rv samples no service): the predicate is
// satisfied immediately and an empty slice is returned without waiting.
func WaitForAppSpans(t *testing.T, sink *Sink, runID string, want int, timeout time.Duration) []Span {
	t.Helper()
	spans := sink.WaitFor(timeout, func(ss []Span) bool {
		return len(SpansByAttr(ss, RunAttr, runID)) >= want
	})
	got := SpansByAttr(spans, RunAttr, runID)
	if len(got) < want {
		DumpSpans(t, fmt.Sprintf("WaitForAppSpans timeout: run %s got %d/%d app spans", runID, len(got), want), sink.Spans())
		t.Fatalf("WaitForAppSpans: run %s timed out after %s with %d/%d app spans", runID, timeout, len(got), want)
	}
	return got
}
