package traced

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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

	mc, err := mongo.Connect(context.Background(), options.Client().ApplyURI("mongodb://127.0.0.1:1"))
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
