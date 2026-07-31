package harness

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TelemetryEnv bundles the telemetry plumbing a sampling test needs: the
// in-process sink to assert on and the OTLP endpoint each service's
// TracerProvider exports to.
type TelemetryEnv struct {
	Sink     *Sink
	Endpoint string
}

// envConfig holds the resolved StartTelemetryEnv options.
type envConfig struct {
	propagator    propagation.TextMapPropagator
	dumpOnFailure bool
	insecureOTLP  bool
}

// EnvOption customizes StartTelemetryEnv.
type EnvOption func(*envConfig)

// WithDumpOnFailure toggles the automatic span dump on test failure (default on).
func WithDumpOnFailure(on bool) EnvOption {
	return func(c *envConfig) { c.dumpOnFailure = on }
}

// WithPropagator overrides the global text-map propagator the env installs
// (default: composite of W3C TraceContext + Baggage).
func WithPropagator(p propagation.TextMapPropagator) EnvOption {
	return func(c *envConfig) { c.propagator = p }
}

// WithInsecureOTLP also sets OTEL_EXPORTER_OTLP_INSECURE=true. Use it when the
// instrumented library spins up its own OTLP exporter (e.g. a deliver-span
// TracerProvider) that reads OTEL_EXPORTER_OTLP_ENDPOINT and needs the plaintext
// transport the test collector speaks.
func WithInsecureOTLP() EnvOption {
	return func(c *envConfig) { c.insecureOTLP = true }
}

// StartTelemetryEnv wires up a complete sampling-test environment in one call: it
// installs a text-map propagator, starts the in-process sink (with DumpOnFailure),
// launches the collector container, and points OTEL_EXPORTER_OTLP_ENDPOINT at it.
// Build each service's TracerProvider against the returned Endpoint and assert on
// the returned Sink. All teardown is registered via t.Cleanup.
func StartTelemetryEnv(t *testing.T, opts ...EnvOption) TelemetryEnv {
	t.Helper()
	cfg := envConfig{
		propagator: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}),
		dumpOnFailure: true,
	}
	for _, o := range opts {
		o(&cfg)
	}

	otel.SetTextMapPropagator(cfg.propagator)
	sink := StartSink(t)
	if cfg.dumpOnFailure {
		DumpOnFailure(t, sink)
	}
	endpoint := StartCollector(context.Background(), t, sink.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	if cfg.insecureOTLP {
		t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	}
	return TelemetryEnv{Sink: sink, Endpoint: endpoint}
}
