package harness

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// String renders the span as a single readable line for failure diagnosis:
// service, scope, name, shortened trace/span/parent IDs, the consistent-sampling
// rv/th (or "-" when absent), the linked trace IDs, and the RunAttr value.
func (s Span) String() string {
	rv := "-"
	if v, ok := s.RV(); ok {
		rv = formatRV(v)
	}
	th := "-"
	if v, ok := s.TH(); ok {
		th = v
	}
	links := make([]string, 0, len(s.Links))
	for _, l := range s.Links {
		links = append(links, short(l.TraceID))
	}
	run := "-"
	if v, ok := s.Attributes[RunAttr]; ok && v != "" {
		run = v
	}
	return fmt.Sprintf("svc=%s scope=%s name=%s trace=%s span=%s parent=%s rv=%s th=%s links=[%s] run=%s",
		orDash(s.ServiceName), orDash(s.Scope), orDash(s.Name),
		short(s.TraceID), short(s.SpanID), short(s.ParentSpanID),
		rv, th, strings.Join(links, " "), run)
}

// DumpSpans logs header followed by one line per span (sorted by trace then span
// ID so spans of the same trace group together), using t.Logf. Call it when an
// assertion fails to see exactly what reached the sink. Logs "(no spans)" when
// the set is empty.
func DumpSpans(t *testing.T, header string, spans []Span) {
	t.Helper()
	if len(spans) == 0 {
		t.Logf("%s: (no spans)", header)
		return
	}
	sorted := make([]Span, len(spans))
	copy(sorted, spans)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].TraceID != sorted[j].TraceID {
			return sorted[i].TraceID < sorted[j].TraceID
		}
		return sorted[i].SpanID < sorted[j].SpanID
	})
	t.Logf("%s: %d span(s)", header, len(sorted))
	for _, s := range sorted {
		t.Logf("  %s", s.String())
	}
}

// DumpOnFailure registers a t.Cleanup that dumps every span the sink received if
// the test failed. Add it one line after StartSink to get an automatic span dump
// on any assertion failure — the fastest way to see why a sampling assertion did
// not hold (missing service, inconsistent rv, or a missing RunAttr).
func DumpOnFailure(t *testing.T, sink *Sink) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			DumpSpans(t, "spans at failure", sink.Spans())
		}
	})
}

// short returns the first 8 hex chars of an ID (or "-" / the whole string if
// shorter), enough to eyeball trace/span relationships in a dump.
func short(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
