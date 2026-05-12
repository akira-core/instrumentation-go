package otelmongo

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// testMongoURI is populated by TestMain from the container. Zero value = Docker unavailable.
var testMongoURI string

// TestMain starts a single MongoDB container for all integration tests in this
// package and tears it down after the suite completes. If the container cannot
// be started (e.g. Docker is unavailable), integration tests fall back to the
// MONGO_URI environment variable and are skipped if that is also unset.
func TestMain(m *testing.M) {
	ctx := context.Background()
	var container *tcmongo.MongoDBContainer
	container, err := tcmongo.Run(ctx, "mongo:7.0",
		tcmongo.WithReplicaSet("rs0"),
	)
	if err != nil {
		log.Printf("WARNING: could not start mongodb container (integration tests will be skipped unless MONGO_URI is set): %v", err)
	} else {
		testMongoURI, err = container.ConnectionString(ctx)
		if err != nil {
			log.Printf("WARNING: could not get mongodb connection string: %v", err)
		}
	}
	code := m.Run()
	if container != nil {
		_ = container.Terminate(ctx)
	}
	os.Exit(code)
}

// requireMongoDB returns the MongoDB URI for integration tests, preferring the
// container URI set by TestMain and falling back to the MONGO_URI env var.
func requireMongoDB(t *testing.T) string {
	t.Helper()
	if testMongoURI != "" {
		return testMongoURI
	}
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("no MongoDB available: TestMain container not running and MONGO_URI not set")
	}
	return uri
}

func TestNewCollection(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	// We only need to verify that NewCollection returns a non-nil wrapper
	// with the correct tracer; we do not need a live server for this.
	raw := &mongo.Collection{}
	coll := NewCollection(raw, tracer, otel.GetTextMapPropagator())
	require.NotNil(t, coll)
	assert.Equal(t, raw, coll.Collection)
	assert.NotNil(t, coll.tracer)

	t.Run("propagationEnabled follows env when tracing on", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "1")
		t.Setenv(envMongoPropagationEnabled, "1")
		c2 := NewCollection(raw, tracer, otel.GetTextMapPropagator())
		assert.True(t, c2.propagationEnabled)
	})

	t.Run("propagationEnabled false when module propagation env false", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "1")
		t.Setenv(envMongoPropagationEnabled, "false")
		c2 := NewCollection(raw, tracer, otel.GetTextMapPropagator())
		assert.False(t, c2.propagationEnabled)
	})

	t.Run("propagationEnabled false when mongo tracing off even if propagation on", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "false")
		t.Setenv(envMongoPropagationEnabled, "1")
		c2 := NewCollection(raw, tracer, otel.GetTextMapPropagator())
		assert.False(t, c2.propagationEnabled)
	})

	t.Run("global off swaps caller tracer with noop", func(t *testing.T) {
		// Even when the caller passes a real tracer, NewCollection must replace it with noop
		// when the global+module tracing gate is off — symmetric with Connect.
		_ = os.Unsetenv(envGlobalTracingEnabled)
		_ = os.Unsetenv(envMongoTracingEnabled)
		sr2 := tracetest.NewSpanRecorder()
		tp2 := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr2))
		t.Cleanup(func() { _ = tp2.Shutdown(context.Background()) })
		realTracer := tp2.Tracer("test")
		c2 := NewCollection(raw, realTracer, otel.GetTextMapPropagator())
		_, span := c2.tracer.Start(context.Background(), "probe")
		span.End()
		assert.Empty(t, sr2.Ended(), "expected zero spans recorded when global tracing is off")
	})

	t.Run("global on keeps caller tracer", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "1")
		t.Setenv(envMongoTracingEnabled, "1")
		sr2 := tracetest.NewSpanRecorder()
		tp2 := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr2))
		t.Cleanup(func() { _ = tp2.Shutdown(context.Background()) })
		realTracer := tp2.Tracer("test")
		c2 := NewCollection(raw, realTracer, otel.GetTextMapPropagator())
		_, span := c2.tracer.Start(context.Background(), "probe")
		span.End()
		assert.Len(t, sr2.Ended(), 1, "expected 1 span recorded when tracing is on")
	})
}

// integrationTP returns a TracerProvider backed by an in-memory SpanRecorder
// for use in integration tests.
func integrationTP(t *testing.T) (trace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	enableTracing(t)
	t.Setenv(envMongoPropagationEnabled, "1")
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, sr
}

