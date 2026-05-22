package otelmongo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

func TestMongoDeliverSpanDisabledWithoutEndpointV2(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tp, tracer := initMongoProvider("localhost", 27017)
	if tp != nil {
		t.Error("expected nil TracerProvider when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
	}
	if tracer != nil {
		t.Error("expected nil Tracer when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
	}
}

func TestMongoDeliverSpanEnabledWithEndpointV2(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	tp, tracer := initMongoProvider("localhost", 27017)
	if tp == nil {
		t.Error("expected non-nil TracerProvider when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}
	if tracer == nil {
		t.Error("expected non-nil Tracer when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}
	if tp != nil {
		tp.Shutdown(t.Context()) //nolint:errcheck
	}
}

func TestMongoServiceNameV2(t *testing.T) {
	cases := []struct {
		addr string
		port int
		want string
	}{
		{"", 0, "mongodb"},
		{"localhost", 27017, "mongodb://localhost"},
		{"localhost", 27018, "mongodb://localhost:27018"},
		{"myhost", 0, "mongodb://myhost"},
	}
	for _, tc := range cases {
		got := mongoServiceName(tc.addr, tc.port)
		if got != tc.want {
			t.Errorf("mongoServiceName(%q, %d) = %q, want %q", tc.addr, tc.port, got, tc.want)
		}
	}
}

func TestStartDeliverSpanDisabledV2(t *testing.T) {
	// StartDeliverSpan now lives on *traced.Collection (the only impl that creates
	// deliver spans). When DeliverTracer is nil the helper must return ctx unchanged.
	impl := &traced.Collection{DeliverTracer: nil}
	ctx := context.Background()
	got, span := impl.StartDeliverSpan(ctx, "testdb", "testcoll")
	defer span.End()
	if got != ctx {
		t.Error("expected unchanged ctx when DeliverTracer is nil")
	}
	if trace.SpanFromContext(got).SpanContext().IsValid() {
		t.Error("expected no span in ctx when DeliverTracer is nil")
	}
}

func TestStartDeliverSpanEnabledV2(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	tracer := tp.Tracer(ScopeName)
	impl := &traced.Collection{
		DeliverTracer: tracer,
		ServerAddr:    "localhost",
		ServerPort:    27017,
	}

	parentCtx, parentSpan := tp.Tracer(ScopeName).Start(context.Background(), "parent")
	defer parentSpan.End()

	got, deliverSpan := impl.StartDeliverSpan(parentCtx, "testdb", "testcoll")
	defer deliverSpan.End()

	deliverSC := trace.SpanFromContext(got).SpanContext()
	if !deliverSC.IsValid() {
		t.Fatal("expected valid span context in returned ctx")
	}
	parentSC := parentSpan.SpanContext()
	if deliverSC.SpanID() == parentSC.SpanID() {
		t.Error("deliver span ID should differ from parent span ID")
	}
	if deliverSC.TraceID() != parentSC.TraceID() {
		t.Error("deliver span should share trace ID with parent")
	}
	// span should still be recording — caller has not yet called End()
	if ro, ok := deliverSpan.(interface{ IsRecording() bool }); ok {
		if !ro.IsRecording() {
			t.Error("deliver span should still be recording before End() is called")
		}
	}
}

// TestShutdownDeliverNilMongoTPNoopV2 — locks in that ShutdownDeliver is a
// no-op when no deliver TracerProvider was created (i.e. OTEL_EXPORTER_OTLP_ENDPOINT
// was unset at Connect time).
func TestShutdownDeliverNilMongoTPNoopV2(t *testing.T) {
	state := &traced.ClientState{}
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

func TestShutdownDeliverShutsRealProviderV2(t *testing.T) {
	sp := &shutdownObservingProcessorV2{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sp))
	state := &traced.ClientState{MongoTP: tp}

	state.ShutdownDeliver(context.Background())

	assert.True(t, sp.shutdownCalled, "expected MongoTP.Shutdown to be invoked")
}

func TestShutdownDeliverHonoursTimeoutV2(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	sp := &shutdownObservingProcessorV2{block: 5 * time.Second}
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

type shutdownObservingProcessorV2 struct {
	shutdownCalled bool
	block          time.Duration
}

func (p *shutdownObservingProcessorV2) OnStart(_ context.Context, _ sdktrace.ReadWriteSpan) {}
func (p *shutdownObservingProcessorV2) OnEnd(_ sdktrace.ReadOnlySpan)                       {}
func (p *shutdownObservingProcessorV2) ForceFlush(_ context.Context) error                  { return nil }
func (p *shutdownObservingProcessorV2) Shutdown(ctx context.Context) error {
	p.shutdownCalled = true
	if p.block > 0 {
		select {
		case <-time.After(p.block):
		case <-ctx.Done():
		}
	}
	return nil
}

// TestTracedStateShutdownDeliverInvokesProcessorShutdownV2 — locks in the inner
// call only; the wiring Client.Disconnect → c.traced.ShutdownDeliver lives at
// client.go:178–183 and is enforced by inspection.
func TestTracedStateShutdownDeliverInvokesProcessorShutdownV2(t *testing.T) {
	sp := &shutdownObservingProcessorV2{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sp))

	state := &traced.ClientState{MongoTP: tp}
	state.ShutdownDeliver(context.Background())

	assert.True(t, sp.shutdownCalled, "ShutdownDeliver must invoke MongoTP.Shutdown")
}

// TestClientDisabledPathHasNoDeliverStateV2 mirrors the v1 sibling: with all
// three Mongo tracing env vars off, mongoTracingEnabled() must resolve false
// so the Connect path takes the early-return branch that constructs
// Client{traced: nil}. The full Connect path requires a live mongod and is
// covered by TestConnectGlobalOff_ZeroWrapperSpans in collection_test.go.
func TestClientDisabledPathHasNoDeliverStateV2(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "0")
	t.Setenv(envMongoTracingEnabled, "0")
	t.Setenv(envMongoPropagationEnabled, "0")
	resetPropEnabledCacheForTest()
	t.Cleanup(resetPropEnabledCacheForTest)

	if mongoTracingEnabled() {
		t.Fatal("tracing gate must resolve false with all envs explicitly off")
	}
}
