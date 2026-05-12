package otelgorillaws

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRecorderTP(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, recorder
}

func TestApplyOptions_GlobalFallback(t *testing.T) {
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	c := &Conn{featureEnabled: true}
	applyOptions(c, nil)

	_, span := c.tracer.Start(context.Background(), "global-fallback")
	span.End()

	require.Len(t, globalRecorder.Ended(), 1)
	assert.Equal(t, "global-fallback", globalRecorder.Ended()[0].Name())
	assert.Equal(t, propagation.TraceContext{}.Fields(), c.propagator.Fields())
}

func TestApplyOptions_FeatureDisabled_UsesNoopTracer(t *testing.T) {
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	c := &Conn{featureEnabled: false}
	applyOptions(c, nil)

	_, span := c.tracer.Start(context.Background(), "should-not-be-recorded")
	span.End()

	assert.Empty(t, globalRecorder.Ended(), "no spans should be recorded on caller's TracerProvider when feature flag is off")
}

func TestApplyOptions_UsesProvidedOptionsWithoutMutatingGlobals(t *testing.T) {
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	customTP, customRecorder := newRecorderTP(t)
	customProp := propagation.NewCompositeTextMapPropagator(propagation.Baggage{})

	c := &Conn{featureEnabled: true}
	applyOptions(c, []Option{
		WithTracerProvider(customTP),
		WithPropagators(customProp),
	})

	_, span := c.tracer.Start(context.Background(), "custom-provider")
	span.End()

	require.Len(t, customRecorder.Ended(), 1)
	assert.Equal(t, "custom-provider", customRecorder.Ended()[0].Name())
	assert.Empty(t, globalRecorder.Ended(), "custom option should not write to global recorder")
	assert.Equal(t, customProp.Fields(), c.propagator.Fields())
	assert.Equal(t, propagation.TraceContext{}.Fields(), otel.GetTextMapPropagator().Fields(), "global propagator must remain unchanged")
}
