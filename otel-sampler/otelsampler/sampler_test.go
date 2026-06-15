package otelsampler

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestProbabilitySamplerDeterministicSubset verifies that higher probabilities
// include every trace sampled by lower probabilities for the same randomness.
func TestProbabilitySamplerDeterministicSubset(t *testing.T) {
	t.Parallel()

	sampler01 := ProbabilitySampler(0.1)
	sampler02 := ProbabilitySampler(0.2)
	sampler05 := ProbabilitySampler(0.5)
	for _, randomness := range []uint64{
		0x00000000000000,
		0x66666666666666,
		0x99999999999999,
		0xd9999999999999,
		0xf4000000000000,
		0xffffffffffffff,
	} {
		params := samplingParams(traceIDWithRandomness(randomness))
		sampled01 := sampler01.ShouldSample(params).Decision == sdktrace.RecordAndSample
		sampled02 := sampler02.ShouldSample(params).Decision == sdktrace.RecordAndSample
		sampled05 := sampler05.ShouldSample(params).Decision == sdktrace.RecordAndSample

		if sampled01 {
			assert.True(t, sampled02, "0.2 must include 0.1 for randomness %#x", randomness)
		}
		if sampled02 {
			assert.True(t, sampled05, "0.5 must include 0.2 for randomness %#x", randomness)
		}
	}
}

// TestProbabilitySamplerServiceThresholdMatrix verifies the A/B/C/D/E service
// threshold relationship using only the bare ProbabilitySampler.
func TestProbabilitySamplerServiceThresholdMatrix(t *testing.T) {
	t.Parallel()

	services := []struct {
		name        string
		probability float64
	}{
		{name: "A", probability: 0.1},
		{name: "B", probability: 0.5},
		{name: "C", probability: 0.5},
		{name: "D", probability: 0.1},
		{name: "E", probability: 0.2},
	}
	samplers := make(map[string]sdktrace.Sampler, len(services))
	for _, service := range services {
		samplers[service.name] = ProbabilitySampler(service.probability)
	}

	for _, tc := range []struct {
		name       string
		randomness uint64
		want       map[string]bool
	}{
		{
			name:       "below all thresholds",
			randomness: 0x70000000000000,
			want: map[string]bool{
				"A": false,
				"B": false,
				"C": false,
				"D": false,
				"E": false,
			},
		},
		{
			name:       "inside 0.5 only",
			randomness: 0x90000000000000,
			want: map[string]bool{
				"A": false,
				"B": true,
				"C": true,
				"D": false,
				"E": false,
			},
		},
		{
			name:       "inside 0.2 and 0.5",
			randomness: 0xd0000000000000,
			want: map[string]bool{
				"A": false,
				"B": true,
				"C": true,
				"D": false,
				"E": true,
			},
		},
		{
			name:       "inside all thresholds",
			randomness: 0xf0000000000000,
			want: map[string]bool{
				"A": true,
				"B": true,
				"C": true,
				"D": true,
				"E": true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := make(map[string]bool, len(services))
			for _, service := range services {
				result := samplers[service.name].ShouldSample(samplingParams(traceIDWithRandomness(tc.randomness)))
				got[service.name] = result.Decision == sdktrace.RecordAndSample
				assert.Equal(t, tc.want[service.name], got[service.name], "service %s", service.name)
				if got[service.name] {
					assert.Contains(t, result.Tracestate.Get("ot"), "th:", "service %s", service.name)
					assert.NotContains(t, result.Tracestate.Get("ot"), "rv:", "service %s", service.name)
				} else {
					assert.Empty(t, result.Tracestate.Get("ot"), "service %s", service.name)
				}
			}

			assert.Equal(t, got["B"], got["C"], "same probability services must make the same decision")
			assert.Equal(t, got["A"], got["D"], "same probability services must make the same decision")
			if got["A"] {
				assert.True(t, got["E"], "0.2 must include 0.1")
				assert.True(t, got["B"], "0.5 must include 0.1")
				assert.True(t, got["C"], "0.5 must include 0.1")
			}
			if got["E"] {
				assert.True(t, got["B"], "0.5 must include 0.2")
				assert.True(t, got["C"], "0.5 must include 0.2")
			}
		})
	}
}

