package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

func TestCursorDecodeAndTrace_NewTraceIDAndLinksOriginTrace(t *testing.T) {
	enableTracing(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

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

	prop := otel.GetTextMapPropagator()

	// Build a cursor document that contains the origin trace metadata.
	injected, err := injectTraceIntoDocument(originCtx, bson.D{{Key: "field", Value: "v"}}, prop)
	require.NoError(t, err, "injectTraceIntoDocument")
	rawDoc, err := bson.Marshal(injected)
	require.NoError(t, err, "bson.Marshal injected doc")

	cur, err := mongo.NewCursorFromDocuments([]interface{}{rawDoc}, nil, nil)
	require.NoError(t, err, "NewCursorFromDocuments")
	defer func() { _ = cur.Close(context.Background()) }()
	require.True(t, cur.Next(context.Background()), "expected cursor.Next=true")

	wrapped := &Cursor{
		Cursor: cur,
		impl:   traced.NewCursor(cur, tracer, prop, true),
	}

	var out bson.D
	enrichedCtx, err := wrapped.DecodeAndTrace(context.Background(), &out)
	require.NoError(t, err, "DecodeAndTrace")

	recovered := trace.SpanContextFromContext(enrichedCtx)
	require.True(t, recovered.IsValid(), "expected returned context to contain a valid span context")
	assert.NotEqual(t, originSpanCtx.TraceID(), recovered.TraceID(), "expected new TraceID different from origin")

	// Validate that the internal decode span has a link to the origin TraceID.
	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "mongo.cursor.decode" {
			continue
		}
		found = true
		links := s.Links()
		require.NotEmpty(t, links, "expected decode span to have at least 1 link")
		assert.Equal(t, originSpanCtx.TraceID(), links[0].SpanContext.TraceID(), "decode link TraceID mismatch")
		break
	}
	require.True(t, found, "expected a span named %q", "mongo.cursor.decode")
}

func TestCursorDecodeAndTrace_NoTrace(t *testing.T) {
	enableTracing(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	tracer := tp.Tracer("test")
	prop := otel.GetTextMapPropagator()

	// Document without _oteltrace metadata: tracing is on, so a decode span is
	// still emitted, but with no link (nothing to link to). Parity with v2.
	rawDoc, err := bson.Marshal(bson.D{{Key: "field", Value: "v"}})
	require.NoError(t, err, "bson.Marshal plain doc")

	cur, err := mongo.NewCursorFromDocuments([]interface{}{rawDoc}, nil, nil)
	require.NoError(t, err, "NewCursorFromDocuments")
	defer func() { _ = cur.Close(context.Background()) }()
	require.True(t, cur.Next(context.Background()), "expected cursor.Next=true")

	wrapped := &Cursor{
		Cursor: cur,
		impl:   traced.NewCursor(cur, tracer, prop, true),
	}

	var out bson.D
	_, err = wrapped.DecodeAndTrace(context.Background(), &out)
	require.NoError(t, err, "DecodeAndTrace")

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "mongo.cursor.decode" {
			continue
		}
		found = true
		assert.Empty(t, s.Links(), "no link expected when document has no _oteltrace")
		break
	}
	require.True(t, found, "expected a span named %q", "mongo.cursor.decode")
}

func TestCursorDecodeAndTrace_NoFlagsNoSpan(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "false")
	t.Setenv(envMongoPropagationEnabled, "true")
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	originCtx, originSpan := tp.Tracer("test").Start(context.Background(), "origin")
	originSpan.End()
	prop := otel.GetTextMapPropagator()
	injected, err := injectTraceIntoDocument(originCtx, bson.D{{Key: "field", Value: "v"}}, prop)
	require.NoError(t, err)
	rawDoc, err := bson.Marshal(injected)
	require.NoError(t, err)

	cur, err := mongo.NewCursorFromDocuments([]interface{}{rawDoc}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cur.Close(context.Background()) }()
	require.True(t, cur.Next(context.Background()))

	// Disabled path: direct.NewCursor is the passthrough impl. noop tracer and
	// propagator stay unused but kept for parity with the prior test setup.
	_ = noop.NewTracerProvider()
	_ = prop
	wrapped := &Cursor{
		Cursor: cur,
		impl:   direct.NewCursor(cur),
	}

	var out bson.D
	enrichedCtx, err := wrapped.DecodeAndTrace(context.Background(), &out)
	require.NoError(t, err)
	assert.False(t, trace.SpanContextFromContext(enrichedCtx).IsValid(), "expected passthrough path to avoid creating span context")

	for _, s := range sr.Ended() {
		assert.NotEqual(t, "mongo.cursor.decode", s.Name(), "no decode span should be recorded when flags are disabled")
	}
}
