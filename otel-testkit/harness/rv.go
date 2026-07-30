package harness

import (
	"context"
	"crypto/rand"
	"encoding/binary"

	"go.opentelemetry.io/otel/trace"
)

// SeedContextRV returns a context carrying a synthetic remote parent whose
// tracestate holds the chosen consistent-sampling randomness value (rv). Use it
// as the head of a flow so the first service's ProbabilitySampler reads exactly
// rv — equivalent to a normal inbound carrier (W3C traceparent + tracestate)
// arriving with that randomness. Downstream services then receive the same rv
// through the instrumentation library's real propagation.
func SeedContextRV(rv uint64) context.Context {
	ts, err := trace.ParseTraceState("ot=rv:" + formatRV(rv))
	if err != nil {
		ts = trace.TraceState{}
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    randTraceID(),
		SpanID:     randSpanID(),
		TraceFlags: trace.FlagsSampled,
		TraceState: ts,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(context.Background(), sc)
}

// RandomRV returns a uniformly random 56-bit randomness value, for statistical
// sampling-rate tests (drive many traces, assert the sampled fraction ≈ rate).
//
// Prefer UniformRVs for CI: a random draw makes the sampled fraction binomial,
// so the assertion needs a wide tolerance to avoid periodic flakes.
func RandomRV() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:]) & randomnessMask
}

// UniformRVs returns n randomness values spread evenly over the 56-bit space
// (the midpoint of each of n equal buckets). Driving one run per value makes a
// sampling-rate check deterministic: the fraction of values a rate samples is
// within 1/n of the rate for every rate, so the test still detects a wrong
// threshold while never flaking. Returns nil for n <= 0.
func UniformRVs(n int) []uint64 {
	if n <= 0 {
		return nil
	}
	span := (randomnessMask + 1) / uint64(n)
	out := make([]uint64, 0, n)
	for k := range n {
		out = append(out, (uint64(k)*span+span/2)&randomnessMask)
	}
	return out
}

func randTraceID() trace.TraceID {
	var id trace.TraceID
	_, _ = rand.Read(id[:])
	id[0] |= 0x01 // ensure non-zero (valid)
	return id
}

func randSpanID() trace.SpanID {
	var id trace.SpanID
	_, _ = rand.Read(id[:])
	id[0] |= 0x01
	return id
}