// TestConnectGlobalOff_ZeroWrapperSpans is the end-to-end assertion that requirement 1
// (global lock controls all spans) holds: with OTEL_INSTRUMENTATION_GO_TRACING_ENABLED unset,
// Connect must use a noop tracer and an InsertOne call must record zero spans on the
// caller's TracerProvider — and no _oteltrace must be injected into the document.
func TestConnectGlobalOff_ZeroWrapperSpans(t *testing.T) {
	uri := requireMongoDB(t)
	_ = os.Unsetenv(envGlobalTracingEnabled)
	_ = os.Unsetenv(envMongoTracingEnabled)
	_ = os.Unsetenv(envMongoPropagationEnabled)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("global_off_zero_spans")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	res, err := coll.InsertOne(context.Background(), bson.D{{Key: "k", Value: "v"}})
	require.NoError(t, err)

	// No wrapper spans of any kind.
	assert.Empty(t, sr.Ended(), "expected zero wrapper spans when global tracing is off")

	// _oteltrace must NOT be injected when propagation gate is off.
	var raw bson.Raw
	err = coll.Collection.FindOne(context.Background(), bson.D{{Key: "_id", Value: res.InsertedID}}).Decode(&raw)
	require.NoError(t, err)
	_, hasMeta := extractMetadataFromRaw(raw)
	assert.False(t, hasMeta, "expected no _oteltrace field when propagation is off")
}

// TestCollectionInsertOneAndFind verifies InsertOne injects _oteltrace into the document.
// otelmongo wrapper does not create its own span for insert; contrib otelmongo provides the command span.
func TestCollectionInsertOneAndFind(t *testing.T) {
	uri := requireMongoDB(t)
	tp, _ := integrationTP(t)
	tracer := tp.Tracer("otelmongo", trace.WithInstrumentationVersion("0.1.0"))

	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("insert_find")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	ctx, span := tracer.Start(context.Background(), "test-root")
	defer span.End()

	doc := bson.D{{Key: "hello", Value: "world"}}
	res, err := coll.InsertOne(ctx, doc)
	require.NoError(t, err)
	require.NotNil(t, res.InsertedID)

	// Retrieve the raw document and verify _oteltrace is present.
	var raw bson.Raw
	err = coll.Collection.FindOne(ctx, bson.D{{Key: "_id", Value: res.InsertedID}}).Decode(&raw)
	require.NoError(t, err)

	meta, ok := extractMetadataFromRaw(raw)
	require.True(t, ok, "_oteltrace field should have been stored in the document")
	assert.NotEmpty(t, meta.Traceparent)
}

// TestCollectionInsertMany_StoresOtelTrace verifies InsertMany injects _oteltrace into each document.
func TestCollectionInsertMany_StoresOtelTrace(t *testing.T) {
	uri := requireMongoDB(t)
	tp, _ := integrationTP(t)
	tracer := tp.Tracer("otelmongo", trace.WithInstrumentationVersion("0.1.0"))

	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("insert_many_trace")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	ctx, span := tracer.Start(context.Background(), "test-root")
	defer span.End()

	docs := []any{
		bson.D{{Key: "n", Value: 1}},
		bson.D{{Key: "n", Value: 2}},
	}
	res, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	require.Len(t, res.InsertedIDs, 2)

	for _, id := range res.InsertedIDs {
		var raw bson.Raw
		err = coll.Collection.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&raw)
		require.NoError(t, err)
		meta, ok := extractMetadataFromRaw(raw)
		require.True(t, ok, "_oteltrace should be stored in each document")
		assert.NotEmpty(t, meta.Traceparent)
	}
}

// TestCollectionReplaceOne_StoresOtelTrace verifies ReplaceOne injects _oteltrace into the replacement.
func TestCollectionReplaceOne_StoresOtelTrace(t *testing.T) {
	uri := requireMongoDB(t)
	tp, _ := integrationTP(t)
	tracer := tp.Tracer("otelmongo", trace.WithInstrumentationVersion("0.1.0"))

	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("replace_trace")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	_, err = coll.InsertOne(context.Background(), bson.D{{Key: "phase", Value: "old"}})
	require.NoError(t, err)

	ctx, span := tracer.Start(context.Background(), "replace-span")
	defer span.End()

	_, err = coll.ReplaceOne(ctx, bson.D{{Key: "phase", Value: "old"}}, bson.D{{Key: "phase", Value: "new"}})
	require.NoError(t, err)

	cursor, err := coll.Collection.Find(ctx, bson.D{})
	require.NoError(t, err)
	defer cursor.Close(ctx)
	require.True(t, cursor.Next(ctx))
	var raw bson.Raw
	require.NoError(t, cursor.Decode(&raw))
	meta, ok := extractMetadataFromRaw(raw)
	require.True(t, ok, "_oteltrace should be in replaced document")
	assert.NotEmpty(t, meta.Traceparent)
}

func TestCollectionUpdateOne_InjectTrace(t *testing.T) {
	uri := requireMongoDB(t)
	tp, sr := integrationTP(t)
	tracer := tp.Tracer("otelmongo", trace.WithInstrumentationVersion("0.1.0"))

	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("update_inject")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	doc := bson.D{{Key: "status", Value: "new"}}
	res, err := coll.InsertOne(context.Background(), doc)
	require.NoError(t, err)

	updateCtx, updateSpan := tracer.Start(context.Background(), "update-span")
	_, err = coll.UpdateOne(updateCtx,
		bson.D{{Key: "_id", Value: res.InsertedID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "updated"}}}},
	)
	require.NoError(t, err)
	updateSpan.End()

	// UpdateOne span should exist (no extra read; trace is injected into update).
	var updateOneSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "update update_inject" {
			updateOneSpan = s
			break
		}
	}
	require.NotNil(t, updateOneSpan, "updateOne span should have been recorded")
}

