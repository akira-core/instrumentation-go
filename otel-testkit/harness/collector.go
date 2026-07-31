package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// defaultCollectorImage is the OpenTelemetry Collector (core) image used to
// receive spans from the SDK and re-export them to the in-process sink.
// Override with OTEL_COLLECTOR_IMAGE.
const defaultCollectorImage = "otel/opentelemetry-collector:0.147.0"

// collectorConfig is the collector pipeline: OTLP/gRPC in, OTLP/gRPC out to the
// host sink (reachable via host.testcontainers.internal), plus a debug exporter.
const collectorConfigTmpl = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
exporters:
  otlp/sink:
    endpoint: host.testcontainers.internal:%d
    tls:
      insecure: true
  debug:
    verbosity: basic
service:
  telemetry:
    logs:
      level: warn
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/sink, debug]
`

// StartCollector launches an OTel Collector container that receives OTLP/gRPC
// on 4317 and forwards every span to the host sink on sinkPort. It returns the
// host-mapped OTLP endpoint (host:port) for SDK exporters to target, and
// registers termination via t.Cleanup.
func StartCollector(ctx context.Context, t *testing.T, sinkPort int) string {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(collectorConfigTmpl, sinkPort)), 0o600); err != nil {
		t.Fatalf("write collector config: %v", err)
	}

	image := os.Getenv("OTEL_COLLECTOR_IMAGE")
	if image == "" {
		image = defaultCollectorImage
	}

	req := testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"4317/tcp"},
		Cmd:          []string{"--config=/etc/otelcol/config.yaml"},
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      cfgPath,
			ContainerFilePath: "/etc/otelcol/config.yaml",
			FileMode:          0o644,
		}},
		HostAccessPorts: []int{sinkPort},
		WaitingFor:      wait.ForListeningPort("4317/tcp").WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start collector container: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = container.Terminate(cctx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("collector host: %v", err)
	}
	mapped, err := container.MappedPort(ctx, "4317/tcp")
	if err != nil {
		t.Fatalf("collector mapped port: %v", err)
	}
	return fmt.Sprintf("%s:%s", host, mapped.Port())
}
