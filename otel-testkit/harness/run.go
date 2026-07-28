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

// WaitForStable waits until anchorService has emitted an application span for the
// run AND the run's app-span count has stopped growing for the quiet window, then
// returns the run's application spans (RunAttr-filtered). It is the count-agnostic
// companion to WaitForAppSpans: use it when the sampler is non-deterministic
// (TraceIDRatioBased, …) so the per-run span count cannot be predicted up front.
// Unlike WaitForAppSpans it does NOT fail on timeout — "downstream dropped by the
// sampler" is a legitimate outcome — it just returns whatever arrived.
//
// anchorService must be a service that is guaranteed to emit (run it at sampling
// rate 1.0) and is the last to export — e.g. the synchronous head, which only
// ends after its downstream call returns. Gating on it is what makes this robust:
// a bare "count stopped growing" check can return before a slow, last-to-export
// span (the head) arrives, dropping it (see the quiet window vs. collector latency
// race). Once the anchor is present, the earlier-exported downstream spans are in
// too, and the quiet window absorbs any collector reordering.
//
// For a deterministic topology where the exact app-span count is known (every
// service at rate 1.0), prefer WaitForAppSpans, which waits for all of them.
func WaitForStable(t *testing.T, sink *Sink, runID, anchorService string, quiet, timeout time.Duration) []Span {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := -1
	stableSince := time.Now()
	for {
		spans := SpansByAttr(sink.Spans(), RunAttr, runID)
		anchored := len(SpansByService(spans, anchorService)) > 0
		n := len(spans)
		if n != last {
			last = n
			stableSince = time.Now()
		} else if anchored && time.Since(stableSince) >= quiet {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return sink.ByRun(runID)
}
