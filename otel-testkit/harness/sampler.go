package harness

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/akira-core/instrumentation-go/otel-sampler/otelsampler"
)

// ConsistentSampler returns the sampler a service should use for consistent
// probabilistic sampling at the given rate: a ProbabilitySampler wrapped with
// WithSingleLinkSeed. The wrapper is required, not optional —
//
//   - span-link consumers (new root + a link) read their randomness from the
//     link via WithSingleLinkSeed; a bare ProbabilitySampler ignores links and
//     would derive a fresh, inconsistent rv from the new trace ID; and
//   - it writes the explicit "ot=rv:" into the root span's tracestate, so the
//     randomness is actually emitted (a bare ProbabilitySampler only writes
//     "ot=th:" when sampling).
//
// Use this (or ConsistentSamplerFromEnv) wherever you build a service
// TracerProvider — including real applications under test.
func ConsistentSampler(rate float64) sdktrace.Sampler {
	return otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySampler(rate))
}

// ConsistentSamplerFromEnv is ConsistentSampler with the probability read from
// OTEL_TRACES_SAMPLER_ARG (falling back to def), mirroring how a deployed
// service is configured.
func ConsistentSamplerFromEnv(def float64) sdktrace.Sampler {
	return otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySamplerFromEnv(def))
}
