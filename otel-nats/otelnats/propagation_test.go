package otelnats_test

import (
	"testing"

	nats "github.com/nats-io/nats.go"

	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

func TestHeaderCarrier_NilHeader(t *testing.T) {
	var c otelnats.HeaderCarrier
	if got := c.Keys(); got != nil {
		t.Errorf("Keys() with nil header: got %v, want nil", got)
	}
	if got := c.Get("traceparent"); got != "" {
		t.Errorf("Get() with nil header: got %q", got)
	}
	c.Set("k", "v") // no-op, should not panic
}

func TestHeaderCarrier_NonNilHeader(t *testing.T) {
	h := nats.Header{}
	h.Set("traceparent", "00-abc-1-01")
	h.Set("tracestate", "x=y")
	c := otelnats.HeaderCarrier{H: h}

	keys := c.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys(): got len %d", len(keys))
	}
	if got := c.Get("traceparent"); got != "00-abc-1-01" {
		t.Errorf("Get(traceparent): got %q", got)
	}
	if got := c.Get("tracestate"); got != "x=y" {
		t.Errorf("Get(tracestate): got %q", got)
	}

	c.Set("new", "val")
	if got := c.Get("new"); got != "val" {
		t.Errorf("after Set: Get(new)=%q", got)
	}
}

func TestHeaderCarrier_Values_MultiInstance(t *testing.T) {
	h := nats.Header{}
	h.Add("baggage", "a=1")
	h.Add("baggage", "b=2")
	c := otelnats.HeaderCarrier{H: h}

	vals := c.Values("baggage")
	if len(vals) != 2 || vals[0] != "a=1" || vals[1] != "b=2" {
		t.Errorf("Values(baggage): got %v, want [a=1 b=2]", vals)
	}
}

func TestHeaderCarrier_Values_NilHeader(t *testing.T) {
	var c otelnats.HeaderCarrier
	if got := c.Values("baggage"); got != nil {
		t.Errorf("Values() with nil header: got %v, want nil", got)
	}
}

func TestHeaderCarrier_CanonicalFallback_Get(t *testing.T) {
	h := nats.Header{}
	h.Set("Traceparent", "00-canonical-1-01") // MIME-canonical form only
	c := otelnats.HeaderCarrier{H: h}

	if got := c.Get("traceparent"); got != "00-canonical-1-01" {
		t.Errorf("Get(traceparent) canonical fallback: got %q, want canonical value", got)
	}
}

func TestHeaderCarrier_CanonicalFallback_Values(t *testing.T) {
	h := nats.Header{}
	h.Add("Baggage", "a=1")
	h.Add("Baggage", "b=2")
	c := otelnats.HeaderCarrier{H: h}

	vals := c.Values("baggage")
	if len(vals) != 2 || vals[0] != "a=1" || vals[1] != "b=2" {
		t.Errorf("Values(baggage) canonical fallback: got %v, want [a=1 b=2]", vals)
	}
}

func TestHeaderCarrier_VerbatimWinsOverCanonical(t *testing.T) {
	h := nats.Header{}
	h.Set("traceparent", "00-verbatim-1-01")
	h.Set("Traceparent", "00-canonical-1-01")
	c := otelnats.HeaderCarrier{H: h}

	if got := c.Get("traceparent"); got != "00-verbatim-1-01" {
		t.Errorf("Get(traceparent): got %q, want verbatim entry (no merge with canonical)", got)
	}
	vals := c.Values("traceparent")
	if len(vals) != 1 || vals[0] != "00-verbatim-1-01" {
		t.Errorf("Values(traceparent): got %v, want [00-verbatim-1-01] only", vals)
	}
}

func TestHeaderCarrier_Set_WritesVerbatimKey(t *testing.T) {
	h := nats.Header{}
	c := otelnats.HeaderCarrier{H: h}
	c.Set("traceparent", "00-x-1-01")

	if _, ok := h["traceparent"]; !ok {
		t.Errorf("Set did not write verbatim key %q into underlying header: %v", "traceparent", h)
	}
}

// TestHeaderCarrier_VerbatimEmptyValueWinsOverCanonical pins presence-based
// fallback: a verbatim key present with an empty value must win over a
// non-empty canonical entry (the fallback triggers on key absence, not value
// emptiness), identically for Get and Values.
func TestHeaderCarrier_VerbatimEmptyValueWinsOverCanonical(t *testing.T) {
	h := nats.Header{
		"traceparent": {""},
		"Traceparent": {"00-canonical-1-01"},
	}
	c := otelnats.HeaderCarrier{H: h}

	if got := c.Get("traceparent"); got != "" {
		t.Errorf("Get(traceparent): got %q, want empty verbatim value (no canonical fallback)", got)
	}
	vals := c.Values("traceparent")
	if len(vals) != 1 || vals[0] != "" {
		t.Errorf(`Values(traceparent): got %v, want [""] (verbatim entry, no fallback)`, vals)
	}
}
