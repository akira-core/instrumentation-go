package otelmongo

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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
	// startDeliverSpan now lives on tracedCollection (the only impl that creates
	// deliver spans). When deliverTracer is nil the helper must return ctx unchanged.
	impl := &tracedCollection{deliverTracer: nil}
	ctx := context.Background()
	got, span := impl.startDeliverSpan(ctx, "testdb", "testcoll")
	defer span.End()
	if got != ctx {
		t.Error("expected unchanged ctx when deliverTracer is nil")
	}
	if trace.SpanFromContext(got).SpanContext().IsValid() {
		t.Error("expected no span in ctx when deliverTracer is nil")
	}
}

func TestStartDeliverSpanEnabledV2(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	tracer := tp.Tracer(ScopeName)
	impl := &tracedCollection{
		deliverTracer: tracer,
		serverAddr:    "localhost",
		serverPort:    27017,
	}

	parentCtx, parentSpan := tp.Tracer(ScopeName).Start(context.Background(), "parent")
	defer parentSpan.End()

	got, deliverSpan := impl.startDeliverSpan(parentCtx, "testdb", "testcoll")
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
