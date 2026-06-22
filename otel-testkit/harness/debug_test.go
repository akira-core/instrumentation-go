package harness

import (
	"strings"
	"testing"
)

func TestSpanString(t *testing.T) {
	s := Span{
		ServiceName:  "svc1",
		Scope:        "otelmongo",
		Name:         "insert",
		TraceID:      "3f2a9c0111111111",
		SpanID:       "88ab12cd22222222",
		ParentSpanID: "",
		TraceState:   "ot=rv:2a000000000000",
		Attributes:   map[string]string{RunAttr: "run-7b3e"},
		Links:        []Link{{TraceID: "deadbeef99999999"}},
	}
	got := s.String()
	for _, want := range []string{
		"svc=svc1", "scope=otelmongo", "name=insert",
		"trace=3f2a9c01", "span=88ab12cd", "parent=-",
		"rv=2a000000000000", "th=-", "links=[deadbeef]", "run=run-7b3e",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Span.String() = %q, missing %q", got, want)
		}
	}
}

func TestSpanStringEmpty(t *testing.T) {
	got := (Span{}).String()
	if !strings.Contains(got, "rv=-") || !strings.Contains(got, "run=-") || !strings.Contains(got, "links=[]") {
		t.Errorf("empty Span.String() = %q", got)
	}
}

func TestDumpSpansEmptyNoPanic(t *testing.T) {
	DumpSpans(t, "empty", nil)
}
