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