// TestProbabilitySamplerSameTraceIDProducesSameDecision verifies that a fixed
// trace ID yields a stable decision and tracestate.
func TestProbabilitySamplerSameTraceIDProducesSameDecision(t *testing.T) {
	t.Parallel()

	sampler := ProbabilitySampler(0.1)
	traceID := traceIDWithRandomness(0xf4000000000000)

	first := sampler.ShouldSample(samplingParams(traceID))
	second := sampler.ShouldSample(samplingParams(traceID))

	assert.Equal(t, first.Decision, second.Decision)
	assert.Equal(t, first.Tracestate.String(), second.Tracestate.String())
}

// TestProbabilitySamplerUsesExplicitRandomnessBeforeTraceID verifies that
// parent tracestate rv takes precedence over the current trace ID randomness.
func TestProbabilitySamplerUsesExplicitRandomnessBeforeTraceID(t *testing.T) {
	t.Parallel()

	sampler := ProbabilitySampler(0.5)
	parent := spanContext(t, traceIDWithRandomness(0x00000000000000), "0000000000000001", "ot=rv:f0000000000000")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), parent)

	result := sampler.ShouldSample(params)

	require.Equal(t, sdktrace.RecordAndSample, result.Decision)
	assert.Contains(t, result.Tracestate.Get("ot"), "th:")
	assert.Contains(t, result.Tracestate.Get("ot"), "rv:f0000000000000")
}

// TestProbabilitySamplerFallsBackToTraceIDRandomness verifies that trace ID
// randomness is used when no explicit rv is present.
func TestProbabilitySamplerFallsBackToTraceIDRandomness(t *testing.T) {
	t.Parallel()

	sampler := ProbabilitySampler(0.5)

	sampled := sampler.ShouldSample(samplingParams(traceIDWithRandomness(0xf0000000000000)))
	require.Equal(t, sdktrace.RecordAndSample, sampled.Decision)
	assert.Contains(t, sampled.Tracestate.Get("ot"), "th:")
	assert.NotContains(t, sampled.Tracestate.Get("ot"), "rv:")

	dropped := sampler.ShouldSample(samplingParams(traceIDWithRandomness(0x00000000000000)))
	assert.Equal(t, sdktrace.Drop, dropped.Decision)
	assert.Empty(t, dropped.Tracestate.Get("ot"))
}

// TestProbabilitySamplerInvalidRandomnessFallsBackToTraceID verifies that an
// invalid rv is ignored instead of driving the decision.
func TestProbabilitySamplerInvalidRandomnessFallsBackToTraceID(t *testing.T) {
	t.Parallel()

	sampler := ProbabilitySampler(0.5)
	parent := spanContext(t, traceIDWithRandomness(0xf0000000000000), "0000000000000001", "ot=rv:not-hex-value")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), parent)

	result := sampler.ShouldSample(params)

	assert.Equal(t, sdktrace.Drop, result.Decision)
}

// TestProbabilitySamplerFromEnv verifies that OTEL_TRACES_SAMPLER_ARG controls
// the probability used by ProbabilitySamplerFromEnv.
func TestProbabilitySamplerFromEnv(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")

	result := ProbabilitySamplerFromEnv(1).ShouldSample(samplingParams(traceIDWithRandomness(0xffffffffffffff)))

	assert.Equal(t, sdktrace.Drop, result.Decision)
}

// TestWithSingleLinkSeedUsesLinkRandomness verifies that a single valid link
// provides the rv used by the wrapped sampler.
func TestWithSingleLinkSeedUsesLinkRandomness(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
	link := spanContext(t, traceIDWithRandomness(0x00000000000000), "0000000000000001", "ot=rv:f0000000000000")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.Links = []trace.Link{{SpanContext: link}}

	result := sampler.ShouldSample(params)

	require.Equal(t, sdktrace.RecordAndSample, result.Decision)
	assert.Contains(t, result.Tracestate.Get("ot"), "rv:f0000000000000")
}

