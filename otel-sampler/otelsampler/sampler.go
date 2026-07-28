package otelsampler

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultSamplingPrecision = 4
	maxAdjustedCount         = 1 << 56
	randomnessMask           = maxAdjustedCount - 1
	probabilityZeroThreshold = 1 / float64(maxAdjustedCount)
)

type probabilitySampler struct {
	threshold   uint64
	thkv        string
	description string
}

// ShouldSample implements sdktrace.Sampler.
func (ps *probabilitySampler) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	parentSC := trace.SpanContextFromContext(params.ParentContext)
	state := parentSC.TraceState()
	existingOT := state.Get("ot")

	var randomness uint64
	var hasRandomness bool
	if existingOT != "" {
		randomness, hasRandomness = tracestateRandomness(existingOT)
	}

	// Match OpenTelemetry Go PR #8123: presume the trace ID is random when
	// explicit tracestate randomness is absent.
	if !hasRandomness {
		randomness = binary.BigEndian.Uint64(params.TraceID[8:16]) & randomnessMask
	}

	if ps.threshold > randomness {
		return sdktrace.SamplingResult{
			Decision:   sdktrace.Drop,
			Tracestate: state,
		}
	}

	newOT := insertOrUpdateTraceStateThKeyValue(existingOT, ps.thkv)
	if newOT == "" {
		state = state.Delete("ot")
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: state}
	}
	if existingOT == newOT {
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: state}
	}

	combined, err := state.Insert("ot", newOT)
	if err != nil {
		otel.Handle(fmt.Errorf("could not combine tracestate: %w", err))
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: state}
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: combined}
}

// Description implements sdktrace.Sampler.
func (ps *probabilitySampler) Description() string {
	return ps.description
}

// ProbabilitySampler samples traces with a threshold-based probability sampler.
//
// This mirrors the experimental OpenTelemetry Go PR #8123 ProbabilitySampler:
// it reads randomness from the parent "ot=rv:..." tracestate when present,
// otherwise derives randomness from the least significant 56 bits of the trace
// ID, and writes the selected threshold to "ot=th:..." when recording.
func ProbabilitySampler(probability float64) sdktrace.Sampler {
	const (
		maxPrecision = 14
		hexBits      = 4
	)
	if probability >= 1.0 {
		return &probabilitySampler{
			threshold:   0,
			thkv:        "th:0",
			description: "ProbabilitySampler{1}",
		}
	}
	if math.IsNaN(probability) || probability < probabilityZeroThreshold {
		return sdktrace.NeverSample()
	}

	_, expF := math.Frexp(probability)
	_, expR := math.Frexp(1 - probability)
	precision := min(maxPrecision, max(defaultSamplingPrecision+expF/-hexBits, defaultSamplingPrecision+expR/-hexBits))

	scaled := uint64(math.Round(probability * float64(maxAdjustedCount)))
	threshold := uint64(maxAdjustedCount) - scaled
	if shift := hexBits * (maxPrecision - precision); shift != 0 {
		half := uint64(1) << (shift - 1)
		threshold += half
		threshold >>= shift
		threshold <<= shift
	}

	tvalue := strings.TrimRight(strconv.FormatUint(uint64(maxAdjustedCount)+threshold, 16)[1:], "0")
	return &probabilitySampler{
		threshold:   threshold,
		thkv:        "th:" + tvalue,
		description: fmt.Sprintf("ProbabilitySampler{%g}", probability),
	}
}

// ProbabilitySamplerFromEnv returns ProbabilitySampler configured from
// OTEL_TRACES_SAMPLER_ARG. The defaultProbability is used when the environment
// variable is unset or cannot be parsed as a float.
func ProbabilitySamplerFromEnv(defaultProbability float64) sdktrace.Sampler {
	probability, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")), 64)
	if err != nil {
		probability = defaultProbability
	}
	return ProbabilitySampler(probability)
}
