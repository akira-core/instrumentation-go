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

// startRelayProxyWithFlags runs a GO Feature Flag relay proxy serving flagsYAML
// and returns its base URL plus the container, for tests that rewrite the flag
// file mid-run.
func startRelayProxyWithFlags(t *testing.T, flagsYAML string) (string, testcontainers.Container) {
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
				Reader:            stringReader(flagsYAML),
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
	return fmt.Sprintf("http://%s:%s", host, port.Port()), c
}

// startRelayProxy runs a GO Feature Flag relay proxy serving relayFlagsYAML and
// returns its base URL.
func startRelayProxy(t *testing.T, defaultVariation string) string {
	t.Helper()
	url, _ := startRelayProxyWithFlags(t, relayFlagsYAML(defaultVariation))
	return url
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

// --- fallback cases against a real relay ------------------------------------
//
// The unit suite proves every fallback with an in-memory provider; these four
// prove the same contract through the real GO Feature Flag provider and a real
// relay, where the failure is produced by the relay's own configuration rather
// than by a hand-written stub.

// TestRelayProxyMissingFlagLeavesTheEnvironmentInCharge: the relay is up and
// healthy but simply does not define otel-nats-tracing — the ordinary state of
// a relay that only carries the master kill switch. FLAG_NOT_FOUND must fall
// through to the environment, which TestMain set to on.
func TestRelayProxyMissingFlagLeavesTheEnvironmentInCharge(t *testing.T) {
	url, _ := startRelayProxyWithFlags(t, `some-unrelated-flag:
  variations:
    enabled: true
    disabled: false
  defaultRule:
    variation: disabled
`)
	installRelayProvider(t, url)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.Publish(context.Background(), "relayflags.subject", []byte("payload")))

	assert.True(t, conn.TracingEnabled(),
		"the relay does not define otel-nats-tracing, so OTEL_NATS_TRACING_ENABLED=1 must stand")
	assert.NotEmpty(t, sr.Ended(),
		"spans must be emitted when the relay has no opinion and the environment says on")
}

// TestRelayProxyUnreachableLeavesTheEnvironmentInCharge: the endpoint is real
// wiring against a relay that no longer exists — the container is terminated
// before the provider is bound, and the bind is the same non-blocking
// SetNamedProvider the zero-code auto-install performs. Every evaluation fails
// with a provider that never completed a fetch, and the environment must stand.
func TestRelayProxyUnreachableLeavesTheEnvironmentInCharge(t *testing.T) {
	ctx := context.Background()
	url, c := startRelayProxyWithFlags(t, relayFlagsYAML("disabled"))
	require.NoError(t, c.Terminate(ctx), "terminate the relay before anything fetches from it")

	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:              url,
		DataCollectorDisabled: true,
	})
	require.NoError(t, err, "provider construction performs no I/O and must not fail on a dead endpoint")
	require.NoError(t, openfeature.SetNamedProvider(otelflags.FlagDomain, provider))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.Publish(context.Background(), "relayflags.subject", []byte("payload")))

	assert.True(t, conn.TracingEnabled(),
		"the relay is unreachable, so its disabled flag must never arrive and the environment must stand")
	assert.NotEmpty(t, sr.Ended(),
		"spans must be emitted while the relay is down and the environment says on")
}

// TestRelayProxyWrongTypeLeavesTheEnvironmentInCharge: the relay defines the
// key with STRING variations. The provider resolves it, the SDK rejects the
// type, and TYPE_MISMATCH must fall through to the environment — a relay
// misconfiguration must never decide a switch.
func TestRelayProxyWrongTypeLeavesTheEnvironmentInCharge(t *testing.T) {
	url, _ := startRelayProxyWithFlags(t, `otel-nats-tracing:
  variations:
    enabled: "definitely"
    disabled: "nope"
  defaultRule:
    variation: disabled
`)
	installRelayProvider(t, url)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	assert.True(t, conn.TracingEnabled(),
		"the relay serves otel-nats-tracing as a string, so the value is unusable and OTEL_NATS_TRACING_ENABLED=1 must stand")
}

// TestRelayProxyChangeReachesALiveConnection is the dynamic-flag promise
// end-to-end: flip the flag on a running relay and a connection built earlier
// observes it, without a restart and without a reset hook, through the real
// polling chain — provider poll (1 s here) on top of the relay's own file poll
// (1 s) — rather than through a provider rebind, which is how the unit suite
// models a change.
func TestRelayProxyChangeReachesALiveConnection(t *testing.T) {
	ctx := context.Background()
	url, c := startRelayProxyWithFlags(t, relayFlagsYAML("enabled"))

	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:                  url,
		DataCollectorDisabled:     true,
		FlagChangePollingInterval: time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, provider))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.True(t, conn.TracingEnabled(), "the relay serves enabled before the flip")

	// The operator flips the flag on the relay: rewrite the flag file inside the
	// running container. The relay re-reads it within its pollingInterval, the
	// provider fetches within FlagChangePollingInterval.
	require.NoError(t, c.CopyToContainer(ctx,
		[]byte(relayFlagsYAML("disabled")), "/goff/flags.yaml", 0o644))

	assert.Eventually(t, func() bool { return !conn.TracingEnabled() },
		20*time.Second, 250*time.Millisecond,
		"a live connection must observe the relay change within the polling chain; nothing may cache the old value")
}

// stringReader adapts a config literal to the io.Reader testcontainers wants.
func stringReader(s string) io.Reader { return strings.NewReader(s) }
