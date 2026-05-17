package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func init() {
	// So that injectTraceIntoDocument and contextFromTraceMetadata use a working propagator in tests (otel globals).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
}

func enableTracing(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)
}

// enableDocumentPropagation sets the same env gates as Collection / ContextFrom* for _oteltrace.
// Propagation requires both the global and module tracing flags to be on.
func enableDocumentPropagation(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)
}

func Test_extractMetadataFromRaw(t *testing.T) {
	enableTracing(t)
	t.Run("valid_document_returns_metadata", func(t *testing.T) {
		doc := bson.D{
			{Key: "name", Value: "foo"},
			{Key: TraceMetadataKey, Value: bson.D{
				{Key: "traceparent", Value: "00-trace123456789012345678901234-0123456789012345-01"},
				{Key: "tracestate", Value: "k=v"},
			}},
		}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)

		meta, ok := extractMetadataFromRaw(raw)
		require.True(t, ok)
		require.NotNil(t, meta)
		assert.Equal(t, "00-trace123456789012345678901234-0123456789012345-01", meta.Traceparent)
		assert.Equal(t, "k=v", meta.Tracestate)
	})

	t.Run("missing_oteltrace_returns_false", func(t *testing.T) {
		doc := bson.D{{Key: "name", Value: "foo"}}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)

		meta, ok := extractMetadataFromRaw(raw)
		assert.False(t, ok)
		assert.Nil(t, meta)
	})

	t.Run("oteltrace_not_document_returns_false", func(t *testing.T) {
		doc := bson.D{{Key: TraceMetadataKey, Value: "not-a-doc"}}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)

		meta, ok := extractMetadataFromRaw(raw)
		assert.False(t, ok)
		assert.Nil(t, meta)
	})

	t.Run("empty_traceparent_returns_false", func(t *testing.T) {
		doc := bson.D{
			{Key: TraceMetadataKey, Value: bson.D{
				{Key: "traceparent", Value: ""},
				{Key: "tracestate", Value: ""},
			}},
		}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)

		meta, ok := extractMetadataFromRaw(raw)
		assert.False(t, ok)
		assert.Nil(t, meta)
	})
}

func Test_injectTraceIntoDocument(t *testing.T) {
	enableTracing(t)
	t.Run("context_without_valid_span_returns_doc_unchanged", func(t *testing.T) {
		ctx := context.Background()
		doc := bson.D{{Key: "x", Value: 1}}

		out, err := injectTraceIntoDocument(ctx, doc, otel.GetTextMapPropagator())
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "x", out[0].Key)
		// No _oteltrace field
		var hasOtel bool
		for _, e := range out {
			if e.Key == TraceMetadataKey {
				hasOtel = true
				break
			}
		}
		assert.False(t, hasOtel)
	})

	t.Run("context_with_valid_span_injects_oteltrace", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		ctx, span := tp.Tracer("test").Start(context.Background(), "op")
		defer span.End()

		doc := bson.D{{Key: "x", Value: 1}}
		out, err := injectTraceIntoDocument(ctx, doc, otel.GetTextMapPropagator())
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(out), 2)
		var meta TraceMetadata
		for _, e := range out {
			if e.Key == TraceMetadataKey {
				metaVal, ok := e.Value.(TraceMetadata)
				require.True(t, ok)
				meta = metaVal
				break
			}
		}
		assert.NotEmpty(t, meta.Traceparent)
		assert.Equal(t, span.SpanContext().TraceID().String(), meta.Traceparent[3:3+32]) // traceparent format: version-traceid-spanid-flags
	})

	t.Run("non_marshalable_document_returns_error", func(t *testing.T) {
		ctx := context.Background()
		ch := make(chan int)

		_, err := injectTraceIntoDocument(ctx, ch, otel.GetTextMapPropagator())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "marshal")
	})
}

