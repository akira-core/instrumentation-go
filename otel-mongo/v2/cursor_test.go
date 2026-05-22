package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// stdProp is the standard W3C propagator used across cursor tests.
var stdProp = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

// buildDocWithTrace creates a raw BSON document that contains an _oteltrace field
// matching the span context in ctx.
func buildDocWithTrace(t *testing.T, ctx context.Context) bson.Raw { //nolint:revive // ctx is second parameter intentionally for test helpers
	t.Helper()
	enableTracing(t)
	doc := bson.D{{Key: "value", Value: "test"}}
	injected, err := injectTraceIntoDocument(ctx, doc, stdProp)
	require.NoError(t, err)
	raw, err := bson.Marshal(injected)
	require.NoError(t, err)
	return raw
}

func TestCursorDecodeWithContext_ExtractsTrace(t *testing.T) {
	otel.SetTextMapPropagator(stdProp)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, span := tracer.Start(context.Background(), "origin")
	originSpanCtx := span.SpanContext()
	span.End()

	rawDoc := buildDocWithTrace(t, ctx)

	cursor, err := mongo.NewCursorFromDocuments([]any{rawDoc}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()

	require.True(t, cursor.Next(context.Background()))

	c := &Cursor{
		Cursor: cursor,
		impl:   traced.NewCursor(cursor, tracer, stdProp, true),
	}

	var result bson.D
	_, err = c.DecodeWithContext(context.Background(), &result)
	require.NoError(t, err)

	ended := sr.Ended()
	var decodeSpan sdktrace.ReadOnlySpan
	for _, s := range ended {
		if s.Name() == "mongo.cursor.decode" {
			decodeSpan = s
			break
		}
	}
	require.NotNil(t, decodeSpan, "mongo.cursor.decode span should have been created")
	links := decodeSpan.Links()
	require.NotEmpty(t, links, "expected link to origin trace")
	assert.Equal(t, originSpanCtx.TraceID(), links[0].SpanContext.TraceID())
}

func TestCursorDecodeWithContext_NoTrace(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	raw, err := bson.Marshal(bson.D{{Key: "x", Value: 1}})
	require.NoError(t, err)

	cursor, err := mongo.NewCursorFromDocuments([]any{raw}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()

	require.True(t, cursor.Next(context.Background()))

	baseCtx := context.Background()
	c := &Cursor{
		Cursor: cursor,
		impl:   traced.NewCursor(cursor, tracer, stdProp, false),
	}

	var result bson.D
	_, err = c.DecodeWithContext(baseCtx, &result)
	require.NoError(t, err)

	ended := sr.Ended()
	var decodeSpan sdktrace.ReadOnlySpan
	for _, s := range ended {
		if s.Name() == "mongo.cursor.decode" {
			decodeSpan = s
			break
		}
	}
	require.NotNil(t, decodeSpan, "mongo.cursor.decode span should have been created")
	assert.Empty(t, decodeSpan.Links(), "no links expected when document has no trace")
}

func TestCursorDecodeWithContext_NoFlagsNoSpan(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "false")
	t.Setenv(envMongoPropagationEnabled, "true")
	otel.SetTextMapPropagator(stdProp)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, originSpan := tp.Tracer("test").Start(context.Background(), "origin")
	originSpan.End()
	rawDoc := buildDocWithTrace(t, ctx)

	cursor, err := mongo.NewCursorFromDocuments([]any{rawDoc}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()
	require.True(t, cursor.Next(context.Background()))

	// Disabled path: use direct.NewCursor — passthrough impl with no OTel SDK reachable.
	_ = noop.NewTracerProvider()
	c := &Cursor{
		Cursor: cursor,
		impl:   direct.NewCursor(cursor),
	}

	var result bson.D
	enrichedCtx, err := c.DecodeWithContext(context.Background(), &result)
	require.NoError(t, err)
	assert.False(t, trace.SpanContextFromContext(enrichedCtx).IsValid(), "expected passthrough to avoid creating span context")

	for _, s := range sr.Ended() {
		assert.NotEqual(t, "mongo.cursor.decode", s.Name(), "no decode span should be recorded when flags are disabled")
	}
}

func TestSingleResultDecodeLinksOriginTrace(t *testing.T) {
	otel.SetTextMapPropagator(stdProp)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpan.End()

	rawDoc := buildDocWithTrace(t, ctx)

	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdProp, true),
	}

	var out bson.D
	err := wrapped.Decode(&out)
	require.NoError(t, err)

	ended := sr.Ended()
	require.NotEmpty(t, ended)

	var findoneSpan sdktrace.ReadOnlySpan
	for _, s := range ended {
		if s.Name() == "findone" {
			findoneSpan = s
			break
		}
	}
	require.NotNil(t, findoneSpan, "findone span should have ended")

	links := findoneSpan.Links()
	require.NotEmpty(t, links, "expected link to origin trace")
	assert.Equal(t, originSpan.SpanContext().TraceID(), links[0].SpanContext.TraceID())
}

func TestSingleResultTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(stdProp)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpanCtx := originSpan.SpanContext()
	originSpan.End()

	rawDoc := buildDocWithTrace(t, ctx)

	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone2")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdProp, true),
	}

	enriched := wrapped.TraceContext()
	recovered := trace.SpanContextFromContext(enriched)
	assert.True(t, recovered.IsValid())
	assert.Equal(t, originSpanCtx.TraceID(), recovered.TraceID())
}

func TestCursorDecode(t *testing.T) {
	raw, err := bson.Marshal(bson.D{{Key: "field", Value: "v"}})
	require.NoError(t, err)

	cursor, err := mongo.NewCursorFromDocuments([]any{raw}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()

	require.True(t, cursor.Next(context.Background()))

	c := &Cursor{Cursor: cursor, impl: direct.NewCursor(cursor)}

	var result bson.D
	err = c.Decode(&result)
	require.NoError(t, err)
	assert.NotEmpty(t, result)
}

func TestSingleResultRaw(t *testing.T) {
	otel.SetTextMapPropagator(stdProp)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpan.End()

	rawDoc := buildDocWithTrace(t, ctx)
	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone-raw")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdProp, false),
	}

	raw, err := wrapped.Raw()
	require.NoError(t, err)
	assert.NotEmpty(t, raw)

	ended := sr.Ended()
	var found bool
	for _, s := range ended {
		if s.Name() == "findone-raw" {
			found = true
			break
		}
	}
	assert.True(t, found, "findone-raw span should be ended after Raw()")
}

func TestSingleResultDecodeSpanEndedOnce(t *testing.T) {
	otel.SetTextMapPropagator(stdProp)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpan.End()

	rawDoc := buildDocWithTrace(t, ctx)
	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone-once")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdProp, false),
	}

	var out bson.D
	_ = wrapped.Decode(&out)
	_ = wrapped.Decode(&out)

	var count int
	for _, s := range sr.Ended() {
		if s.Name() == "findone-once" {
			count++
		}
	}
	assert.Equal(t, 1, count, "span must be ended exactly once")
}

// TestCursorDecode_DirectPathEmitsNoSpanV2 mirrors the v1 sibling test.
// Asserts the passthrough Cursor.Decode does not record any span on the
// caller's recorder when constructed via the disabled-mode impl. Together
// with the existing TestCursorDecodeWithContext_NoFlagsNoSpan this locks in
// the byte-identical-with-native-driver invariant for v2.
func TestCursorDecode_DirectPathEmitsNoSpanV2(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	raw, err := bson.Marshal(bson.D{{Key: "field", Value: "v"}})
	require.NoError(t, err)
	cursor, err := mongo.NewCursorFromDocuments([]any{raw}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()
	require.True(t, cursor.Next(context.Background()))

	c := &Cursor{Cursor: cursor, impl: direct.NewCursor(cursor)}
	var out bson.D
	require.NoError(t, c.Decode(&out))

	assert.Empty(t, sr.Ended(), "passthrough Cursor.Decode must emit zero spans")
}

// TestSingleResultDirectPath_TraceContextUnchangedV2 mirrors the v1 sibling
// test. The disabled-path SingleResult must return the parent context
// unchanged (no propagation, no span).
func TestSingleResultDirectPath_TraceContextUnchangedV2(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	raw, err := bson.Marshal(bson.D{{Key: "value", Value: "test"}})
	require.NoError(t, err)
	mongoSR := mongo.NewSingleResultFromDocument(raw, nil, nil)

	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "sentinel")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         direct.NewSingleResult(mongoSR, parent),
	}

	got := wrapped.TraceContext()
	assert.Equal(t, "sentinel", got.Value(key{}), "passthrough must return parent ctx unchanged")
	assert.False(t, trace.SpanFromContext(got).SpanContext().IsValid(), "passthrough must not embed a span context")
	assert.Empty(t, sr.Ended(), "passthrough must emit zero spans")
}
