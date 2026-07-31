package otelsampler_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-sampler/otelsampler"
)

const sdkRandomnessMask = 1<<56 - 1

// These tests exercise the sampler through the real OpenTelemetry SDK and
// TraceContext propagator. They intentionally do not involve transport
// instrumentation; NATS, Mongo, and WebSocket coverage belongs in E2E tests.

// TestSDKRootSpanInjectsRandomnessTraceState verifies that the real SDK stores
// sampler-produced rv in the root SpanContext and TraceContext carrier.
func TestSDKRootSpanInjectsRandomnessTraceState(t *testing.T) {
	t.Parallel()

	randomness := uint64(0xf0000000000000)
	expectedRV := fmt.Sprintf("rv:%014x", randomness)
	tp, sr := newSDKTestProvider(
		t,
		otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySampler(0.5)),
		sdkTraceIDWithRandomness(randomness),
	)

	ctx, span := tp.Tracer("sdk-integration").Start(context.Background(), "A")
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid())
	require.True(t, sc.IsSampled())
	require.Contains(t, sc.TraceState().Get("ot"), expectedRV)

	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	require.NotEmpty(t, carrier.Get("traceparent"))
	require.Contains(t, carrier.Get("tracestate"), expectedRV)

	extracted := propagation.TraceContext{}.Extract(context.Background(), carrier)
	extractedSC := trace.SpanContextFromContext(extracted)
	require.True(t, extractedSC.IsRemote())
	assert.Contains(t, extractedSC.TraceState().Get("ot"), expectedRV)

	span.End()
	assert.Len(t, sr.Ended(), 1)
}

// TestSDKLinkedSpanUsesUpstreamRandomness verifies that a span started with one
// link samples from the upstream rv without inheriting the upstream trace ID.
func TestSDKLinkedSpanUsesUpstreamRandomness(t *testing.T) {
	t.Parallel()

	randomness := uint64(0xf0000000000000)
	expectedRV := fmt.Sprintf("rv:%014x", randomness)
	upstreamSC := sdkSpanContext(t, randomness, "0000000000000001", "ot="+expectedRV, true)
	tp, sr := newSDKTestProvider(
		t,
		otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySampler(0.5)),
		sdkTraceIDWithRandomness(0x00000000000000),
	)

	ctx, span := tp.Tracer("sdk-integration").Start(
		context.Background(),
		"B",
		trace.WithLinks(trace.Link{SpanContext: upstreamSC}),
	)
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid())
	require.True(t, sc.IsSampled())
	assert.Contains(t, sc.TraceState().Get("ot"), expectedRV)
	assert.NotEqual(t, upstreamSC.TraceID(), sc.TraceID(), "link seed must not change the span trace ID")

	span.End()
	assert.Len(t, sr.Ended(), 1)
}

// TestSDKDroppedLinkedSpanStillPropagatesRandomness verifies that a dropped SDK
// span still returns a non-recording SpanContext carrying the upstream rv.
func TestSDKDroppedLinkedSpanStillPropagatesRandomness(t *testing.T) {
	t.Parallel()

	randomness := uint64(0xd0000000000000)
	expectedRV := fmt.Sprintf("rv:%014x", randomness)
	upstreamSC := sdkSpanContext(t, randomness, "0000000000000001", "ot="+expectedRV, true)
	tp, sr := newSDKTestProvider(
		t,
		otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySampler(0.1)),
		sdkTraceIDWithRandomness(0xf0000000000000),
	)

	ctx, span := tp.Tracer("sdk-integration").Start(
		context.Background(),
		"B",
		trace.WithLinks(trace.Link{SpanContext: upstreamSC}),
	)
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid())
	require.False(t, sc.IsSampled())
	require.Contains(t, sc.TraceState().Get("ot"), expectedRV)

	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	require.NotEmpty(t, carrier.Get("traceparent"))
	require.Contains(t, carrier.Get("tracestate"), expectedRV)

	span.End()
	assert.Empty(t, sr.Ended())
}