func Test_ContextFromRawDocument(t *testing.T) {
	enableDocumentPropagation(t)
	t.Run("raw_without_oteltrace_returns_ctx_unchanged", func(t *testing.T) {
		doc := bson.D{{Key: "a", Value: 1}}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)
		ctx := context.Background()

		out := ContextFromRawDocument(ctx, raw)
		sc := trace.SpanFromContext(out).SpanContext()
		assert.False(t, sc.IsValid(), "context should not have valid span when document has no _oteltrace")
	})

	t.Run("raw_with_valid_oteltrace_returns_enriched_context", func(t *testing.T) {
		// W3C traceparent: version (2 hex) - trace-id (32 hex) - span-id (16 hex) - flags (2 hex)
		traceparent := "00-12345678901234567890123456789012-0123456789012345-01"
		doc := bson.D{
			{Key: TraceMetadataKey, Value: bson.D{
				{Key: "traceparent", Value: traceparent},
				{Key: "tracestate", Value: ""},
			}},
		}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)
		ctx := context.Background()

		out := ContextFromRawDocument(ctx, raw)
		sc := trace.SpanFromContext(out).SpanContext()
		assert.True(t, sc.IsValid())
		assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
	})

	t.Run("propagation_disabled_returns_ctx_unchanged_even_with_oteltrace", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "false")
		resetPropEnabledCacheForTest()
		t.Cleanup(resetPropEnabledCacheForTest)
		traceparent := "00-12345678901234567890123456789012-0123456789012345-01"
		doc := bson.D{
			{Key: TraceMetadataKey, Value: bson.D{
				{Key: "traceparent", Value: traceparent},
			}},
		}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)
		ctx := context.Background()
		out := ContextFromRawDocument(ctx, raw)
		assert.Equal(t, ctx, out)
		assert.False(t, trace.SpanFromContext(out).SpanContext().IsValid())
	})
}

func Test_ContextFromDocument(t *testing.T) {
	enableDocumentPropagation(t)
	t.Run("full_document_with_trace_metadata_returns_valid_span_context", func(t *testing.T) {
		fullDoc := bson.M{
			"_oteltrace": bson.M{
				"traceparent": "00-12345678901234567890123456789012-0123456789012345-01",
			},
		}
		sc, ok := ContextFromDocument(context.Background(), fullDoc)
		require.True(t, ok)
		assert.True(t, sc.IsValid())
		assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
	})

	t.Run("non_marshalable_document_returns_false", func(t *testing.T) {
		ch := make(chan int)
		sc, ok := ContextFromDocument(context.Background(), ch)
		require.False(t, ok)
		assert.False(t, sc.IsValid())
	})
	t.Run("missing_metadata_returns_false", func(t *testing.T) {
		sc, ok := ContextFromDocument(context.Background(), bson.M{"x": 1})
		require.False(t, ok)
		assert.False(t, sc.IsValid())
	})

	t.Run("bson_D_input_returns_valid_span_context", func(t *testing.T) {
		fullDoc := bson.D{
			{Key: "msg", Value: "extract-test"},
			{Key: "_oteltrace", Value: bson.D{
				{Key: "traceparent", Value: "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"},
			}},
		}
		sc, ok := ContextFromDocument(context.Background(), fullDoc)
		require.True(t, ok)
		assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", sc.TraceID().String())
		assert.Equal(t, "bbbbbbbbbbbbbbbb", sc.SpanID().String())
		assert.True(t, sc.IsSampled(), "trace flags 01 should set sampled bit")
	})

	t.Run("propagation_disabled_returns_false_despite_metadata", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "false")
		resetPropEnabledCacheForTest()
		t.Cleanup(resetPropEnabledCacheForTest)
		fullDoc := bson.M{
			"_oteltrace": bson.M{
				"traceparent": "00-12345678901234567890123456789012-0123456789012345-01",
			},
		}
		sc, ok := ContextFromDocument(context.Background(), fullDoc)
		require.False(t, ok)
		assert.False(t, sc.IsValid())
	})
}

