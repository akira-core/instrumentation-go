package integration_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	otelmongo "github.com/akira-core/instrumentation-go/otel-mongo/v2"
)

// These tests stand up a real GO Feature Flag relay proxy in front of a real
// MongoDB. Flag RESOLUTION is unit-tested with an in-memory provider; what only
// a real relay can prove is that the wiring recipe the documentation tells
// applications to copy — provider options, endpoint format, flag keys matching a
// real relay configuration file — actually decides this module's switches.
//
// otel-nats carries the equivalent suite for the tracing key alone. This module
// is covered separately because its second key does something no other module's
// does: otel-mongo-propagation decides whether roughly 90 bytes of _oteltrace
// are written into the application's OWN documents, never stripped on read and
// removable only by a $unset migration. "An operator can start and stop that
// from the relay" is a claim about stored data, so it is asserted against stored
// data.
//
// TestMain sets all three module variables to 1 for this package, so unless a
// test overrides one, every disabled result below is attributable to the relay.

const relayProxyYAML = `listen: 1031
pollingInterval: 1000
retriever:
  kind: file
  path: /goff/flags.yaml
`

// relayFlagsYAML renders a GO Feature Flag configuration serving each given key
// at a fixed boolean. Keys are sorted so the rendered document is deterministic.
func relayFlagsYAML(flags map[string]bool) string {
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		variation := "disabled"
		if flags[k] {
			variation = "enabled"
		}
		fmt.Fprintf(&b, `%s:
  variations:
    enabled: true
    disabled: false
  defaultRule:
    variation: %s
`, k, variation)
	}
	return b.String()
}

// startRelayProxy runs a GO Feature Flag relay proxy serving flags and returns
// its base URL.
func startRelayProxy(t *testing.T, flags map[string]bool) string {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "gofeatureflag/go-feature-flag:v1.45.1",
		ExposedPorts: []string{"1031/tcp"},
		Files: []testcontainers.ContainerFile{
			{
				Reader:            strings.NewReader(relayProxyYAML),
				ContainerFilePath: "/goff/goff-proxy.yaml",
				FileMode:          0o644,
			},
			{
				Reader:            strings.NewReader(relayFlagsYAML(flags)),
				ContainerFilePath: "/goff/flags.yaml",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForHTTP("/health").WithPort("1031/tcp").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "start relay proxy container")
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "1031")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// useRelay starts a relay serving flags and binds a provider pointed at it —
// the wiring an application copies. DataCollectorDisabled is required (see
// feature-flags.md); the auto-install path hardcodes it for applications that
// set OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT instead of writing this.
//
// It must run BEFORE the Client is constructed: relayPossible is resolved at
// construction, so a client built earlier resolves statically for its whole life
// and never consults the relay.
func useRelay(t *testing.T, flags map[string]bool) {
	t.Helper()
	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:              startRelayProxy(t, flags),
		DataCollectorDisabled: true,
	})
	require.NoError(t, err, "construct GO Feature Flag provider")
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, provider))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})
}

// insertAndRead performs one InsertOne through the wrapper and reads the stored
// document back through the RAW driver, so what it returns is the bytes MongoDB
// holds rather than anything the wrapper chose to show.
func insertAndRead(t *testing.T, coll *otelmongo.Collection) bson.Raw {
	t.Helper()
	ctx := context.Background()

	res, err := coll.InsertOne(ctx, bson.D{{Key: "hello", Value: "relay"}})
	require.NoError(t, err)

	var raw bson.Raw
	require.NoError(t, coll.Collection.
		FindOne(ctx, bson.D{{Key: "_id", Value: res.InsertedID}}).Decode(&raw))
	return raw
}

// relayCollection builds a client AFTER the provider is bound and returns a
// collection in its own namespace, so no two tests read each other's documents.
func relayCollection(t *testing.T, name string) *otelmongo.Collection {
	t.Helper()
	client, err := otelmongo.NewClient(mongoURI)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	coll := client.Database("integ_v2_relay").Collection(name)
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })
	return coll
}

func hasOtelTrace(raw bson.Raw) bool {
	_, err := raw.LookupErr("_oteltrace")
	return err == nil
}