// TestWithSingleLinkSeedUsesLinkTraceIDWhenRandomnessAbsent verifies that the
// link trace ID seeds rv when the link has no explicit rv.
func TestWithSingleLinkSeedUsesLinkTraceIDWhenRandomnessAbsent(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
	link := spanContext(t, traceIDWithRandomness(0xf0000000000000), "0000000000000001", "")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.Links = []trace.Link{{SpanContext: link}}

	result := sampler.ShouldSample(params)

	assert.Equal(t, sdktrace.RecordAndSample, result.Decision)
	assert.Contains(t, result.Tracestate.Get("ot"), "rv:f0000000000000")
}

// TestWithSingleLinkSeedUsesParentBeforeLink verifies that an existing parent
// context takes precedence over any links.
func TestWithSingleLinkSeedUsesParentBeforeLink(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
	parent := spanContext(t, traceIDWithRandomness(0x00000000000000), "0000000000000001", "")
	link := spanContext(t, traceIDWithRandomness(0xf0000000000000), "0000000000000002", "")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), parent)
	params.Links = []trace.Link{{SpanContext: link}}

	result := sampler.ShouldSample(params)

	assert.Equal(t, sdktrace.Drop, result.Decision)
	assert.NotContains(t, result.Tracestate.Get("ot"), "rv:f0000000000000")
}

// TestWithSingleLinkSeedFallsBackWithoutExactlyOneValidLink verifies that link
// seeding is skipped unless there is exactly one valid link.
func TestWithSingleLinkSeedFallsBackWithoutExactlyOneValidLink(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
	first := spanContext(t, traceIDWithRandomness(0xf0000000000000), "0000000000000001", "")
	second := spanContext(t, traceIDWithRandomness(0xf0000000000001), "0000000000000002", "")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.Links = []trace.Link{
		{SpanContext: first},
		{SpanContext: second},
	}

	result := sampler.ShouldSample(params)

	assert.Equal(t, sdktrace.Drop, result.Decision)
}

// TestWithSingleLinkSeedWritesRootRandomness verifies that root spans receive
// an explicit rv even when the wrapped sampler drops the span.
func TestWithSingleLinkSeedWritesRootRandomness(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))

	sampled := sampler.ShouldSample(samplingParams(traceIDWithRandomness(0xf0000000000000)))
	require.Equal(t, sdktrace.RecordAndSample, sampled.Decision)
	assert.Contains(t, sampled.Tracestate.Get("ot"), "rv:f0000000000000")

	dropped := sampler.ShouldSample(samplingParams(traceIDWithRandomness(0x00000000000000)))
	require.Equal(t, sdktrace.Drop, dropped.Decision)
	assert.Contains(t, dropped.Tracestate.Get("ot"), "rv:00000000000000")
}