func Test_ContextFromRawDocument_Exported(t *testing.T) {
	enableDocumentPropagation(t)
	traceparent := "00-12345678901234567890123456789012-0123456789012345-01"
	doc := bson.D{
		{Key: TraceMetadataKey, Value: bson.D{
			{Key: "traceparent", Value: traceparent},
		}},
	}
	raw, err := bson.Marshal(doc)
	require.NoError(t, err)
	out := ContextFromRawDocument(context.Background(), raw)
	sc := trace.SpanFromContext(out).SpanContext()
	assert.True(t, sc.IsValid())
	assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
}

func Test_injectTraceIntoUpdate(t *testing.T) {
	enableTracing(t)
	t.Run("context_without_valid_span_returns_update_unchanged", func(t *testing.T) {
		ctx := context.Background()
		update := bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: 1}}}}

		out, err := injectTraceIntoUpdate(ctx, update, otel.GetTextMapPropagator())
		require.NoError(t, err)
		assert.Equal(t, update, out)
	})

	t.Run("operator_update_injects_into_set", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		ctx, span := tp.Tracer("test").Start(context.Background(), "op")
		defer span.End()

		update := bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "ok"}}}}
		out, err := injectTraceIntoUpdate(ctx, update, otel.GetTextMapPropagator())
		require.NoError(t, err)
		outD, ok := out.(bson.D)
		require.True(t, ok)
		var setVal bson.D
		for _, e := range outD {
			if e.Key == "$set" {
				setVal, _ = e.Value.(bson.D)
				break
			}
		}
		require.NotEmpty(t, setVal)
		var found bool
		for _, e := range setVal {
			if e.Key == TraceMetadataKey {
				found = true
				break
			}
		}
		assert.True(t, found, "$set should contain _oteltrace")
	})

	t.Run("replacement_document_appends_oteltrace", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		ctx, span := tp.Tracer("test").Start(context.Background(), "op")
		_ = span
		defer span.End()

		replacement := bson.D{{Key: "name", Value: "foo"}}
		out, err := injectTraceIntoUpdate(ctx, replacement, otel.GetTextMapPropagator())
		require.NoError(t, err)
		outD, ok := out.(bson.D)
		require.True(t, ok)
		require.GreaterOrEqual(t, len(outD), 2)
		var found bool
		for _, e := range outD {
			if e.Key == TraceMetadataKey {
				found = true
				break
			}
		}
		assert.True(t, found)
	})
}

func Test_injectTraceIntoUpdate_DotNotationPreserved(t *testing.T) {
	enableTracing(t)
	// mongo.M{"u._id": "v"} marshals to BSON with a literal field name "u._id" (a string
	// containing a dot character). bson.Unmarshal into bson.D must return that same literal
	// key — it must NOT expand it to a nested {"u": {"_id": "v"}} document.
	// This test guards against a regression where the marshal/unmarshal round-trip inside
	// injectTraceIntoUpdate would silently rewrite or drop dot-containing field names.
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	update := bson.D{{Key: "$setOnInsert", Value: bson.D{
		{Key: "u._id", Value: "123"},
		{Key: "p._id", Value: "444"},
	}}}

	out, err := injectTraceIntoUpdate(ctx, update, otel.GetTextMapPropagator())
	require.NoError(t, err)
	outD, ok := out.(bson.D)
	require.True(t, ok)

	uDotIDFound := false
	pDotIDFound := false
	for _, e := range outD {
		if e.Key == "$setOnInsert" {
			subDoc, ok := e.Value.(bson.D)
			require.True(t, ok)
			for _, s := range subDoc {
				if s.Key == "u._id" {
					uDotIDFound = true
				}
				if s.Key == "p._id" {
					pDotIDFound = true
				}
			}
		}
	}
	assert.True(t, uDotIDFound, "literal dot-notation key 'u._id' must survive the bson.Marshal/Unmarshal round-trip unchanged")
	assert.True(t, pDotIDFound, "literal dot-notation key 'p._id' must survive the bson.Marshal/Unmarshal round-trip unchanged")
}

// Test_upsertSetField moved to internal/shared/upsertset_test.go since the
// helper is package-private there.
