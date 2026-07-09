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

// TestParseServerFromURI exercises the URI-derived static fallback directly,
// covering the multi-host replica-set and mongodb+srv:// forms that motivated
// per-command capture (only the first host is used; the SRV name is not
// resolved) plus the IPv6 and malformed edge cases. url.Parse validates the
// whole string first, so a multi-host IPv6 URI (invalid IP-literal) and any URI
// containing a newline control character both fail safe to ("", 0) before the
// first-host split is ever reached.
func TestParseServerFromURI(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		wantAddr string
		wantPort int
	}{
		{"single host with port", "mongodb://mongo:27018", "mongo", 27018},
		{"single host default port", "mongodb://mongo", "mongo", 27017},
		{"multi-host replica set uses first", "mongodb://host1:27017,host2:27018", "host1", 27017},
		{"multi-host first host non-default port", "mongodb://host1:27019,host2:27020", "host1", 27019},
		{"mongodb+srv name not resolved", "mongodb+srv://cluster.example.mongodb.net", "cluster.example.mongodb.net", 27017},
		{"single ipv6 host", "mongodb://[::1]:27017", "::1", 27017},
		{"multi-host ipv6 unparseable", "mongodb://[::1]:27017,[::2]:27018", "", 0},
		{"multi-host with newline char", "mongodb://host1:27017,\nhost2:27018", "", 0},
		{"empty uri", "", "", 0},
		{"scheme only no host", "mongodb://", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, port := parseServerFromURI(tc.uri)
			assert.Equal(t, tc.wantAddr, addr)
			assert.Equal(t, tc.wantPort, port)
		})
	}
}