// TestWithSingleLinkSeedPreservesRandomnessAcrossLinkedChain verifies that rv
// is preserved across repeated single-link sampling decisions.
func TestWithSingleLinkSeedPreservesRandomnessAcrossLinkedChain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		randomness uint64
		decision   sdktrace.SamplingDecision
	}{
		{
			name:       "sampled",
			randomness: 0xf0000000000000,
			decision:   sdktrace.RecordAndSample,
		},
		{
			name:       "dropped",
			randomness: 0x00000000000000,
			decision:   sdktrace.Drop,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
			expectedRV := fmt.Sprintf("rv:%014x", tc.randomness)

			aTraceID := traceIDWithRandomness(tc.randomness)
			aResult := sampler.ShouldSample(samplingParams(aTraceID))
			require.Equal(t, tc.decision, aResult.Decision)
			require.Contains(t, aResult.Tracestate.Get("ot"), expectedRV)
			aSC := spanContextFromResult(t, aTraceID, "0000000000000001", aResult)

			bTraceID := traceIDWithRandomness(tc.randomness ^ randomnessMask)
			bParams := samplingParams(bTraceID)
			bParams.Links = []trace.Link{{SpanContext: aSC}}
			bResult := sampler.ShouldSample(bParams)
			require.Equal(t, aResult.Decision, bResult.Decision)
			require.Contains(t, bResult.Tracestate.Get("ot"), expectedRV)
			bSC := spanContextFromResult(t, bTraceID, "0000000000000002", bResult)

			cTraceID := traceIDWithRandomness(tc.randomness ^ randomnessMask)
			cParams := samplingParams(cTraceID)
			cParams.Links = []trace.Link{{SpanContext: bSC}}
			cResult := sampler.ShouldSample(cParams)
			require.Equal(t, aResult.Decision, cResult.Decision)
			require.Contains(t, cResult.Tracestate.Get("ot"), expectedRV)
			cSC := spanContextFromResult(t, cTraceID, "0000000000000003", cResult)

			dParams := samplingParams(cTraceID)
			dParams.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), cSC)
			dResult := sampler.ShouldSample(dParams)
			assert.Equal(t, aResult.Decision, dResult.Decision)
			assert.Contains(t, dResult.Tracestate.Get("ot"), expectedRV)
		})
	}
}

// TestWithSingleLinkSeedServiceThresholdChain verifies the A/B/C/D/E threshold
// relationship across a mixed link and parent-child chain.
func TestWithSingleLinkSeedServiceThresholdChain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		randomness uint64
		want       map[string]sdktrace.SamplingDecision
	}{
		{
			name:       "below all thresholds",
			randomness: 0x70000000000000,
			want: map[string]sdktrace.SamplingDecision{
				"A": sdktrace.Drop,
				"B": sdktrace.Drop,
				"C": sdktrace.Drop,
				"D": sdktrace.Drop,
				"E": sdktrace.Drop,
			},
		},
		{
			name:       "inside 0.5 only",
			randomness: 0x90000000000000,
			want: map[string]sdktrace.SamplingDecision{
				"A": sdktrace.Drop,
				"B": sdktrace.RecordAndSample,
				"C": sdktrace.RecordAndSample,
				"D": sdktrace.Drop,
				"E": sdktrace.Drop,
			},
		},
		{
			name:       "inside 0.2 and 0.5",
			randomness: 0xd0000000000000,
			want: map[string]sdktrace.SamplingDecision{
				"A": sdktrace.Drop,
				"B": sdktrace.RecordAndSample,
				"C": sdktrace.RecordAndSample,
				"D": sdktrace.Drop,
				"E": sdktrace.RecordAndSample,
			},
		},
		{
			name:       "inside all thresholds",
			randomness: 0xf0000000000000,
			want: map[string]sdktrace.SamplingDecision{
				"A": sdktrace.RecordAndSample,
				"B": sdktrace.RecordAndSample,
				"C": sdktrace.RecordAndSample,
				"D": sdktrace.RecordAndSample,
				"E": sdktrace.RecordAndSample,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expectedRV := fmt.Sprintf("rv:%014x", tc.randomness)

			aTraceID := traceIDWithRandomness(tc.randomness)
			aResult := WithSingleLinkSeed(ProbabilitySampler(0.1)).ShouldSample(samplingParams(aTraceID))
			require.Equal(t, tc.want["A"], aResult.Decision)
			require.Contains(t, aResult.Tracestate.Get("ot"), expectedRV)
			aSC := spanContextFromResult(t, aTraceID, "0000000000000001", aResult)

			bTraceID := traceIDWithRandomness(tc.randomness ^ randomnessMask)
			bParams := samplingParams(bTraceID)
			bParams.Links = []trace.Link{{SpanContext: aSC}}
			bResult := WithSingleLinkSeed(ProbabilitySampler(0.5)).ShouldSample(bParams)
			require.Equal(t, tc.want["B"], bResult.Decision)
			require.Contains(t, bResult.Tracestate.Get("ot"), expectedRV)
			bSC := spanContextFromResult(t, bTraceID, "0000000000000002", bResult)

			cTraceID := traceIDWithRandomness(tc.randomness ^ 0x01000000000000)
			cParams := samplingParams(cTraceID)
			cParams.Links = []trace.Link{{SpanContext: bSC}}
			cResult := WithSingleLinkSeed(ProbabilitySampler(0.5)).ShouldSample(cParams)
			require.Equal(t, tc.want["C"], cResult.Decision)
			require.Contains(t, cResult.Tracestate.Get("ot"), expectedRV)
			cSC := spanContextFromResult(t, cTraceID, "0000000000000003", cResult)

			dTraceID := traceIDWithRandomness(tc.randomness ^ 0x02000000000000)
			dParams := samplingParams(dTraceID)
			dParams.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), cSC)
			dResult := WithSingleLinkSeed(ProbabilitySampler(0.1)).ShouldSample(dParams)
			require.Equal(t, tc.want["D"], dResult.Decision)
			require.Contains(t, dResult.Tracestate.Get("ot"), expectedRV)
			dSC := spanContextFromResult(t, dTraceID, "0000000000000004", dResult)

			eTraceID := traceIDWithRandomness(tc.randomness ^ 0x03000000000000)
			eParams := samplingParams(eTraceID)
			eParams.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), dSC)
			eResult := WithSingleLinkSeed(ProbabilitySampler(0.2)).ShouldSample(eParams)
			require.Equal(t, tc.want["E"], eResult.Decision)
			require.Contains(t, eResult.Tracestate.Get("ot"), expectedRV)

			assert.Equal(t, bResult.Decision, cResult.Decision, "B and C use the same threshold")
			assert.Equal(t, aResult.Decision, dResult.Decision, "A and D use the same threshold")
			if aResult.Decision == sdktrace.RecordAndSample {
				assert.Equal(t, sdktrace.RecordAndSample, eResult.Decision, "0.2 must include 0.1")
				assert.Equal(t, sdktrace.RecordAndSample, bResult.Decision, "0.5 must include 0.1")
				assert.Equal(t, sdktrace.RecordAndSample, cResult.Decision, "0.5 must include 0.1")
			}
			if eResult.Decision == sdktrace.RecordAndSample {
				assert.Equal(t, sdktrace.RecordAndSample, bResult.Decision, "0.5 must include 0.2")
				assert.Equal(t, sdktrace.RecordAndSample, cResult.Decision, "0.5 must include 0.2")
			}
		})
	}
}

