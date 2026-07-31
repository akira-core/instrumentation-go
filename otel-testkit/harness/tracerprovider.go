package harness

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// BuildTracerProvider creates a TracerProvider whose OTLP/gRPC exporter targets
// the collector at endpoint, with the given service.name and sampler. It uses a
// synchronous span processor (WithSyncer) so sampling decisions are observable
// immediately. Shutdown is registered via t.Cleanup.
func BuildTracerProvider(t *testing.T, serviceName string, sampler sdktrace.Sampler, endpoint string) *sdktrace.TracerProvider {
	t.Helper()

	ctx := context.Background()
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("create otlp exporter for %s: %v", serviceName, err)
	}

	res := resource.NewSchemaless(attribute.String("service.name", serviceName))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	})
	return tp
}
