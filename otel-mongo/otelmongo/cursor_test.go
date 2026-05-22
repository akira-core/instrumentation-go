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

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// stdPropV1 is the standard W3C propagator used across cursor / single-result
// tests. Mirrors v2/cursor_test.go::stdProp so future parity audits can grep
// either name and find the same value.
var stdPropV1 = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

// buildDocWithTraceV1 creates a raw BSON document that carries an _oteltrace
// field matching the span context in ctx. Mirrors v2/cursor_test.go.
func buildDocWithTraceV1(t *testing.T, ctx context.Context) bson.Raw { //nolint:revive // ctx as second param matches helper convention
	t.Helper()
	enableTracing(t)
	doc := bson.D{{Key: "value", Value: "test"}}
	injected, err := injectTraceIntoDocument(ctx, doc, stdPropV1)
	require.NoError(t, err)
	raw, err := bson.Marshal(injected)
	require.NoError(t, err)
	return raw
}

// TestCursorDecode covers the bare passthrough Decode method on a direct.Cursor.
// Parity with v2/cursor_test.go::TestCursorDecode.
func TestCursorDecode(t *testing.T) {
	raw, err := bson.Marshal(bson.D{{Key: "field", Value: "v"}})
	require.NoError(t, err)

	cursor, err := mongo.NewCursorFromDocuments([]interface{}{raw}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()

	require.True(t, cursor.Next(context.Background()))

	c := &Cursor{Cursor: cursor, impl: direct.NewCursor(cursor)}

	var result bson.D
	require.NoError(t, c.Decode(&result))
	assert.NotEmpty(t, result)
}

// TestCursorDecode_DirectPathEmitsNoSpan asserts the passthrough Cursor.Decode
// does not record any span on the caller's recorder when constructed via the
// disabled-mode impl. Together with TestCursorDecodeWithContext_NoFlagsNoSpan
// this locks in the byte-identical-with-native-driver invariant for v1.
func TestCursorDecode_DirectPathEmitsNoSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	raw, err := bson.Marshal(bson.D{{Key: "field", Value: "v"}})
	require.NoError(t, err)
	cursor, err := mongo.NewCursorFromDocuments([]interface{}{raw}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = cursor.Close(context.Background()) }()
	require.True(t, cursor.Next(context.Background()))

	c := &Cursor{Cursor: cursor, impl: direct.NewCursor(cursor)}
	var out bson.D
	require.NoError(t, c.Decode(&out))

	assert.Empty(t, sr.Ended(), "passthrough Cursor.Decode must emit zero spans")
}

// TestSingleResultDecodeLinksOriginTrace covers the v1-specific behaviour:
// the FindOne span gets a link to the origin trace stored in _oteltrace, and
// the span is ended exactly once on first decode. Parity with
// v2/cursor_test.go::TestSingleResultDecodeLinksOriginTrace.
func TestSingleResultDecodeLinksOriginTrace(t *testing.T) {
	otel.SetTextMapPropagator(stdPropV1)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpan.End()

	rawDoc := buildDocWithTraceV1(t, ctx)

	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdPropV1, true),
	}

	var out bson.D
	require.NoError(t, wrapped.Decode(&out))

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

// TestSingleResultTraceContext covers TraceContext() returning the producer
// trace context. Parity with v2/cursor_test.go::TestSingleResultTraceContext.
func TestSingleResultTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(stdPropV1)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpanCtx := originSpan.SpanContext()
	originSpan.End()

	rawDoc := buildDocWithTraceV1(t, ctx)

	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone2")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdPropV1, true),
	}

	enriched := wrapped.TraceContext()
	recovered := trace.SpanContextFromContext(enriched)
	assert.True(t, recovered.IsValid())
	assert.Equal(t, originSpanCtx.TraceID(), recovered.TraceID())
}

// TestSingleResultRaw covers Raw() ending the FindOne span. Parity with
// v2/cursor_test.go::TestSingleResultRaw.
func TestSingleResultRaw(t *testing.T) {
	otel.SetTextMapPropagator(stdPropV1)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	ctx, originSpan := tracer.Start(context.Background(), "origin")
	originSpan.End()

	rawDoc := buildDocWithTraceV1(t, ctx)
	mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
	_, findSpan := tracer.Start(context.Background(), "findone-raw")

	wrapped := &SingleResult{
		SingleResult: mongoSR,
		impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdPropV1, false),
	}

	raw, err := wrapped.Raw()
	require.NoError(t, err)
	assert.NotEmpty(t, raw)

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() == "findone-raw" {
			found = true
			break
		}
	}
	assert.True(t, found, "findone-raw span should be ended after Raw()")
}

// TestSingleResultDecodeSpanEndedOnce locks in the "first-touch ends span"
// invariant: span must end exactly once across any sequence of Decode / Raw /
// TraceContext, in any order, on the same SingleResult. Mirrors v2.
//
// Permutations exercised: Decode×2 (idempotence), Decode→Raw, Raw→Decode,
// Decode→TraceContext, TraceContext→Raw. Each subtest asserts exactly 1
// span ended.
func TestSingleResultDecodeSpanEndedOnce(t *testing.T) {
	otel.SetTextMapPropagator(stdPropV1)

	cases := []struct {
		name string
		ops  []string // each entry is "Decode" | "Raw" | "TraceContext"
	}{
		{"decode_twice", []string{"Decode", "Decode"}},
		{"decode_then_raw", []string{"Decode", "Raw"}},
		{"raw_then_decode", []string{"Raw", "Decode"}},
		{"decode_then_tracectx", []string{"Decode", "TraceContext"}},
		{"tracectx_then_raw", []string{"TraceContext", "Raw"}},
		{"all_three", []string{"Decode", "Raw", "TraceContext"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
			otel.SetTracerProvider(tp)
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			tracer := tp.Tracer("test")

			ctx, originSpan := tracer.Start(context.Background(), "origin")
			originSpan.End()

			rawDoc := buildDocWithTraceV1(t, ctx)
			mongoSR := mongo.NewSingleResultFromDocument(rawDoc, nil, nil)
			spanName := "findone-" + tc.name
			_, findSpan := tracer.Start(context.Background(), spanName)

			wrapped := &SingleResult{
				SingleResult: mongoSR,
				impl:         traced.NewSingleResult(mongoSR, findSpan, ctx, stdPropV1, false),
			}

			for _, op := range tc.ops {
				switch op {
				case "Decode":
					var out bson.D
					_ = wrapped.Decode(&out)
				case "Raw":
					_, _ = wrapped.Raw()
				case "TraceContext":
					_ = wrapped.TraceContext()
				default:
					t.Fatalf("unknown op %q", op)
				}
			}

			var count int
			for _, s := range sr.Ended() {
				if s.Name() == spanName {
					count++
				}
			}
			assert.Equal(t, 1, count,
				"span must end exactly once across the sequence %v", tc.ops)
		})
	}
}

// TestSingleResultDirectPath_TraceContextUnchanged asserts the disabled-path
// SingleResult returns the parent context unchanged (no propagation, no span).
func TestSingleResultDirectPath_TraceContextUnchanged(t *testing.T) {
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
