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

// TestParseServerFromURI exercises the URI-derived static fallback directly.
// It covers the multi-host replica-set and mongodb+srv:// forms that motivated
// per-command capture (only the first host is used; the SRV name is not
// resolved), the userinfo/path/query trimming, IPv6 (single and multi-host),
// and — per the caller's requirement — the stray-space and stray-newline forms
// a URI can pick up when assembled across config-file lines. Cases that url.Parse
// would reject wholesale (last host without a port, three or more hosts, IPv6
// host lists, embedded whitespace) resolve to the first host here because the
// authority is sliced out by hand.
func TestParseServerFromURI(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		wantAddr string
		wantPort int
	}{
		{"single host with port", "mongodb://mongo:27018", "mongo", 27018},
		{"single host default port", "mongodb://mongo", "mongo", 27017},
		{"single host with path and query", "mongodb://mongo:27018/admin?w=majority", "mongo", 27018},
		{"multi-host replica set uses first", "mongodb://host1:27017,host2:27018", "host1", 27017},
		{"multi-host first host non-default port", "mongodb://host1:27019,host2:27020", "host1", 27019},
		{"multi-host last host omits port", "mongodb://host1:27017,host2", "host1", 27017},
		{"three hosts last omits port", "mongodb://host1:27019,host2:27018,host3", "host1", 27019},
		{"multi-host with path and query", "mongodb://host1:27017,host2:27018/admin?replicaSet=rs0", "host1", 27017},
		{"userinfo stripped", "mongodb://user:pass@host1:27018,host2:27019", "host1", 27018},
		{"mongodb+srv name not resolved", "mongodb+srv://cluster.example.mongodb.net", "cluster.example.mongodb.net", 27017},
		{"single ipv6 host", "mongodb://[::1]:27017", "::1", 27017},
		{"multi-host ipv6 uses first", "mongodb://[::1]:27017,[::2]:27018", "::1", 27017},
		{"multi-host newline before second host", "mongodb://host1:27017,\nhost2:27018", "host1", 27017},
		{"space after comma", "mongodb://host1:27018, host2:27019", "host1", 27018},
		{"space before comma", "mongodb://host1:27020 ,host2:27021", "host1", 27020},
		{"tab and crlf around hosts, last omits port", "mongodb://host1:27018 ,\r\n\thost2", "host1", 27018},
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