func TestCollectionDeleteOne_OneRoundTrip(t *testing.T) {
	uri := requireMongoDB(t)
	tp, sr := integrationTP(t)

	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("delete_one")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	doc := bson.D{{Key: "key", Value: "to-delete"}}
	res, err := coll.InsertOne(context.Background(), doc)
	require.NoError(t, err)

	_, err = coll.DeleteOne(context.Background(), bson.D{{Key: "_id", Value: res.InsertedID}})
	require.NoError(t, err)

	// deleteOne span should exist; no extra read (one round-trip only).
	var deleteSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "delete delete_one" {
			deleteSpan = s
			break
		}
	}
	require.NotNil(t, deleteSpan, "deleteOne span should have been recorded")
}

func TestCollectionCountDocuments(t *testing.T) {
	uri := requireMongoDB(t)
	tp, _ := integrationTP(t)
	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("count_docs")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	_, err = coll.InsertOne(context.Background(), bson.D{{Key: "x", Value: 1}})
	require.NoError(t, err)
	_, err = coll.InsertOne(context.Background(), bson.D{{Key: "x", Value: 2}})
	require.NoError(t, err)

	n, err := coll.CountDocuments(context.Background(), bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

func TestCollectionUpdateByID_InjectTrace(t *testing.T) {
	uri := requireMongoDB(t)
	tp, sr := integrationTP(t)
	tracer := tp.Tracer("otelmongo", trace.WithInstrumentationVersion("0.1.0"))
	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("update_by_id")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	res, err := coll.InsertOne(context.Background(), bson.D{{Key: "v", Value: 0}})
	require.NoError(t, err)
	id := res.InsertedID

	ctx, span := tracer.Start(context.Background(), "update-by-id")
	_, err = coll.UpdateByID(ctx, id, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}})
	require.NoError(t, err)
	span.End()

	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "update update_by_id" {
			found = s
			break
		}
	}
	require.NotNil(t, found, "updateOne span should have been recorded")
}

func TestCollectionDeleteOneByID(t *testing.T) {
	uri := requireMongoDB(t)
	tp, sr := integrationTP(t)
	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("delete_by_id")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	res, err := coll.InsertOne(context.Background(), bson.D{{Key: "k", Value: "v"}})
	require.NoError(t, err)

	dr, err := coll.DeleteOneByID(context.Background(), res.InsertedID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), dr.DeletedCount)

	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "delete delete_by_id" {
			found = s
			break
		}
	}
	require.NotNil(t, found, "deleteOne span should have been recorded")
}

func TestCollectionFindOneByID(t *testing.T) {
	uri := requireMongoDB(t)
	tp, _ := integrationTP(t)
	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("find_one_by_id")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	res, err := coll.InsertOne(context.Background(), bson.D{{Key: "name", Value: "alice"}})
	require.NoError(t, err)

	sr := coll.FindOneByID(context.Background(), res.InsertedID)
	var doc struct {
		Name string `bson:"name"`
	}
	err = sr.Decode(&doc)
	require.NoError(t, err)
	assert.Equal(t, "alice", doc.Name)
}

func TestCollectionFindByIDs(t *testing.T) {
	uri := requireMongoDB(t)
	tp, _ := integrationTP(t)
	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("find_by_ids")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	r1, _ := coll.InsertOne(context.Background(), bson.D{{Key: "n", Value: 1}})
	r2, _ := coll.InsertOne(context.Background(), bson.D{{Key: "n", Value: 2}})
	ids := []any{r1.InsertedID, r2.InsertedID}

	cur, err := coll.FindByIDs(context.Background(), ids)
	require.NoError(t, err)
	defer cur.Close(context.Background())

	var count int
	for cur.Next(context.Background()) {
		count++
	}
	assert.Equal(t, 2, count)
}

func TestCollectionBulkWrite(t *testing.T) {
	uri := requireMongoDB(t)
	tp, sr := integrationTP(t)
	otel.SetTracerProvider(tp)
	client, err := Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	db := client.Database("otelmongo_test")
	coll := db.Collection("bulk_write")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	models := []mongo.WriteModel{
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "a", Value: 1}}),
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "a", Value: 2}}),
	}
	res, err := coll.BulkWrite(context.Background(), models)
	require.NoError(t, err)
	assert.Equal(t, int64(2), res.InsertedCount)

	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "bulkWrite bulk_write" {
			found = s
			break
		}
	}
	require.NotNil(t, found, "bulkWrite span should have been recorded")
}
