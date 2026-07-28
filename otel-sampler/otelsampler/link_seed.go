package otelsampler

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

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
// The wrapper only changes sampler input. It does not change parentage or span
// links. When a valid parent already exists, or when there are zero or multiple
// valid links, the delegate receives the original sampling parameters.
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

	params.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), linkSC)
	params.TraceID = linkSC.TraceID()
	result := s.delegate.ShouldSample(params)
	return withRandomnessTraceState(result, randomnessFromSpanContext(linkSC))
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
	newOT := insertOrUpdateTraceStateRvKeyValue(existingOT, randomness)
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

func insertOrUpdateTraceStateRvKeyValue(existingOT string, randomness uint64) string {
	rvkv := fmt.Sprintf("rv:%014x", randomness&randomnessMask)
	if existingOT == "" {
		return rvkv
	}

	start := -1
	var end int
	if strings.HasPrefix(existingOT, "rv:") {
		start = 0
	} else if idx := strings.Index(existingOT, ";rv:"); idx != -1 {
		start = idx + 1
	}
	if start == -1 {
		return rvkv + ";" + existingOT
	}

	for end = start; end < len(existingOT); end++ {
		if existingOT[end] == ';' {
			end++
			break
		}
	}

	if end == len(existingOT) {
		return strings.TrimSuffix(rvkv+";"+existingOT[:start], ";")
	}
	return rvkv + ";" + existingOT[:start] + existingOT[end:]
}