func samplingParams(traceID trace.TraceID) sdktrace.SamplingParameters {
	return sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		TraceID:       traceID,
		Name:          "test",
	}
}

func traceIDWithRandomness(randomness uint64) trace.TraceID {
	var traceID trace.TraceID
	traceID[0] = 1
	binary.BigEndian.PutUint64(traceID[8:16], randomness&randomnessMask)
	return traceID
}

func spanContext(t *testing.T, traceID trace.TraceID, spanIDHex, traceStateText string) trace.SpanContext {
	t.Helper()

	spanID, err := trace.SpanIDFromHex(spanIDHex)
	require.NoError(t, err)
	cfg := trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
		Remote:  true,
	}
	if traceStateText != "" {
		state, err := trace.ParseTraceState(traceStateText)
		require.NoError(t, err)
		cfg.TraceState = state
	}
	return trace.NewSpanContext(cfg)
}

func spanContextFromResult(
	t *testing.T,
	traceID trace.TraceID,
	spanIDHex string,
	result sdktrace.SamplingResult,
) trace.SpanContext {
	t.Helper()

	spanID, err := trace.SpanIDFromHex(spanIDHex)
	require.NoError(t, err)
	cfg := trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceState: result.Tracestate,
		Remote:     true,
	}
	if result.Decision == sdktrace.RecordAndSample {
		cfg.TraceFlags = trace.FlagsSampled
	}
	return trace.NewSpanContext(cfg)
}
