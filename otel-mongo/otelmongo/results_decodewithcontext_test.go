package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestBuildConsumerCtx_NewTraceIDAndLinksOriginTrace(t *testing.T) {
	enableTracing(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	tracer := tp.Tracer("test")
	originCtx, originSpan := tracer.Start(context.Background(), "origin")
	originSpanCtx := originSpan.SpanContext()
	originSpan.End()

	// Pass a context that still carries origin span context, and ensure the helper detaches it.
	newCtx, span := buildConsumerCtx(originCtx, tracer, nil, "", nil, "test-decode-span", nil, originSpanCtx)
	span.End()

	recovered := trace.SpanContextFromContext(newCtx)
	require.True(t, recovered.IsValid(), "expected returned context to contain a valid span context")
	assert.NotEqual(t, originSpanCtx.TraceID(), recovered.TraceID(), "expected new TraceID different from origin")

	// Validate that the span has a link-only association to the origin TraceID.
	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "test-decode-span" {
			continue
		}
		found = true
		links := s.Links()
		require.NotEmpty(t, links, "expected decode span to have at least 1 link")
		assert.Equal(t, originSpanCtx.TraceID(), links[0].SpanContext.TraceID(), "decode link TraceID mismatch")
		break
	}
	require.True(t, found, `expected a span named "test-decode-span"`)
}

func TestBuildConsumerCtx_WithDeliverTracer_ChildOfDeliver(t *testing.T) {
	enableTracing(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	deliverSR := tracetest.NewSpanRecorder()
	deliverTP := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(deliverSR),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = deliverTP.Shutdown(context.Background()) })
	deliverTracer := deliverTP.Tracer("deliver-test")

	tracer := tp.Tracer("test")
	_, originSpan := tracer.Start(context.Background(), "origin")
	originSpanCtx := originSpan.SpanContext()
	originSpan.End()

	newCtx, span := buildConsumerCtx(context.Background(), tracer, deliverTracer, "test deliver", nil, "test-consumer-span", nil, originSpanCtx)
	span.End()

	consumerSpanCtx := trace.SpanContextFromContext(newCtx)
	require.True(t, consumerSpanCtx.IsValid(), "expected returned context to contain a valid span context")
	assert.NotEqual(t, originSpanCtx.TraceID(), consumerSpanCtx.TraceID(), "expected new TraceID different from origin")

	// Deliver span should exist in deliverSR with a link to origin.
	var deliverFound bool
	for _, s := range deliverSR.Ended() {
		if s.Name() != "test deliver" {
			continue
		}
		deliverFound = true
		require.NotEmpty(t, s.Links(), "expected deliver span to have a link to origin")
		assert.Equal(t, originSpanCtx.TraceID(), s.Links()[0].SpanContext.TraceID(), "deliver span link should point to origin TraceID")
		// Consumer span should share deliver span's TraceID.
		assert.Equal(t, s.SpanContext().TraceID(), consumerSpanCtx.TraceID(), "consumer span TraceID should match deliver span TraceID")
		break
	}
	require.True(t, deliverFound, `expected a deliver span named "test deliver"`)
}
