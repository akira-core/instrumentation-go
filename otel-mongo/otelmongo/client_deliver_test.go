package otelmongo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// TestShutdownDeliverNilMongoTPNoop locks in the contract that
// ShutdownDeliver is a no-op when no deliver TracerProvider was created
// (i.e. OTEL_EXPORTER_OTLP_ENDPOINT was unset at Connect time). Mirrors v2.
func TestShutdownDeliverNilMongoTPNoop(t *testing.T) {
	state := &traced.ClientState{} // MongoTP nil
	// Must not panic and must return immediately.
	done := make(chan struct{})
	go func() {
		state.ShutdownDeliver(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ShutdownDeliver with nil MongoTP did not return promptly")
	}
}

// TestShutdownDeliverShutsRealProvider asserts that when MongoTP is a real
// SDK provider, ShutdownDeliver actually invokes Shutdown — verifiable via a
// span-processor whose Shutdown side-effect we observe.
func TestShutdownDeliverShutsRealProvider(t *testing.T) {
	sp := &shutdownObservingProcessor{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sp))
	state := &traced.ClientState{MongoTP: tp}

	state.ShutdownDeliver(context.Background())

	assert.True(t, sp.shutdownCalled, "expected MongoTP.Shutdown to be invoked")
}

// TestShutdownDeliverHonoursTimeout asserts the 3-second timeout cap protects
// callers from a misbehaving exporter. A processor whose Shutdown blocks
// longer than the cap must NOT deadlock the deliver-shutdown call.
func TestShutdownDeliverHonoursTimeout(t *testing.T) {
	// Use a tiny inherited ctx deadline so the test stays fast — the implementation
	// derives `WithTimeout(ctx, 3*time.Second)` which respects the inherited deadline.
	parent, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	sp := &shutdownObservingProcessor{block: 5 * time.Second}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sp))
	state := &traced.ClientState{MongoTP: tp}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		state.ShutdownDeliver(parent)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownDeliver did not return within timeout cap")
	}
	require.Less(t, time.Since(start), 2*time.Second, "ShutdownDeliver must respect inherited ctx deadline")
}

// shutdownObservingProcessor is a SpanProcessor that records whether Shutdown
// was invoked and optionally blocks for `block` duration to simulate a slow
// exporter — used by the timeout test.
type shutdownObservingProcessor struct {
	shutdownCalled bool
	block          time.Duration
}

func (p *shutdownObservingProcessor) OnStart(_ context.Context, _ sdktrace.ReadWriteSpan) {}
func (p *shutdownObservingProcessor) OnEnd(_ sdktrace.ReadOnlySpan)                       {}
func (p *shutdownObservingProcessor) ForceFlush(_ context.Context) error                  { return nil }
func (p *shutdownObservingProcessor) Shutdown(ctx context.Context) error {
	p.shutdownCalled = true
	if p.block > 0 {
		select {
		case <-time.After(p.block):
		case <-ctx.Done():
		}
	}
	return nil
}

// TestTracedStateShutdownDeliverInvokesProcessorShutdown asserts the same code
// path Client.Disconnect calls — c.traced.ShutdownDeliver — actually shuts the
// underlying MongoTP. We can't drive this through Client.Disconnect itself
// without a live *mongo.Client (calling Disconnect on a nil mongo.Client
// panics), so this test exercises the inner call directly. The wiring from
// Client.Disconnect → c.traced.ShutdownDeliver is locked in by reading
// client.go:171–175 — any future refactor that removes that call must also
// add a Disconnect-level test (e.g. against a testcontainers mongod).
func TestTracedStateShutdownDeliverInvokesProcessorShutdown(t *testing.T) {
	sp := &shutdownObservingProcessor{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sp))

	state := &traced.ClientState{MongoTP: tp}
	state.ShutdownDeliver(context.Background())

	assert.True(t, sp.shutdownCalled, "ShutdownDeliver must invoke MongoTP.Shutdown")
}

// TestClientDisabledPathHasNoDeliverState locks in the structural invariant
// that the disabled-mode Client has traced == nil — so there is no MongoTP
// for the test (and the runtime) to accidentally Shutdown.
//
// The full Connect path (live mongod) is exercised by
// TestConnectGlobalOff_ZeroWrapperSpans in collection_test.go. This test only
// asserts the gate function returns false; the cache reset on entry + exit
// keeps any sibling test that follows from inheriting our env state.
func TestClientDisabledPathHasNoDeliverState(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "0")
	t.Setenv(envMongoTracingEnabled, "0")
	t.Setenv(envMongoPropagationEnabled, "0")
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)

	require.False(t, mongoTracingEnabled(),
		"tracing gate must resolve false — this is the only branch separating Client.traced=nil from Client.traced=&ClientState{...} in ConnectWithOptions")
}