// TestSDKParentChildContinuesRandomness verifies that parent-child spans keep
// using and propagating the same rv across different service probabilities.
func TestSDKParentChildContinuesRandomness(t *testing.T) {
	t.Parallel()

	randomness := uint64(0xd0000000000000)
	expectedRV := fmt.Sprintf("rv:%014x", randomness)
	upstreamSC := sdkSpanContext(t, randomness, "0000000000000001", "ot="+expectedRV, true)
	parentCtx := trace.ContextWithRemoteSpanContext(context.Background(), upstreamSC)

	eTP, eSR := newSDKTestProvider(
		t,
		otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySampler(0.2)),
	)
	eCtx, eSpan := eTP.Tracer("sdk-integration").Start(parentCtx, "E")
	eSC := trace.SpanContextFromContext(eCtx)
	require.True(t, eSC.IsValid())
	require.True(t, eSC.IsSampled())
	require.Contains(t, eSC.TraceState().Get("ot"), expectedRV)
	eSpan.End()
	require.Len(t, eSR.Ended(), 1)

	dTP, dSR := newSDKTestProvider(
		t,
		otelsampler.WithSingleLinkSeed(otelsampler.ProbabilitySampler(0.1)),
	)
	dCtx, dSpan := dTP.Tracer("sdk-integration").Start(
		trace.ContextWithRemoteSpanContext(context.Background(), eSC),
		"D",
	)
	dSC := trace.SpanContextFromContext(dCtx)
	require.True(t, dSC.IsValid())
	require.False(t, dSC.IsSampled())
	require.Contains(t, dSC.TraceState().Get("ot"), expectedRV)

	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(dCtx, carrier)
	require.Contains(t, carrier.Get("tracestate"), expectedRV)

	dSpan.End()
	assert.Empty(t, dSR.Ended())
}

func newSDKTestProvider(
	t *testing.T,
	sampler sdktrace.Sampler,
	traceIDs ...trace.TraceID,
) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(&deterministicIDGenerator{traceIDs: traceIDs}),
		sdktrace.WithSampler(sampler),
		sdktrace.WithSpanProcessor(sr),
	)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})
	return tp, sr
}

type deterministicIDGenerator struct {
	traceIDs  []trace.TraceID
	nextTrace uint64
	nextSpan  uint64
}

func (g *deterministicIDGenerator) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	traceID := sdkTraceIDWithRandomness(g.nextTrace + 1)
	if int(g.nextTrace) < len(g.traceIDs) {
		traceID = g.traceIDs[g.nextTrace]
	}
	g.nextTrace++
	return traceID, g.NewSpanID(context.Background(), traceID)
}

func (g *deterministicIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	g.nextSpan++
	var spanID trace.SpanID
	binary.BigEndian.PutUint64(spanID[:], g.nextSpan)
	return spanID
}

func sdkTraceIDWithRandomness(randomness uint64) trace.TraceID {
	var traceID trace.TraceID
	traceID[0] = 1
	binary.BigEndian.PutUint64(traceID[8:16], randomness&sdkRandomnessMask)
	return traceID
}

func sdkSpanContext(
	t *testing.T,
	randomness uint64,
	spanIDHex string,
	traceStateText string,
	sampled bool,
) trace.SpanContext {
	t.Helper()

	spanID, err := trace.SpanIDFromHex(spanIDHex)
	require.NoError(t, err)
	cfg := trace.SpanContextConfig{
		TraceID:    sdkTraceIDWithRandomness(randomness),
		SpanID:     spanID,
		TraceState: sdkTraceState(t, traceStateText),
		Remote:     true,
	}
	if sampled {
		cfg.TraceFlags = trace.FlagsSampled
	}
	return trace.NewSpanContext(cfg)
}

func sdkTraceState(t *testing.T, traceStateText string) trace.TraceState {
	t.Helper()

	state, err := trace.ParseTraceState(traceStateText)
	require.NoError(t, err)
	return state
}
