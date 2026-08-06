package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	otelgorillaws "github.com/akira-core/instrumentation-go/otel-gorilla-ws"
)

// These tests stand up a real GO Feature Flag relay proxy in front of a real
// WebSocket handshake. Flag RESOLUTION is unit-tested with an in-memory
// provider; what only a real relay can prove is that the wiring recipe the
// documentation tells applications to copy — provider options, endpoint format,
// flag keys matching a real relay configuration file — actually decides this
// module's switches.
//
// otel-nats carries the equivalent suite for a per-operation gate. This module
// is covered separately because its switch decides something no other module's
// does: whether otel-ws is offered and confirmed during the HANDSHAKE, which
// fixes the wire format for the connection's whole life and cannot be revisited.
// A relay that resolved a moment too late would not merely lose a span — it
// would leave one side wrapping frames the other parses raw. So the assertion
// here is on the bytes of the negotiation, not on a boolean.

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
// It must run BEFORE the handshake: negotiation is decided once, immediately
// before it, and cannot be revisited afterwards.
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

// newRelayTP installs a recording TracerProvider and the W3C propagator.
//
// It deliberately does NOT touch the environment, unlike newIntegrationTP:
// which module variable is set is the very thing under test here, and a helper
// that quietly forces one on would make the enable direction unassertable.
func newRelayTP(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
	return recorder
}

// relayEchoServer starts an otel-ws-aware server and reports, per connection,
// what the client OFFERED in Sec-WebSocket-Protocol — the observable that says
// whether the dialer consulted the relay before the handshake. It echoes one
// message back so the round trip can be asserted on payload bytes.
//
// The handler runs on the server's goroutine; the offer travels over a buffered
// channel rather than a shared variable so the read is ordered and race-free.
func relayEchoServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	offered := make(chan string, 1)

	up := &otelgorillaws.Upgrader{
		CheckOrigin:  func(*http.Request) bool { return true },
		Subprotocols: []string{appProtocol},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case offered <- r.Header.Get("Sec-WebSocket-Protocol"):
		default:
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		ctx, typ, payload, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(ctx, typ, payload)
	}))
	t.Cleanup(srv.Close)
	return srv, offered
}

// appProtocol is the application subprotocol every dial here offers.
//
// It must be non-empty: Dial injects otel-ws only when the caller asked for at
// least one subprotocol of its own (Scenario E in otel-ws.md). Dialing with a
// nil list would make every assertion below vacuous — the negotiation would be
// absent for a reason that has nothing to do with the relay.
const appProtocol = "json"

func wsScheme(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// awaitOffer reads the subprotocol header the client sent, failing rather than
// blocking forever if the handler never ran.
func awaitOffer(t *testing.T, offered <-chan string) string {
	t.Helper()
	select {
	case v := <-offered:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("server handler never ran")
		return ""
	}
}

// TestRelayProxyDisablesNegotiationAndSpans is the revoke direction end-to-end.
// The deployment turned this module on, and one relay configuration file stops
// the offer, the confirmation and the spans — leaving a plain gorilla
// connection whose payload crosses the wire untouched.
func TestRelayProxyDisablesNegotiationAndSpans(t *testing.T) {
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	useRelay(t, map[string]bool{"otel-gorilla-ws-tracing": false})

	recorder := newRelayTP(t)
	srv, offered := relayEchoServer(t)

	conn, resp, err := otelgorillaws.Dial(context.Background(), wsScheme(srv), nil, []string{appProtocol})
	require.NoError(t, err)
	defer conn.Close()

	assert.NotContains(t, awaitOffer(t, offered), "otel-ws",
		"the relay serves otel-gorilla-ws-tracing=false, so the dialer must not offer otel-ws even though OTEL_GORILLA_WS_TRACING_ENABLED=1")
	assert.NotContains(t, resp.Header.Get("Sec-WebSocket-Protocol"), "otel-ws",
		"and the server must not confirm it")

	payload := []byte(`{"kind":"relay-off"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, payload, got, "an un-negotiated connection carries the payload verbatim")
	assert.Empty(t, recorder.Ended(), "no spans while the relay says off")
}

// TestRelayProxyEnablesNegotiationTheDeploymentLeftOff is the direction the
// superseded revoke-only model could not express. The module variable is
// explicitly falsy, so an otel-ws handshake can only be the relay's doing.
func TestRelayProxyEnablesNegotiationTheDeploymentLeftOff(t *testing.T) {
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "false")
	useRelay(t, map[string]bool{"otel-gorilla-ws-tracing": true})

	recorder := newRelayTP(t)
	srv, offered := relayEchoServer(t)

	conn, resp, err := otelgorillaws.Dial(context.Background(), wsScheme(srv), nil, []string{appProtocol})
	require.NoError(t, err)
	defer conn.Close()

	assert.Contains(t, awaitOffer(t, offered), "otel-ws",
		"the relay enabled a module the deployment left off, so otel-ws is offered")
	assert.Contains(t, resp.Header.Get("Sec-WebSocket-Protocol"), "otel-ws",
		"and the server confirms it")

	payload := []byte(`{"kind":"relay-on"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, payload, got,
		"the envelope is an internal wire format: both sides speak it, the application still sees its own bytes")
	assert.NotEmpty(t, recorder.Ended(), "and spans are emitted")
}

// TestRelayProxyMasterVetoStopsNegotiation is the process-wide kill switch in
// its relay spelling: one key, and this module neither negotiates nor traces,
// however enthusiastic its own key and its own environment variable are.
func TestRelayProxyMasterVetoStopsNegotiation(t *testing.T) {
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	useRelay(t, map[string]bool{
		otelflags.FlagKeyGlobalTracing: false,
		"otel-gorilla-ws-tracing":      true,
	})

	recorder := newRelayTP(t)
	srv, offered := relayEchoServer(t)

	conn, resp, err := otelgorillaws.Dial(context.Background(), wsScheme(srv), nil, []string{appProtocol})
	require.NoError(t, err)
	defer conn.Close()

	assert.NotContains(t, awaitOffer(t, offered), "otel-ws",
		"the master key vetoes a module both its own key and its environment enable")
	assert.NotContains(t, resp.Header.Get("Sec-WebSocket-Protocol"), "otel-ws")

	payload := []byte(`{"kind":"master-veto"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.Empty(t, recorder.Ended(), "and no span is created")
}

// TestRelayProxyMissingFlagLeavesTheEnvironmentInCharge: the relay is up and
// healthy but does not define this module's key — the ordinary state of a relay
// carrying only the master kill switch. FLAG_NOT_FOUND must fall through to the
// environment, which says on.
func TestRelayProxyMissingFlagLeavesTheEnvironmentInCharge(t *testing.T) {
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	useRelay(t, map[string]bool{"some-unrelated-flag": false})

	recorder := newRelayTP(t)
	srv, offered := relayEchoServer(t)

	conn, _, err := otelgorillaws.Dial(context.Background(), wsScheme(srv), nil, []string{appProtocol})
	require.NoError(t, err)
	defer conn.Close()

	assert.Contains(t, awaitOffer(t, offered), "otel-ws",
		"the relay has no opinion, so OTEL_GORILLA_WS_TRACING_ENABLED=1 stands")

	payload := []byte(`{"kind":"no-key"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, payload))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.NotEmpty(t, recorder.Ended())
}
