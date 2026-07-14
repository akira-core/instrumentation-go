package otelmongo

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// TestCollectionCapturesPerCommandServerAddress exercises the full
// CommandMonitor -> context-holder -> span.SetAttributes path against a real
// MongoDB server. The traced.Collection is deliberately given a wrong static
// ServerAddr/ServerPort so the assertion proves the span's server.address
// comes from the per-command capture, not the static fallback — see spec
// "Per-command server address capture" / design.md Decision 2.
func TestCollectionCapturesPerCommandServerAddress(t *testing.T) {
	uri := requireMongoDB(t)
	ctx := context.Background()

	// An independent chained monitor records the insert command's real
	// ConnectionID straight from the driver. It is the oracle for the assertion
	// below: rather than compare against the URI (which for a replica-set
	// container legitimately differs from the advertised connection host — the
	// very reason per-command capture exists), we compare the span attribute
	// against the address the driver itself reported for this exact command.
	var insertConnID string
	oracle := &event.CommandMonitor{
		Started: func(_ context.Context, ev *event.CommandStartedEvent) {
			if ev.CommandName == "insert" {
				insertConnID = ev.ConnectionID
			}
		},
	}
	clientOpts := options.Client().ApplyURI(uri).SetMonitor(shared.NewCommandMonitor(oracle))
	mc, err := mongo.Connect(ctx, clientOpts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mc.Disconnect(context.Background()) })

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	rawColl := mc.Database("otelmongo_capture_test").Collection("docs")
	t.Cleanup(func() { _ = rawColl.Drop(context.Background()) })

	impl := &traced.Collection{
		Coll:       rawColl,
		Tracer:     tp.Tracer("test"),
		ServerAddr: "bogus-fallback-host",
		ServerPort: 1,
	}

	_, err = impl.InsertOne(ctx, bson.M{"x": 1})
	require.NoError(t, err)

	spans := sr.Ended()
	require.NotEmpty(t, spans)
	span := spans[len(spans)-1]

	var gotAddr string
	var gotPort int64
	var sawPort, gotFallback bool
	for _, kv := range span.Attributes() {
		switch string(kv.Key) {
		case "server.address":
			gotAddr = kv.Value.AsString()
		case "server.port":
			gotPort = kv.Value.AsInt64()
			sawPort = true
		case "mongodb.server_address.fallback":
			gotFallback = kv.Value.AsBool()
		}
	}
	assert.NotEqual(t, "bogus-fallback-host", gotAddr,
		"expected the per-command captured address to override the deliberately wrong static fallback")
	assert.False(t, gotFallback, "expected no mongodb.server_address.fallback attribute when capture succeeds")

	// Strengthened beyond "not the bogus fallback": the span must carry the exact
	// host:port the driver reported for this insert command, parsed from the
	// oracle monitor's independently-observed ConnectionID (format "<addr>[-<n>]").
	require.NotEmpty(t, insertConnID, "oracle monitor should have observed the insert command's ConnectionID")
	connAddr := insertConnID
	if i := strings.LastIndexByte(connAddr, '['); i >= 0 {
		connAddr = connAddr[:i]
	}
	wantAddr, wantPort := shared.SplitHostPort(connAddr)
	require.NotEmpty(t, wantAddr, "test setup: could not parse host from ConnectionID %q", insertConnID)
	assert.Equal(t, wantAddr, gotAddr, "captured server.address should match the driver's ConnectionID")
	if wantPort != 27017 {
		require.True(t, sawPort, "expected server.port for the non-default connection port")
		assert.Equal(t, int64(wantPort), gotPort, "captured server.port should match the driver's ConnectionID")
	} else {
		assert.False(t, sawPort, "server.port omitted for the default 27017 per semconv")
	}
}

// TestCollectionCapturesPerCommandServerAddress_ChainsCallerMonitor mirrors the
// spec's "Caller-supplied CommandMonitor is chained, not replaced" requirement
// end-to-end: a caller Started/Succeeded callback registered alongside our own
// address capture must still fire for every command.
func TestCollectionCapturesPerCommandServerAddress_ChainsCallerMonitor(t *testing.T) {
	uri := requireMongoDB(t)
	ctx := context.Background()

	var callerStartedCount, callerSucceededCount int
	caller := &event.CommandMonitor{
		Started:   func(context.Context, *event.CommandStartedEvent) { callerStartedCount++ },
		Succeeded: func(context.Context, *event.CommandSucceededEvent) { callerSucceededCount++ },
	}

	clientOpts := options.Client().ApplyURI(uri).SetMonitor(shared.NewCommandMonitor(caller))
	mc, err := mongo.Connect(ctx, clientOpts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mc.Disconnect(context.Background()) })

	rawColl := mc.Database("otelmongo_capture_test").Collection("chain_docs")
	t.Cleanup(func() { _ = rawColl.Drop(context.Background()) })

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	impl := &traced.Collection{Coll: rawColl, Tracer: tp.Tracer("test")}

	_, err = impl.InsertOne(ctx, bson.M{"x": 1})
	require.NoError(t, err)

	assert.Positive(t, callerStartedCount, "expected caller's Started callback to still fire")
	assert.Positive(t, callerSucceededCount, "expected caller's Succeeded callback to still fire")
}
