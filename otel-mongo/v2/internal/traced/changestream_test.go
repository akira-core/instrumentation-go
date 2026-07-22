package traced

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

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
)

// TestChangeStreamReaderAttrs_CarriesStaticServerAddr locks the invariant that the
// ChangeStream reader's getMore spans keep the static server.address/
// server.port snapshot. Those spans are seeded from baseSpanOpts built by Watch and
// never get a post-call ServerAttributes overwrite, so if DBAttributes stops emitting
// server.* (as it does since the emit-once-post-call refactor) the reader attrs must
// append ServerAttributes explicitly or the spans carry no server.address at all.
func TestChangeStreamReaderAttrs_CarriesStaticServerAddr(t *testing.T) {
	impl := &Collection{
		ServerAddr: "static-cs-host",
		ServerPort: 27019, // non-default: proves server.port survives too
	}
	attrs := impl.changeStreamReaderAttrs("db", "coll")

	var addr string
	var port int64
	var sawPort bool
	for _, kv := range attrs {
		switch string(kv.Key) {
		case "server.address":
			addr = kv.Value.AsString()
		case "server.port":
			port = kv.Value.AsInt64()
			sawPort = true
		}
	}
	assert.Equal(t, "static-cs-host", addr, "reader spans must keep the static server.address")
	require.True(t, sawPort, "expected server.port for a non-default static port")
	assert.Equal(t, int64(27019), port)
}

func TestBuildLinkedSpanCtx_NewTraceIDAndLinksOriginTrace(t *testing.T) {
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

	newCtx, span := buildLinkedSpanCtx(originCtx, tracer, "test-decode-span", nil, originSpanCtx)
	span.End()

	recovered := trace.SpanContextFromContext(newCtx)
	require.True(t, recovered.IsValid())
	assert.NotEqual(t, originSpanCtx.TraceID(), recovered.TraceID())

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "test-decode-span" {
			continue
		}
		found = true
		links := s.Links()
		require.NotEmpty(t, links)
		assert.Equal(t, originSpanCtx.TraceID(), links[0].SpanContext.TraceID())
		break
	}
	require.True(t, found, `expected a span named "test-decode-span"`)
}

// TestChangeStreamDecodeAndTrace_LinksOriginFromFullDocument proves the traced
// ChangeStream extracts _oteltrace from the nested "fullDocument" field (Cursor
// reads it top-level) and links the decode span to the origin trace. A unit test
// cannot drive a successful *mongo.ChangeStream.Decode — the driver returns
// ErrNilCursor without a server-backed cursor — but extraction + link run before
// Decode, so the linked span is still emitted and the ErrNilCursor is expected.
func TestChangeStreamDecodeAndTrace_LinksOriginFromFullDocument(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	tracer := tp.Tracer("test")
	prop := otel.GetTextMapPropagator()

	originCtx, originSpan := tracer.Start(context.Background(), "origin")
	originSpanCtx := originSpan.SpanContext()
	originSpan.End()

	// fullDocument carries the origin trace in its _oteltrace field.
	fullDoc, err := shared.InjectTraceIntoDocument(originCtx, bson.D{{Key: "field", Value: "v"}}, prop)
	require.NoError(t, err, "InjectTraceIntoDocument")
	rawEvent, err := bson.Marshal(bson.D{{Key: "fullDocument", Value: fullDoc}})
	require.NoError(t, err, "bson.Marshal change event")

	impl := &ChangeStream{
		cs:                 &mongo.ChangeStream{Current: bson.Raw(rawEvent)},
		tracer:             tracer,
		propagator:         prop,
		propagationEnabled: true,
		spanName:           "mongo.cursor.decode",
	}

	var out bson.D
	_, err = impl.DecodeAndTrace(context.Background(), &out)
	require.ErrorIs(t, err, mongo.ErrNilCursor, "Decode needs a live cursor; extraction+link still run first")

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "mongo.cursor.decode" {
			continue
		}
		found = true
		links := s.Links()
		require.NotEmpty(t, links, "expected decode span linked to fullDocument origin")
		assert.Equal(t, originSpanCtx.TraceID(), links[0].SpanContext.TraceID())
		break
	}
	require.True(t, found, `expected a span named "mongo.cursor.decode"`)
}

// TestChangeStreamDecodeAndTrace_NoLinkWhenNoTraceMetadata pins that a
// fullDocument without _oteltrace yields a decode span with no link.
func TestChangeStreamDecodeAndTrace_NoLinkWhenNoTraceMetadata(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	tracer := tp.Tracer("test")
	prop := otel.GetTextMapPropagator()

	rawEvent, err := bson.Marshal(bson.D{{Key: "fullDocument", Value: bson.D{{Key: "field", Value: "v"}}}})
	require.NoError(t, err, "bson.Marshal change event")

	impl := &ChangeStream{
		cs:                 &mongo.ChangeStream{Current: bson.Raw(rawEvent)},
		tracer:             tracer,
		propagator:         prop,
		propagationEnabled: true,
		spanName:           "mongo.cursor.decode",
	}

	var out bson.D
	_, err = impl.DecodeAndTrace(context.Background(), &out)
	require.ErrorIs(t, err, mongo.ErrNilCursor)

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "mongo.cursor.decode" {
			continue
		}
		found = true
		assert.Empty(t, s.Links(), "no link expected when fullDocument lacks _oteltrace")
		break
	}
	require.True(t, found, `expected a span named "mongo.cursor.decode"`)
}
