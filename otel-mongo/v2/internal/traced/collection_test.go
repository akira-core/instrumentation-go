package traced

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// spanServerAttrs extracts server.address/server.port from a recorded span's attributes.
func spanServerAttrs(t *testing.T, span sdktrace.ReadOnlySpan) (addr string, port int64, sawPort bool) {
	t.Helper()
	for _, kv := range span.Attributes() {
		switch string(kv.Key) {
		case "server.address":
			addr = kv.Value.AsString()
		case "server.port":
			port = kv.Value.AsInt64()
			sawPort = true
		}
	}
	return addr, port, sawPort
}

// TestCollection_FallsBackToStaticAddrWhenNoCommandCaptured exercises the real
// InsertOne code path (not a mock) against a *mongo.Client whose Connect is
// lazy (mongo.Connect does not dial synchronously) pointed at an address nothing
// listens on. With a short context deadline, InsertOne fails before the driver
// ever selects a server and sends a wire command, so CommandMonitor.Started
// never fires and the capture holder stays empty. The span must then carry the
// static ServerAddr/ServerPort exactly as it did before per-command capture
// existed — see spec "Fallback to static URI-derived address".
func TestCollection_FallsBackToStaticAddrWhenNoCommandCaptured(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mc, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mc.Disconnect(context.Background()) })

	impl := &Collection{
		Coll:       mc.Database("otelmongo_test").Collection("docs"),
		Tracer:     tp.Tracer("test"),
		ServerAddr: "static-fallback-host",
		ServerPort: 27018, // non-default: proves server.port also falls back, not just server.address
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = impl.InsertOne(opCtx, bson.M{"x": 1}) //nolint:errcheck // expected to fail fast; only the span attributes matter here

	spans := sr.Ended()
	require.Len(t, spans, 1)
	addr, port, sawPort := spanServerAttrs(t, spans[0])
	assert.Equal(t, "static-fallback-host", addr)
	require.True(t, sawPort, "expected server.port for a non-default port fallback")
	assert.Equal(t, int64(27018), port)
}

// newFailingPropCollection builds a propagation-enabled Collection pointed at a lazy
// (never-dialed) client. With PropagationEnabled and an un-encodable document, the
// _oteltrace injection fails and the method returns *before* the raw driver call — the
// early-return path that must still emit the static server.* fallback.
func newFailingPropCollection(t *testing.T, sr *tracetest.SpanRecorder) *Collection {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mc, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mc.Disconnect(context.Background()) })

	return &Collection{
		Coll:               mc.Database("otelmongo_test").Collection("docs"),
		Tracer:             tp.Tracer("test"),
		Propagator:         propagation.TraceContext{},
		PropagationEnabled: true,
		ServerAddr:         "static-fallback-host",
		ServerPort:         27018,
	}
}

// TestCollection_InsertOne_InjectFailureKeepsStaticAddr proves InsertOne's inject-failure
// early return still carries the static server.* fallback (regression: it used to skip it),
// and that the span is marked as errored (regression: it used to skip RecordSpanError too).
func TestCollection_InsertOne_InjectFailureKeepsStaticAddr(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	impl := newFailingPropCollection(t, sr)

	_, err := impl.InsertOne(context.Background(), bson.M{"bad": make(chan int)})
	require.Error(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	addr, port, sawPort := spanServerAttrs(t, spans[0])
	assert.Equal(t, "static-fallback-host", addr)
	require.True(t, sawPort, "inject-failure span must still carry server.port fallback")
	assert.Equal(t, int64(27018), port)
	assert.Equal(t, codes.Error, spans[0].Status().Code, "inject-failure span must be recorded as errored")
	assert.NotEmpty(t, spans[0].Status().Description, "inject-failure span must carry an error description")
}

// TestCollection_InsertMany_InjectFailureKeepsStaticAddr is the InsertMany counterpart:
// an un-encodable document in the per-document loop makes InjectTraceIntoDocument fail
// before the driver call, and the failed span must still carry the static server.*
// fallback and be recorded as errored.
func TestCollection_InsertMany_InjectFailureKeepsStaticAddr(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	impl := newFailingPropCollection(t, sr)

	_, err := impl.InsertMany(context.Background(), []any{bson.M{"bad": make(chan int)}})
	require.Error(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	addr, port, sawPort := spanServerAttrs(t, spans[0])
	assert.Equal(t, "static-fallback-host", addr)
	require.True(t, sawPort, "inject-failure span must still carry server.port fallback")
	assert.Equal(t, int64(27018), port)
	assert.Equal(t, codes.Error, spans[0].Status().Code, "inject-failure span must be recorded as errored")
	assert.NotEmpty(t, spans[0].Status().Description, "inject-failure span must carry an error description")
}

// TestCollection_ReplaceOne_InjectFailureKeepsStaticAddr is the ReplaceOne counterpart:
// an un-encodable replacement document makes InjectTraceIntoDocument fail before the
// driver call, and the failed span must still carry the static server.* fallback and be
// recorded as errored.
func TestCollection_ReplaceOne_InjectFailureKeepsStaticAddr(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	impl := newFailingPropCollection(t, sr)

	_, err := impl.ReplaceOne(context.Background(), bson.M{"_id": 1}, bson.M{"bad": make(chan int)})
	require.Error(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	addr, port, sawPort := spanServerAttrs(t, spans[0])
	assert.Equal(t, "static-fallback-host", addr)
	require.True(t, sawPort, "inject-failure span must still carry server.port fallback")
	assert.Equal(t, int64(27018), port)
	assert.Equal(t, codes.Error, spans[0].Status().Code, "inject-failure span must be recorded as errored")
	assert.NotEmpty(t, spans[0].Status().Description, "inject-failure span must carry an error description")
}

// TestCollection_BulkWrite_InjectFailureKeepsStaticAddr is the BulkWrite counterpart:
// an un-encodable InsertOneModel makes BuildBulkWriteModelsWithTrace fail before the
// driver call, and the failed span must still carry the static server.* fallback.
func TestCollection_BulkWrite_InjectFailureKeepsStaticAddr(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	impl := newFailingPropCollection(t, sr)

	bad := mongo.NewInsertOneModel().SetDocument(bson.M{"bad": make(chan int)})
	_, err := impl.BulkWrite(context.Background(), []mongo.WriteModel{bad})
	require.Error(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	addr, port, sawPort := spanServerAttrs(t, spans[0])
	assert.Equal(t, "static-fallback-host", addr)
	require.True(t, sawPort, "inject-failure span must still carry server.port fallback")
	assert.Equal(t, int64(27018), port)
}
