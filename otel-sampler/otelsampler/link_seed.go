package otelsampler

import (
	"encoding/binary"
	"fmt"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type singleLinkSeedSampler struct {
	delegate sdktrace.Sampler
}

// WithSingleLinkSeed wraps a sampler so root spans carry explicit randomness,
// using exactly one valid link as the sampling seed when available.
//
// The wrapper adjusts the sampling input and, on root paths, writes an explicit
// "ot=rv:" into the returned SamplingResult.Tracestate. It never changes the
// parentage or the span links the SDK creates: a linked root stays a new root
// with its own TraceID. When a valid parent already exists, or when there are
// zero or multiple valid links, the delegate receives the original sampling
// parameters.
//
// On the single-link path the delegate sees the link as a remote parent, but
// only the link's "ot=rv:" randomness is carried over: vendor tracestate
// members and the upstream "th:" are deliberately dropped so an unrelated new
// trace does not inherit them. The caller's ParentContext values (baggage, …)
// are preserved.
//
// Do not compose this with sdktrace.ParentBased: because the seed is presented
// as a valid remote parent, ParentBased would decide from the link's sampled
// flag and ignore the probability entirely, degrading to head-based sampling.
// For consistent sampling wrap ProbabilitySampler directly.
func WithSingleLinkSeed(delegate sdktrace.Sampler) sdktrace.Sampler {
	if delegate == nil {
		delegate = sdktrace.AlwaysSample()
	}
	return singleLinkSeedSampler{delegate: delegate}
}

// ShouldSample implements sdktrace.Sampler.
func (s singleLinkSeedSampler) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if trace.SpanContextFromContext(params.ParentContext).IsValid() {
		return s.delegate.ShouldSample(params)
	}

	linkSC, ok := singleValidLink(params.Links)
	if !ok {
		result := s.delegate.ShouldSample(params)
		return withRandomnessTraceState(result, randomnessFromTraceID(params.TraceID))
	}

	randomness := randomnessFromSpanContext(linkSC)
	// Seed with the link's randomness only. Copying the link's whole tracestate
	// would carry foreign vendor members — and, on the Drop path, the upstream
	// "th:" — onto a brand-new, unrelated trace.
	params.ParentContext = trace.ContextWithRemoteSpanContext(
		params.ParentContext, linkSC.WithTraceState(randomnessTraceState(randomness)))
	params.TraceID = linkSC.TraceID()
	result := s.delegate.ShouldSample(params)
	return withRandomnessTraceState(result, randomness)
}

// randomnessTraceState returns a tracestate carrying only "ot=rv:<14 hex>".
func randomnessTraceState(randomness uint64) trace.TraceState {
	state, err := trace.TraceState{}.Insert("ot", rvKeyValue(randomness))
	if err != nil {
		otel.Handle(fmt.Errorf("could not build randomness tracestate: %w", err))
		return trace.TraceState{}
	}
	return state
}

// Description implements sdktrace.Sampler.
func (s singleLinkSeedSampler) Description() string {
	return fmt.Sprintf("WithSingleLinkSeed{%s}", s.delegate.Description())
}

func singleValidLink(links []trace.Link) (trace.SpanContext, bool) {
	var found trace.SpanContext
	for _, link := range links {
		if !link.SpanContext.IsValid() {
			continue
		}
		if found.IsValid() {
			return trace.SpanContext{}, false
		}
		found = link.SpanContext
	}
	if !found.IsValid() {
		return trace.SpanContext{}, false
	}
	return found, true
}

func randomnessFromSpanContext(sc trace.SpanContext) uint64 {
	if existingOT := sc.TraceState().Get("ot"); existingOT != "" {
		if randomness, ok := tracestateRandomness(existingOT); ok {
			return randomness
		}
	}
	return randomnessFromTraceID(sc.TraceID())
}

func randomnessFromTraceID(traceID trace.TraceID) uint64 {
	return binary.BigEndian.Uint64(traceID[8:16]) & randomnessMask
}

func withRandomnessTraceState(result sdktrace.SamplingResult, randomness uint64) sdktrace.SamplingResult {
	state := result.Tracestate
	existingOT := state.Get("ot")
	newOT := insertOrUpdateOTSubKey(existingOT, "rv:", rvKeyValue(randomness))
	if existingOT == newOT {
		return result
	}

	combined, err := state.Insert("ot", newOT)
	if err != nil {
		otel.Handle(fmt.Errorf("could not combine randomness tracestate: %w", err))
		return result
	}
	result.Tracestate = combined
	return result
}

// rvKeyValue renders the "rv:" tracestate sub-key for a 56-bit randomness value.
func rvKeyValue(randomness uint64) string {
	return fmt.Sprintf("rv:%014x", randomness&randomnessMask)
}
