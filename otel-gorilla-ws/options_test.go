package otelgorillaws

import (
	"context"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-gorilla-ws/internal/traced"
)

func newRecorderTP(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, recorder
}

func TestResolveOptions_GlobalFallback(t *testing.T) {
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	tracer, prop := resolveOptions(nil)

	_, span := tracer.Start(context.Background(), "global-fallback")
	span.End()

	require.Len(t, globalRecorder.Ended(), 1)
	assert.Equal(t, "global-fallback", globalRecorder.Ended()[0].Name())
	assert.Equal(t, propagation.TraceContext{}.Fields(), prop.Fields())
}

func TestResolveOptions_UsesProvidedOptionsWithoutMutatingGlobals(t *testing.T) {
	globalTP, globalRecorder := newRecorderTP(t)
	otel.SetTracerProvider(globalTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	customTP, customRecorder := newRecorderTP(t)
	customProp := propagation.NewCompositeTextMapPropagator(propagation.Baggage{})

	tracer, prop := resolveOptions([]Option{
		WithTracerProvider(customTP),
		WithPropagators(customProp),
	})

	_, span := tracer.Start(context.Background(), "custom-provider")
	span.End()

	require.Len(t, customRecorder.Ended(), 1)
	assert.Equal(t, "custom-provider", customRecorder.Ended()[0].Name())
	assert.Empty(t, globalRecorder.Ended(), "custom option should not write to global recorder")
	assert.Equal(t, customProp.Fields(), prop.Fields())
	assert.Equal(t, propagation.TraceContext{}.Fields(), otel.GetTextMapPropagator().Fields(),
		"global propagator must remain unchanged")
}

func TestNewConn_FeatureDisabled_PicksDirectImpl(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "0")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "0")
	wsGate.ResetForTest()
	t.Cleanup(wsGate.ResetForTest)

	c := newConn(&websocket.Conn{}, true)
	_, ok := c.impl.(*direct.Conn)
	assert.True(t, ok, "env-off must select internal/direct.Conn; got %T", c.impl)
}

func TestNewConn_FeatureEnabled_PicksTracedImpl(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	wsGate.ResetForTest()
	t.Cleanup(wsGate.ResetForTest)

	c := newConn(&websocket.Conn{}, true)
	tc, ok := c.impl.(*traced.Conn)
	require.True(t, ok, "env-on must select internal/traced.Conn; got %T", c.impl)
	assert.True(t, tc.PropagationEnabled, "negotiated=true must propagate to traced.Conn.PropagationEnabled")
}

func TestNewConn_FeatureOn_NegotiatedOff_PropagationDisabled(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	wsGate.ResetForTest()
	t.Cleanup(wsGate.ResetForTest)

	c := newConn(&websocket.Conn{}, false)
	tc, ok := c.impl.(*traced.Conn)
	require.True(t, ok, "env-on must select internal/traced.Conn; got %T", c.impl)
	assert.False(t, tc.PropagationEnabled,
		"negotiated=false must leave traced.Conn.PropagationEnabled=false (spans-only mode)")
}
