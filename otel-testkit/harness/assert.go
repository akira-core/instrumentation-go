package harness

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SpansByAttr returns the spans whose attribute attr equals val. Use it with
// RunAttr to select the spans of one logical run across services.
func SpansByAttr(spans []Span, attr, val string) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.Attributes[attr] == val {
			out = append(out, s)
		}
	}
	return out
}

// SpansByService returns the spans whose ServiceName equals name.
func SpansByService(spans []Span, name string) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.ServiceName == name {
			out = append(out, s)
		}
	}
	return out
}

// DistinctRVs returns the sorted distinct consistent-sampling randomness values
// found across spans (spans without an rv are ignored).
func DistinctRVs(spans []Span) []uint64 {
	seen := map[uint64]bool{}
	for _, s := range spans {
		if rv, ok := s.RV(); ok {
			seen[rv] = true
		}
	}
	out := make([]uint64, 0, len(seen))
	for rv := range seen {
		out = append(out, rv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AssertConsistentRV asserts every span that carries a tracestate rv carries the
// same one, and returns it. This is the core consistent-sampling invariant: a
// trace's randomness must survive inject→extract unchanged across every service.
// Fails if no span carries an rv. Spans without an rv are ignored.
func AssertConsistentRV(t *testing.T, spans []Span) uint64 {
	t.Helper()
	rvs := DistinctRVs(spans)
	if len(rvs) != 1 {
		// Dump the spans before failing so the cause (missing rv vs. several rv
		// values) is visible without re-running with DumpSpans by hand.
		DumpSpans(t, "AssertConsistentRV failed", spans)
	}
	require.NotEmpty(t, rvs, "no span carried a tracestate rv (ot=rv:)")
	require.Len(t, rvs, 1, "spans carried inconsistent rv values: %x", rvs)
	return rvs[0]
}

// AssertPresence asserts that, among spans, a service is present (has ≥1 span)
// iff its sampling rate samples rv. want maps service.name → sampling rate.
// This encodes "node i appears ⇔ rv ≥ threshold(rate_i)".
func AssertPresence(t *testing.T, spans []Span, want map[string]float64, rv uint64) {
	t.Helper()
	for name, rate := range want {
		present := len(SpansByService(spans, name)) > 0
		assert.Equalf(t, ExpectedSampled(rate, rv), present,
			"service %q presence=%v at rate=%.3f rv=%x", name, present, rate, rv)
	}
}

// AssertNoWrapperSpans asserts that no span carries the given instrumentation
// scope. Use it when the library's tracing feature flag is disabled: the
// wrapper must emit no spans while the application's own spans still flow.
func AssertNoWrapperSpans(t *testing.T, spans []Span, scope string) {
	t.Helper()
	for _, s := range spans {
		require.NotEqualf(t, scope, s.Scope,
			"tracing disabled: unexpected wrapper-scope span %q", s.Name)
	}
}

// SpansByScope returns the spans whose instrumentation Scope equals scope.
// Use it to isolate a wrapper's spans (e.g. otelmongo.ScopeName) from the
// application spans you start yourself.
func SpansByScope(spans []Span, scope string) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.Scope == scope {
			out = append(out, s)
		}
	}
	return out
}

// SpansByServicePrefix returns the spans whose ServiceName starts with prefix.
// Use it to select synthetic deliver spans (service "mongodb://…", "nats://…").
func SpansByServicePrefix(spans []Span, prefix string) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if strings.HasPrefix(s.ServiceName, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// SpansOfRun returns every span belonging to one logical run: the application
// spans tagged with RunAttr == runID, plus all spans sharing their trace IDs
// (wrapper client + deliver spans), plus spans in traces reachable via span
// links (span-link hops start a new trace ID). Use it when an assertion must
// see the whole picture — not just the application spans — e.g. to check that
// producer, consumer, and deliver spans all carry the same randomness.
func SpansOfRun(all []Span, runID string) []Span {
	traceIDs := map[string]bool{}
	for _, s := range SpansByAttr(all, RunAttr, runID) {
		if s.TraceID != "" {
			traceIDs[s.TraceID] = true
		}
	}
	// Transitively pull in linked traces (span-link consumers are new roots).
	for changed := true; changed; {
		changed = false
		for _, s := range all {
			if !traceIDs[s.TraceID] {
				continue
			}
			for _, l := range s.Links {
				if l.TraceID != "" && !traceIDs[l.TraceID] {
					traceIDs[l.TraceID] = true
					changed = true
				}
			}
		}
	}
	out := make([]Span, 0, len(all))
	for _, s := range all {
		if traceIDs[s.TraceID] {
			out = append(out, s)
		}
	}
	return out
}

// SampledFraction returns the fraction of the given per-run span sets in which
// service appears. Drive many random-rv runs at a single rate, then compare
// against the rate.
func SampledFraction(runs [][]Span, service string) float64 {
	if len(runs) == 0 {
		return 0
	}
	n := 0
	for _, run := range runs {
		if len(SpansByService(run, service)) > 0 {
			n++
		}
	}
	return float64(n) / float64(len(runs))
}

// AssertSampledFraction asserts the fraction of runs in which service appears is
// within delta of rate (statistical sampling-rate check).
func AssertSampledFraction(t *testing.T, runs [][]Span, service string, rate, delta float64) {
	t.Helper()
	frac := SampledFraction(runs, service)
	assert.InDeltaf(t, rate, frac, delta,
		"service %q sampled fraction %.3f should be near rate %.3f over %d runs",
		service, frac, rate, len(runs))
}

// AssertAppSpanCounts asserts each service has exactly the expected number of
// application spans: countIfSampled when its rate samples rv, else 0. Pass the
// RunAttr-filtered span set (application spans only); this is the precise-count
// companion to AssertPresence.
func AssertAppSpanCounts(t *testing.T, spans []Span, want map[string]float64, rv uint64, countIfSampled int) {
	t.Helper()
	for name, rate := range want {
		expected := 0
		if ExpectedSampled(rate, rv) {
			expected = countIfSampled
		}
		got := len(SpansByService(spans, name))
		assert.Equalf(t, expected, got,
			"service %q span count=%d (want %d) at rate=%.3f rv=%x", name, got, expected, rate, rv)
	}
}

// AssertSameTrace asserts all given spans share a single TraceID. Use it on a
// parent-child segment (a span-link hop starts a new trace, so do not use it
// across one). Fails on an empty set.
func AssertSameTrace(t *testing.T, spans []Span) {
	t.Helper()
	require.NotEmpty(t, spans, "AssertSameTrace: no spans")
	ids := map[string]bool{}
	for _, s := range spans {
		ids[s.TraceID] = true
	}
	require.Lenf(t, ids, 1, "spans span multiple trace IDs: %v", traceIDKeys(ids))
}

// AssertLinkedTrace asserts the toService span is a span-link consumer of the
// fromService span: it lives in a different trace and carries a link whose
// TraceID matches fromService's trace. (Keyed on TraceID — robust to the
// wrapper's client span being the actual link target within the producer trace.)
func AssertLinkedTrace(t *testing.T, spans []Span, fromService, toService string) {
	t.Helper()
	from := SpansByService(spans, fromService)
	to := SpansByService(spans, toService)
	require.NotEmptyf(t, from, "AssertLinkedTrace: no spans for %q", fromService)
	require.NotEmptyf(t, to, "AssertLinkedTrace: no spans for %q", toService)
	fromTrace := from[0].TraceID

	for _, s := range to {
		require.NotEqualf(t, fromTrace, s.TraceID,
			"%q shares a trace with %q — expected a span link, not parent-child", toService, fromService)
		for _, l := range s.Links {
			if l.TraceID == fromTrace {
				return
			}
		}
	}
	t.Errorf("%q has no span link to %q's trace %s", toService, fromService, fromTrace)
}

// TraceIDOf returns the TraceID of the first application span belonging to
// service. The bool is false when service has no span in the set. Handy when an
// assertion or a failure message needs a service's trace ID directly.
func TraceIDOf(spans []Span, service string) (string, bool) {
	for _, s := range SpansByService(spans, service) {
		if s.TraceID != "" {
			return s.TraceID, true
		}
	}
	return "", false
}

// AssertTraceContinued asserts the upstream and downstream services' spans live
// in the same trace — i.e. trace context propagated across the hop (parent-child
// continuation). Unlike AssertSameTrace, it is scoped to the named service pair,
// so an unrelated span-link hop elsewhere in the run does not make it fail. This
// is the sampler-agnostic propagation check: it reads only TraceID, never the
// "ot=rv:" tracestate, so it works for any sampler (TraceIDRatioBased,
// ParentBased, …), not just the consistent sampler.
func AssertTraceContinued(t *testing.T, spans []Span, upstream, downstream string) {
	t.Helper()
	up, okUp := TraceIDOf(spans, upstream)
	down, okDown := TraceIDOf(spans, downstream)
	require.Truef(t, okUp, "AssertTraceContinued: no spans for upstream %q", upstream)
	require.Truef(t, okDown, "AssertTraceContinued: no spans for downstream %q", downstream)
	require.Equalf(t, up, down,
		"%q (trace %s) did not continue %q's trace %s — expected propagation", downstream, down, upstream, up)
}

// AssertTraceNotContinued asserts trace context did NOT propagate from upstream
// to downstream: the downstream span lives in a different trace and carries no
// link back to the upstream trace (ruling out the span-link topology, which is a
// legitimate cross-trace connection). Use it for the propagation-disabled case.
// Like AssertTraceContinued it is sampler-agnostic (TraceID only), replacing the
// rv-based DistinctRVs check for libraries whose sampler does not write "ot=rv:".
// Both services must have produced a span (run them at sampling rate 1.0 so the
// sampler cannot drop the spans under test).
func AssertTraceNotContinued(t *testing.T, spans []Span, upstream, downstream string) {
	t.Helper()
	up, okUp := TraceIDOf(spans, upstream)
	require.Truef(t, okUp, "AssertTraceNotContinued: no spans for upstream %q", upstream)
	down := SpansByService(spans, downstream)
	require.NotEmptyf(t, down, "AssertTraceNotContinued: no spans for downstream %q", downstream)

	for _, s := range down {
		require.NotEqualf(t, up, s.TraceID,
			"%q shares upstream %q's trace %s — propagation was not disabled", downstream, upstream, up)
		for _, l := range s.Links {
			require.NotEqualf(t, up, l.TraceID,
				"%q links to upstream %q's trace %s — that is span-link propagation, not a severed trace", downstream, upstream, up)
		}
	}
}

func traceIDKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
