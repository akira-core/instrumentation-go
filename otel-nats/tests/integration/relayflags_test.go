package integration_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// This is the one test in the repository that stands up a real GO Feature Flag
// relay proxy. Its job is not to re-prove flag resolution — that is unit-tested
// with an in-memory provider in every module — but to verify that the wiring
// recipe the documentation tells applications to copy actually resolves against
// a real relay: provider construction options, endpoint format, and flag keys
// matching a real relay configuration file.
//
// Only one module is covered. The wiring is identical across the four, so three
// more containers would add cost without information.
//
// BOTH directions are asserted, because the relay is authoritative in both: it
// can disable tracing a deployment enabled, and enable tracing a deployment left
// off. Each case starts its own relay serving the variation it needs, so neither
// depends on the other's ordering.

// relayFlagsYAML serves otel-nats-tracing at the given default variation.
func relayFlagsYAML(variation string) string {
	return `otel-nats-tracing:
  variations:
    enabled: true
    disabled: false
  defaultRule:
    variation: ` + variation + `
`
}

const relayProxyYAML = `listen: 1031
pollingInterval: 1000
retriever:
  kind: file
  path: /goff/flags.yaml
`

// startRelayProxy runs a GO Feature Flag relay proxy serving relayFlagsYAML and
// returns its base URL.
func startRelayProxy(t *testing.T, defaultVariation string) string {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "gofeatureflag/go-feature-flag:v1.45.1",
		ExposedPorts: []string{"1031/tcp"},
		Files: []testcontainers.ContainerFile{
			{
				Reader:            stringReader(relayProxyYAML),
				ContainerFilePath: "/goff/goff-proxy.yaml",
				FileMode:          0o644,
			},
			{
				Reader:            stringReader(relayFlagsYAML(defaultVariation)),
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

// installRelayProvider is the wiring an application copies when it installs its
// own provider: build the GO Feature Flag provider against the relay proxy and
// bind it to the domain the modules resolve through. DataCollectorDisabled is
// required — see feature-flags.md — and the auto-install path hardcodes it for
// applications that set OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT instead of
// writing this.
//
// It runs BEFORE the connection is built, and must: relayPossible is resolved at
// construction, so a wrapper built earlier would resolve statically for its
// whole life and never consult the relay.
func installRelayProvider(t *testing.T, endpoint string) {
	t.Helper()
	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:              endpoint,
		DataCollectorDisabled: true,
	})
	require.NoError(t, err, "construct GO Feature Flag provider")
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, provider))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})
}

func TestRelayProxyDisablesTracingTheEnvironmentEnabled(t *testing.T) {
	installRelayProvider(t, startRelayProxy(t, "disabled"))

	// TestMain sets OTEL_NATS_TRACING_ENABLED=1 for this package, so a disabled
	// result can only come from the relay.
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.Publish(context.Background(), "relayflags.subject", []byte("payload")))

	assert.False(t, conn.TracingEnabled(),
		"relay serves otel-nats-tracing=false, which disables what OTEL_NATS_TRACING_ENABLED=1 deployed")
	assert.Empty(t, sr.Ended(),
		"no spans while the relay says off, even though the module env var says on")
}

// TestRelayProxyEnablesTracingTheEnvironmentLeftOff is the direction the
// superseded revoke-only model could not express, and the reason this revision
// exists. The module variable is explicitly falsy, so an enabled result can only
// come from the relay.
func TestRelayProxyEnablesTracingTheEnvironmentLeftOff(t *testing.T) {
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "false")
	installRelayProvider(t, startRelayProxy(t, "enabled"))

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.Publish(context.Background(), "relayflags.subject", []byte("payload")))

	assert.True(t, conn.TracingEnabled(),
		"relay serves otel-nats-tracing=true, which enables what the deployment left off")
	assert.NotEmpty(t, sr.Ended(),
		"a publish span is emitted even though OTEL_NATS_TRACING_ENABLED=false")
}

// stringReader adapts a config literal to the io.Reader testcontainers wants.
func stringReader(s string) io.Reader { return strings.NewReader(s) }