// TestRelayProxyDisablesWhatTheEnvironmentEnabled is the revoke direction
// end-to-end: the deployment turned both switches on, and one relay
// configuration file stops the spans AND the document writes.
func TestRelayProxyDisablesWhatTheEnvironmentEnabled(t *testing.T) {
	useRelay(t, map[string]bool{
		"otel-mongo-tracing":     false,
		"otel-mongo-propagation": false,
	})

	tp, sr := newTestProvider()
	setupOtel(tp)

	raw := insertAndRead(t, relayCollection(t, "disabled"))

	assert.Empty(t, sr.Ended(),
		"the relay serves otel-mongo-tracing=false, which disables what OTEL_MONGO_TRACING_ENABLED=1 deployed")
	assert.False(t, hasOtelTrace(raw),
		"and no _oteltrace may be written into the operator's document")
}

// TestRelayProxyEnablesPropagationTheDeploymentLeftOff is the direction the
// superseded revoke-only model could not express, on the switch where it matters
// most. The deployment explicitly disabled propagation, so a document carrying
// _oteltrace can only be the relay's doing.
func TestRelayProxyEnablesPropagationTheDeploymentLeftOff(t *testing.T) {
	t.Setenv("OTEL_MONGO_PROPAGATION_ENABLED", "false")
	useRelay(t, map[string]bool{"otel-mongo-propagation": true})

	tp, sr := newTestProvider()
	setupOtel(tp)

	raw := insertAndRead(t, relayCollection(t, "enabled_prop"))

	assert.NotEmpty(t, sr.Ended(),
		"the relay does not define otel-mongo-tracing, so OTEL_MONGO_TRACING_ENABLED=1 stands and spans are emitted")
	assert.True(t, hasOtelTrace(raw),
		"the relay enabled propagation the deployment left off, so _oteltrace reaches the document")
}

// TestRelayProxyRevokesPropagationButKeepsTracing proves the two keys are
// independent through a real relay: an operator can stop writing into documents
// without losing observability of the commands.
func TestRelayProxyRevokesPropagationButKeepsTracing(t *testing.T) {
	useRelay(t, map[string]bool{
		"otel-mongo-tracing":     true,
		"otel-mongo-propagation": false,
	})

	tp, sr := newTestProvider()
	setupOtel(tp)

	raw := insertAndRead(t, relayCollection(t, "tracing_only"))

	assert.NotEmpty(t, sr.Ended(), "tracing stays on")
	assert.False(t, hasOtelTrace(raw),
		"propagation is revoked independently, so the document keeps no _oteltrace")
}

// TestRelayProxyMasterVetoStopsEverything is the process-wide kill switch in its
// relay spelling: one key, and no module traces or writes, however enthusiastic
// its own key and its own environment variables are.
func TestRelayProxyMasterVetoStopsEverything(t *testing.T) {
	useRelay(t, map[string]bool{
		otelflags.FlagKeyGlobalTracing: false,
		"otel-mongo-tracing":           true,
		"otel-mongo-propagation":       true,
	})

	tp, sr := newTestProvider()
	setupOtel(tp)

	raw := insertAndRead(t, relayCollection(t, "master_veto"))

	assert.Empty(t, sr.Ended(),
		"the master key vetoes a module both its own key and its environment enable")
	assert.False(t, hasOtelTrace(raw), "and nothing is written into the document")
}

// TestRelayProxyMissingFlagsLeaveTheEnvironmentInCharge: the relay is up and
// healthy but defines neither Mongo key — the ordinary state of a relay that
// carries only the master kill switch. FLAG_NOT_FOUND must fall through to the
// environment, which TestMain set to on for both.
func TestRelayProxyMissingFlagsLeaveTheEnvironmentInCharge(t *testing.T) {
	useRelay(t, map[string]bool{"some-unrelated-flag": false})

	tp, sr := newTestProvider()
	setupOtel(tp)

	raw := insertAndRead(t, relayCollection(t, "no_keys"))

	assert.NotEmpty(t, sr.Ended(),
		"the relay has no opinion, so OTEL_MONGO_TRACING_ENABLED=1 stands")
	assert.True(t, hasOtelTrace(raw),
		"and OTEL_MONGO_PROPAGATION_ENABLED=1 stands with it")
}
