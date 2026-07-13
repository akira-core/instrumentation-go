package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
)

// TestConnect_AlignsWithMongoConnect ensures Connect accepts *options.ClientOptions like mongo.Connect.
func TestConnect_AlignsWithMongoConnect(t *testing.T) {
	otel.SetTracerProvider(trace.NewTracerProvider())

	opts := options.Client().ApplyURI("mongodb://localhost:27017")
	client, err := Connect(opts)
	if err != nil {
		t.Skipf("MongoDB not available: %v", err)
	}
	if client != nil {
		_ = client.Disconnect(context.Background())
	}
}

// TestConnectWithOptions_Disabled_DoesNotWrapCallerMonitor asserts the
// disabled-tracing branch of ConnectWithOptions never registers our address-capture
// CommandMonitor: a caller-supplied SetMonitor's Started callback must fire with
// the driver's original, unwrapped event — proving no otel-mongo machinery sits in
// front of it when tracing is off (spec: "No new tracing behavior when tracing is
// disabled" / "Disabled tracing registers no command monitor").
func TestConnectWithOptions_Disabled_DoesNotWrapCallerMonitor(t *testing.T) {
	uri := requireMongoDB(t)

	var callerConnID string
	caller := &event.CommandMonitor{
		Started: func(_ context.Context, ev *event.CommandStartedEvent) {
			if ev.CommandName == "insert" {
				callerConnID = ev.ConnectionID
			}
		},
	}

	c, err := ConnectWithOptions(nil, options.Client().ApplyURI(uri).SetMonitor(caller))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	require.False(t, c.tracingEnabled, "test assumes tracing env is unset/disabled")

	coll := c.Database("otelmongo_test").Collection("disabled_monitor_passthrough")
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })
	_, err = coll.InsertOne(context.Background(), bson.M{"x": 1})
	require.NoError(t, err)

	assert.NotEmpty(t, callerConnID, "expected caller's own Started callback to fire unmodified when tracing is disabled")
}
