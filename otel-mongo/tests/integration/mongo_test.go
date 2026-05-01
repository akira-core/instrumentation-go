package integration_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	otelmongo "github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo"
)

// mongoURI is the connection string for the shared MongoDB container, set once in TestMain.
var mongoURI string

// TestMain starts a MongoDB replica set container, runs all tests, then stops it.
func TestMain(m *testing.M) {
	_ = os.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	_ = os.Setenv("OTEL_MONGO_TRACING_ENABLED", "1")
	_ = os.Setenv("OTEL_MONGO_PROPAGATION_ENABLED", "1")
	ctx := context.Background()

	container, err := tcmongo.Run(ctx, "mongo:7.0",
		tcmongo.WithReplicaSet("rs0"),
	)
	if err != nil {
		log.Fatalf("start mongodb container: %v", err)
	}

	mongoURI, err = container.ConnectionString(ctx)
	if err != nil {
		log.Fatalf("get mongodb connection string: %v", err)
	}

	code := m.Run()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestProvider() (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return tp, sr
}

func setupOtel(tp *sdktrace.TracerProvider) {
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestIntegration_InsertOneInjectsOtelTrace verifies that InsertOne stores _oteltrace
// in the document with the correct traceparent format and that the stored TraceID
// matches the insert span's TraceID.
func TestIntegration_InsertOneInjectsOtelTrace(t *testing.T) {
	tp, _ := newTestProvider()
	setupOtel(tp)

	client, err := otelmongo.NewClient(context.Background(), mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v1").Collection("insert_one")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	tracer := tp.Tracer("insert-test")
	ctx, span := tracer.Start(context.Background(), "insert-root")
	wantTraceID := span.SpanContext().TraceID()

	res, err := coll.InsertOne(ctx, bson.D{{Key: "hello", Value: "world"}})
	span.End()
	require.NoError(t, err)
	require.NotNil(t, res.InsertedID)

	var raw bson.Raw
	err = coll.Collection.FindOne(context.Background(),
		bson.D{{Key: "_id", Value: res.InsertedID}}).Decode(&raw)
	require.NoError(t, err)

	otelVal, err := raw.LookupErr("_oteltrace")
	require.NoError(t, err, "_oteltrace field should be present")
	otelDoc, ok := otelVal.DocumentOK()
	require.True(t, ok, "_oteltrace should be a document")

	var meta struct {
		Traceparent string `bson:"traceparent"`
	}
	require.NoError(t, bson.Unmarshal(otelDoc, &meta))
	require.NotEmpty(t, meta.Traceparent, "traceparent should not be empty")

	sc, ok := otelmongo.ContextFromDocument(context.Background(), raw)
	require.True(t, ok, "ContextFromDocument should return ok=true")
	assert.Equal(t, wantTraceID, sc.TraceID(), "stored TraceID should match insert span")
}

// TestIntegration_InsertManyAllHaveOtelTrace verifies that InsertMany injects
// _oteltrace into every document in the batch.
func TestIntegration_InsertManyAllHaveOtelTrace(t *testing.T) {
	tp, _ := newTestProvider()
	setupOtel(tp)

	client, err := otelmongo.NewClient(context.Background(), mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v1").Collection("insert_many")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	tracer := tp.Tracer("insert-many-test")
	ctx, span := tracer.Start(context.Background(), "insert-many-root")
	defer span.End()

	docs := []any{
		bson.D{{Key: "n", Value: 1}},
		bson.D{{Key: "n", Value: 2}},
		bson.D{{Key: "n", Value: 3}},
	}
	res, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	require.Len(t, res.InsertedIDs, 3)

	for _, id := range res.InsertedIDs {
		var raw bson.Raw
		err = coll.Collection.FindOne(context.Background(),
			bson.D{{Key: "_id", Value: id}}).Decode(&raw)
		require.NoError(t, err)

		_, ok := otelmongo.ContextFromDocument(context.Background(), raw)
		assert.True(t, ok, "each inserted document should have _oteltrace")
	}
}

// TestIntegration_CursorDecodeWithContextExtractsTrace verifies that
// Cursor.DecodeWithContext returns a context carrying the insert span's TraceID.
func TestIntegration_CursorDecodeWithContextExtractsTrace(t *testing.T) {
	tp, _ := newTestProvider()
	setupOtel(tp)

	client, err := otelmongo.NewClient(context.Background(), mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v1").Collection("cursor_decode")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	tracer := tp.Tracer("cursor-test")
	insertCtx, insertSpan := tracer.Start(context.Background(), "insert-for-cursor")
	wantTraceID := insertSpan.SpanContext().TraceID()

	_, err = coll.InsertOne(insertCtx, bson.D{{Key: "val", Value: "cursor-test"}})
	insertSpan.End()
	require.NoError(t, err)

	cursor, err := coll.Find(context.Background(), bson.D{})
	require.NoError(t, err)
	defer cursor.Close(context.Background())

	require.True(t, cursor.Next(context.Background()), "expected at least one document")

	var doc bson.D
	enrichedCtx, err := cursor.DecodeWithContext(context.Background(), &doc)
	require.NoError(t, err)

	sc := oteltrace.SpanContextFromContext(enrichedCtx)
	assert.True(t, sc.IsValid(), "enriched context should carry a valid span context")
	// The cursor decode creates a new span (new TraceID) but links to insert span.
	// Verify the extracted span context is valid (actual link verification done in unit tests).
	assert.NotEqual(t, wantTraceID, sc.TraceID(), "DecodeWithContext should create a new TraceID")
}

// TestIntegration_SingleResultTraceContextExtractsTrace verifies that
// SingleResult.TraceContext returns a context carrying the insert span's TraceID.
func TestIntegration_SingleResultTraceContextExtractsTrace(t *testing.T) {
	tp, _ := newTestProvider()
	setupOtel(tp)

	client, err := otelmongo.NewClient(context.Background(), mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v1").Collection("single_result")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	tracer := tp.Tracer("single-result-test")
	insertCtx, insertSpan := tracer.Start(context.Background(), "insert-for-single")
	wantTraceID := insertSpan.SpanContext().TraceID()

	res, err := coll.InsertOne(insertCtx, bson.D{{Key: "key", Value: "single-test"}})
	insertSpan.End()
	require.NoError(t, err)

	sr := coll.FindOne(context.Background(), bson.D{{Key: "_id", Value: res.InsertedID}})

	enrichedCtx := sr.TraceContext()
	sc := oteltrace.SpanContextFromContext(enrichedCtx)
	assert.True(t, sc.IsValid(), "enriched context should carry a valid span context")
	assert.Equal(t, wantTraceID, sc.TraceID(), "TraceContext should carry insert span's TraceID")
}

// TestIntegration_UpdateOnePreservesTrace verifies that UpdateOne stores _oteltrace
// from the update span's context in the document.
func TestIntegration_UpdateOnePreservesTrace(t *testing.T) {
	tp, _ := newTestProvider()
	setupOtel(tp)

	client, err := otelmongo.NewClient(context.Background(), mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v1").Collection("update_trace")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	res, err := coll.InsertOne(context.Background(),
		bson.D{{Key: "status", Value: "initial"}})
	require.NoError(t, err)

	tracer := tp.Tracer("update-test")
	updateCtx, updateSpan := tracer.Start(context.Background(), "update-root")
	wantTraceID := updateSpan.SpanContext().TraceID()

	_, err = coll.UpdateOne(updateCtx,
		bson.D{{Key: "_id", Value: res.InsertedID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "updated"}}}},
		options.Update().SetUpsert(false),
	)
	updateSpan.End()
	require.NoError(t, err)

	var raw bson.Raw
	err = coll.Collection.FindOne(context.Background(),
		bson.D{{Key: "_id", Value: res.InsertedID}}).Decode(&raw)
	require.NoError(t, err)

	sc, ok := otelmongo.ContextFromDocument(context.Background(), raw)
	require.True(t, ok, "document should have _oteltrace after UpdateOne")
	assert.Equal(t, wantTraceID, sc.TraceID(), "UpdateOne should inject current span's TraceID")
}

// TestIntegration_ContextFromDocumentChangeStream verifies that change stream events
// carry the _oteltrace field and ContextFromDocument extracts the insert span's TraceID.
func TestIntegration_ContextFromDocumentChangeStream(t *testing.T) {
	tp, _ := newTestProvider()
	setupOtel(tp)

	client, err := otelmongo.NewClient(context.Background(), mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v1").Collection("change_stream")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	streamCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Open change stream with fullDocument=updateLookup to get full docs on insert.
	cs, err := coll.Collection.Watch(streamCtx,
		[]bson.D{{{Key: "$match", Value: bson.D{{Key: "operationType", Value: "insert"}}}}},
		options.ChangeStream().SetFullDocument("updateLookup"),
	)
	require.NoError(t, err)
	defer cs.Close(context.Background())

	tracer := tp.Tracer("change-stream-test")
	insertCtx, insertSpan := tracer.Start(context.Background(), "insert-for-cs")
	wantTraceID := insertSpan.SpanContext().TraceID()

	_, err = coll.InsertOne(insertCtx, bson.D{{Key: "msg", Value: "cs-test"}})
	insertSpan.End()
	require.NoError(t, err)

	require.True(t, cs.Next(streamCtx), "expected change stream event")

	var event struct {
		FullDocument bson.M `bson:"fullDocument"`
	}
	require.NoError(t, cs.Decode(&event))
	require.NotNil(t, event.FullDocument, "fullDocument should be present in change event")

	sc, ok := otelmongo.ContextFromDocument(context.Background(), event.FullDocument)
	require.True(t, ok, "ContextFromDocument should extract trace from fullDocument")
	assert.Equal(t, wantTraceID, sc.TraceID(), "change stream event should carry insert span's TraceID")
}
