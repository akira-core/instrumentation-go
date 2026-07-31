package otelsampler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestWithSingleLinkSeedDropsLinkVendorTraceState pins that a linked root does
// not inherit the producer's foreign tracestate members. The linked span starts
// a brand-new trace; carrying "congo=…" across would feed an unrelated vendor
// entry to every downstream hop of that new trace.
func TestWithSingleLinkSeedDropsLinkVendorTraceState(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
	link := spanContext(t, traceIDWithRandomness(0x00000000000000), "0000000000000001",
		"ot=rv:f0000000000000;th:8,congo=t61rcWkgMzE")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.Links = []trace.Link{{SpanContext: link}}

	result := sampler.ShouldSample(params)

	require.Equal(t, sdktrace.RecordAndSample, result.Decision)
	assert.Contains(t, result.Tracestate.Get("ot"), "rv:f0000000000000")
	assert.Empty(t, result.Tracestate.Get("congo"), "vendor member must not cross the link into a new trace")
}

// TestWithSingleLinkSeedDropPathOmitsUpstreamThreshold pins that a dropped
// linked root does not carry the producer's "th:". Keeping it would make a
// downstream exporter compute the adjusted count from the upstream probability.
func TestWithSingleLinkSeedDropPathOmitsUpstreamThreshold(t *testing.T) {
	t.Parallel()

	// rate 0.5 drops rv=0, so the delegate returns the seed tracestate as-is.
	sampler := WithSingleLinkSeed(ProbabilitySampler(0.5))
	link := spanContext(t, traceIDWithRandomness(0xf0000000000000), "0000000000000001",
		"ot=rv:00000000000000;th:8,congo=t61rcWkgMzE")
	params := samplingParams(traceIDWithRandomness(0xf0000000000000))
	params.Links = []trace.Link{{SpanContext: link}}

	result := sampler.ShouldSample(params)

	require.Equal(t, sdktrace.Drop, result.Decision)
	ot := result.Tracestate.Get("ot")
	assert.Contains(t, ot, "rv:00000000000000", "the seed must still propagate for later hops")
	assert.NotContains(t, ot, "th:", "a dropped linked root must not inherit the upstream threshold")
	assert.Empty(t, result.Tracestate.Get("congo"))
}

type ctxProbeKey struct{}

// probeDelegate records the ParentContext it was handed.
type probeDelegate struct {
	seen context.Context
}

func (p *probeDelegate) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	p.seen = params.ParentContext
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (p *probeDelegate) Description() string { return "probe" }

// TestWithSingleLinkSeedPreservesParentContextValues pins that the delegate
// still sees the caller's context values (baggage, tenant IDs, …) on the
// single-link path — replacing them with context.Background() would make a
// value-reading delegate decide differently than on the parent-child path.
func TestWithSingleLinkSeedPreservesParentContextValues(t *testing.T) {
	t.Parallel()

	probe := &probeDelegate{}
	sampler := WithSingleLinkSeed(probe)
	link := spanContext(t, traceIDWithRandomness(0xf0000000000000), "0000000000000001", "")
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.ParentContext = context.WithValue(context.Background(), ctxProbeKey{}, "tenant-a")
	params.Links = []trace.Link{{SpanContext: link}}

	sampler.ShouldSample(params)

	require.NotNil(t, probe.seen)
	assert.Equal(t, "tenant-a", probe.seen.Value(ctxProbeKey{}),
		"caller context values must survive link seeding")
	assert.Equal(t, link.SpanID(), trace.SpanContextFromContext(probe.seen).SpanID(),
		"the link must still be presented as the remote parent")
}

// TestWithSingleLinkSeedParentBasedDegradesToHeadBased documents the composition
// footgun called out in WithSingleLinkSeed's godoc: because the seed is a valid
// remote parent, ParentBased decides from the link's sampled flag and ignores
// the probability. Pinned so the behavior cannot change silently.
func TestWithSingleLinkSeedParentBasedDegradesToHeadBased(t *testing.T) {
	t.Parallel()

	sampler := WithSingleLinkSeed(sdktrace.ParentBased(sdktrace.NeverSample()))
	// rv would be dropped by NeverSample, but the link is sampled.
	link := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceIDWithRandomness(0xf0000000000000),
		SpanID:     trace.SpanID{0, 0, 0, 0, 0, 0, 0, 1},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	params := samplingParams(traceIDWithRandomness(0x00000000000000))
	params.Links = []trace.Link{{SpanContext: link}}

	result := sampler.ShouldSample(params)

	assert.Equal(t, sdktrace.RecordAndSample, result.Decision,
		"ParentBased follows the link's sampled flag — do not compose it with WithSingleLinkSeed")
}

// TestThresholdMatchesSamplerDecision pins the exported Threshold/Sampled
// helpers against the real sampler across the whole randomness range, including
// values sitting exactly on a threshold boundary.
func TestThresholdMatchesSamplerDecision(t *testing.T) {
	t.Parallel()

	for _, p := range []float64{0, 1e-9, 0.001, 0.1, 0.25, 0.5, 0.9, 0.999, 1.0} {
		sampler := ProbabilitySampler(p)
		th := Threshold(p)
		for _, rv := range []uint64{0, 1, th - 1, th, th + 1, 1 << 53, 1 << 55, randomnessMask} {
			rv &= randomnessMask
			got := sampler.ShouldSample(samplingParams(traceIDWithRandomness(rv))).Decision == sdktrace.RecordAndSample
			assert.Equalf(t, got, Sampled(p, rv), "p=%g rv=%#x threshold=%#x", p, rv, th)
		}
	}
}
