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
func RandomRV() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:]) & randomnessMask
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
