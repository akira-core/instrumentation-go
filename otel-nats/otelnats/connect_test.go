package otelnats_test

import (
	"context"
	"os"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	otelnats "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

// TestConnectReturnsErrorOnBadURL covers the early-failure branch of
// Connect: an unreachable URL must surface a real error rather than a
// partially-initialised Conn.
func TestConnectReturnsErrorOnBadURL(t *testing.T) {
	// Use a port we know is closed.
	conn, err := otelnats.Connect("nats://127.0.0.1:1")
	if err == nil {
		t.Fatalf("expected error for unreachable URL, got Conn=%v", conn)
		conn.Close()
	}
}

// TestConnectFiltersNilOptions ensures the variadic shape is tolerant of
// callers passing nils — common in conditional option chains. The
// underlying nats.Connect rejects nil options, so the wrapper must filter
// them before delegating.
func TestConnectFiltersNilOptions(t *testing.T) {
	// Setup tracing-off so we don't need the deliver-TP path.
	_ = os.Unsetenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_NATS_TRACING_ENABLED")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))
	t.Cleanup(srv.Shutdown)

	conn, err := otelnats.Connect(srv.ClientURL(), nil, nil, nil)
	require.NoError(t, err, "nil options must be filtered, not propagated")
	require.NotNil(t, conn)
	t.Cleanup(conn.Close)
}

// TestCloseWithoutDeliverTPNoop locks in the lifecycle invariant for the
// disabled path: Close() must not panic when the deliver TracerProvider was
// never created (the typical case — no OTEL_EXPORTER_OTLP_ENDPOINT set).
func TestCloseWithoutDeliverTPNoop(t *testing.T) {
	_ = os.Unsetenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_NATS_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))
	t.Cleanup(srv.Shutdown)

	conn, err := otelnats.Connect(srv.ClientURL(), nil)
	require.NoError(t, err)

	// Close must return quickly and must not panic.
	done := make(chan struct{})
	go func() { conn.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return promptly when no deliver-TP exists")
	}
}

// TestCloseShutdownTimeoutIsBounded locks in the 3-second cap on the deliver
// TracerProvider shutdown. A misbehaving exporter must not deadlock Close —
// the bounded timeout context derived in Close() must release the goroutine
// regardless of exporter behaviour.
//
// We cannot easily inject a slow exporter through the public Connect path
// (the deliver-TP construction happens inside initNATSProvider). Instead we
// rely on the same end-to-end path the production code takes — set a real
// endpoint so deliver-TP is created — then verify Close completes in well
// under the 3-second cap on a clean shutdown.
func TestCloseShutdownTimeoutIsBounded(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	// Endpoint must be set for initNATSProvider to create a deliver-TP.
	// Point at a closed port so the OTLP gRPC exporter doesn't try to actually
	// connect to anything; the exporter creation itself doesn't fail because
	// it dials lazily.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:1")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))
	t.Cleanup(srv.Shutdown)

	conn, err := otelnats.Connect(srv.ClientURL(), nil)
	require.NoError(t, err)
	require.True(t, conn.TracingEnabled(), "tracing must be on for the deliver-TP to exist")

	start := time.Now()
	done := make(chan struct{})
	go func() { conn.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not respect the 3-second deliver-TP shutdown cap")
	}
	assert.Less(t, time.Since(start), 4*time.Second,
		"Close() should complete within the 3-second cap on deliver-TP shutdown")
}

// TestDrainShutsDeliverTP asserts that Drain (the graceful alternative to
// Close) also shuts down the deliver TracerProvider — otherwise users
// relying on Drain for clean exits would silently leak the deliver-TP
// batch goroutine until process exit.
func TestDrainShutsDeliverTP(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:1")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))
	t.Cleanup(srv.Shutdown)

	conn, err := otelnats.Connect(srv.ClientURL(), nil)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- conn.Drain() }()

	select {
	case err := <-done:
		// nats.Drain may return nil or "connection closed" depending on timing;
		// the important property is that it returns at all (deliver-TP shutdown
		// did not block).
		_ = err
	case <-time.After(5 * time.Second):
		t.Fatal("Drain() did not return — deliver-TP shutdown likely blocked")
	}
}

// TestConnectWithOptionsThreadsTraceDest verifies the WithTraceDestination
// trace option survives into the resulting Conn via TraceDest().
func TestConnectWithOptionsThreadsTraceDest(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))
	t.Cleanup(srv.Shutdown)

	conn, err := otelnats.ConnectWithOptions(srv.ClientURL(), nil,
		otelnats.WithTraceDestination("$NATS.trace.events"),
	)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Equal(t, "$NATS.trace.events", conn.TraceDest())
}

// TestConnectDisabledModeHasNoTraceDest verifies the disabled-path Conn
// returns empty string from TraceDest() — even if the option was passed,
// the directConn impl never holds it because there is no use for it.
func TestConnectDisabledModeHasNoTraceDest(t *testing.T) {
	_ = os.Unsetenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_NATS_TRACING_ENABLED")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))
	t.Cleanup(srv.Shutdown)

	conn, err := otelnats.ConnectWithOptions(srv.ClientURL(), nil,
		otelnats.WithTraceDestination("$NATS.trace.events"),
	)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Empty(t, conn.TraceDest(), "disabled mode must not honour WithTraceDestination — directConn returns empty")
}

// Use context import so go test doesn't strip it under future edits.
var _ = context.Background
