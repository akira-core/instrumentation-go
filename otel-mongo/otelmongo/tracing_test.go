package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func init() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
}

func enableTracing(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoTracingEnabled, "1")
}

// enableDocumentPropagation sets the same env gates as Collection / ContextFrom* for _oteltrace.
func enableDocumentPropagation(t *testing.T) {
	t.Helper()
	t.Setenv(envGlobalTracingEnabled, "1")
	t.Setenv(envMongoPropagationEnabled, "1")
}

func TestContextFromDocumentV1(t *testing.T) {
	enableDocumentPropagation(t)
	t.Run("full_document_with_trace_metadata_returns_valid_span_context", func(t *testing.T) {
		fullDoc := bson.M{
			"_oteltrace": bson.M{
				"traceparent": "00-12345678901234567890123456789012-0123456789012345-01",
			},
		}
		sc, ok := ContextFromDocument(context.Background(), fullDoc)
		require.True(t, ok)
		require.True(t, sc.IsValid())
		assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
	})

	t.Run("missing_metadata_returns_false", func(t *testing.T) {
		sc, ok := ContextFromDocument(context.Background(), bson.M{"x": 1})
		assert.False(t, ok)
		assert.False(t, sc.IsValid())
	})

	t.Run("non_marshalable_document_returns_false", func(t *testing.T) {
		ch := make(chan int)
		sc, ok := ContextFromDocument(context.Background(), ch)
		assert.False(t, ok)
		assert.False(t, sc.IsValid())
	})

	t.Run("empty_traceparent_returns_false", func(t *testing.T) {
		fullDoc := bson.M{
			"_oteltrace": bson.M{
				"traceparent": "",
			},
		}
		sc, ok := ContextFromDocument(context.Background(), fullDoc)
		assert.False(t, ok)
		assert.False(t, sc.IsValid())
	})

	t.Run("propagation_disabled_returns_false_despite_metadata", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "false")
		fullDoc := bson.M{
			"_oteltrace": bson.M{
				"traceparent": "00-12345678901234567890123456789012-0123456789012345-01",
			},
		}
		sc, ok := ContextFromDocument(context.Background(), fullDoc)
		assert.False(t, ok)
		assert.False(t, sc.IsValid())
	})
}

func TestContextFromRawDocumentV1(t *testing.T) {
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
	require.True(t, sc.IsValid())
	assert.Equal(t, "12345678901234567890123456789012", sc.TraceID().String())
}

func TestContextFromRawDocumentV1_PropagationDisabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "true")
	t.Setenv(envMongoPropagationEnabled, "false")
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
}

func TestStartDeliverSpanDisabled(t *testing.T) {
	coll := &Collection{deliverTracer: nil}
	ctx := context.Background()
	got, span := coll.startDeliverSpan(ctx, "testdb", "testcoll")
	defer span.End()
	assert.Equal(t, ctx, got, "expected unchanged ctx when deliverTracer is nil")
	assert.False(t, trace.SpanFromContext(got).SpanContext().IsValid(), "expected no span in ctx when deliverTracer is nil")
}

func TestStartDeliverSpanEnabled(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	tracer := tp.Tracer(ScopeName)
	coll := &Collection{
		deliverTracer: tracer,
		serverAddr:    "localhost",
		serverPort:    27017,
	}

	// Establish a parent span so we can verify the deliver span is a child.
	parentCtx, parentSpan := tp.Tracer(ScopeName).Start(context.Background(), "parent")
	defer parentSpan.End()

	got, deliverSpan := coll.startDeliverSpan(parentCtx, "testdb", "testcoll")
	defer deliverSpan.End()

	deliverSC := trace.SpanFromContext(got).SpanContext()
	require.True(t, deliverSC.IsValid(), "expected valid span context in returned ctx")
	parentSC := parentSpan.SpanContext()
	assert.NotEqual(t, parentSC.SpanID(), deliverSC.SpanID(), "deliver span ID should differ from parent span ID")
	assert.Equal(t, parentSC.TraceID(), deliverSC.TraceID(), "deliver span should share trace ID with parent")
	// span should still be recording — caller has not yet called End()
	if ro, ok := deliverSpan.(interface{ IsRecording() bool }); ok {
		assert.True(t, ro.IsRecording(), "deliver span should still be recording before End() is called")
	}
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
