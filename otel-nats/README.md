# otel-nats (otelnats + oteljetstream)

[繁體中文 (Traditional Chinese)](README.zh-TW.md)

---

OpenTelemetry tracing for [NATS](https://nats.io/) and [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream), aligned with the official `nats.go` / `nats.go/jetstream` APIs. Propagates W3C Trace Context in message headers. `oteljetstream` now fully wraps JetStream consumer management APIs (`StreamConsumerManager` on `JetStream` and `ConsumerManager` on `Stream`) while keeping message publish/consume tracing behavior unchanged. Per [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation): packages accept **TracerProvider** and **Propagators** via options; they do **not** provide InitTracer. Set the global provider and propagator at process startup (see **examples/**).

---

## Layout

```
otel-nats/
├── otelnats/           # Core NATS: Connect, Conn, Publish, Subscribe, HeaderCarrier
│   ├── connect.go      # Connect, ConnectWithOptions, ConnectTLS, ConnectWithCredentials
│   ├── conn.go         # Conn, Publish, PublishMsg, Subscribe, QueueSubscribe, WithTracerProvider, WithPropagators
│   ├── propagation.go  # HeaderCarrier (nats.Header ↔ TextMapCarrier)
│   └── doc.go
├── oteljetstream/      # JetStream: New, JetStream, Stream, Consumer, PushConsumer, full consumer-manager wrappers, Consume, Messages, Fetch
│   ├── jetstream.go    # New(conn), Publish, CreateOrUpdateStream
│   ├── stream.go       # Stream, Consumer/PushConsumer, CreateOrUpdateConsumer/CreateOrUpdatePushConsumer
│   ├── consumer.go     # Consume, Messages, Fetch, MessageBatch (Messages), Msg
│   └── doc.go
├── examples/            # How to create TracerProvider + set global + use otelnats/oteljetstream
├── go.mod
└── README.md
```

---

## Usage

### Tracing feature flags

`otel-nats` (`otelnats` + `oteljetstream`) reads three env vars; **all default to OFF when unset**:

| Variable | Tier | Default | Effect |
|---|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master | OFF | hard prerequisite for every per-module flag |
| `OTEL_NATS_TRACING_ENABLED` | module tracing | OFF | wrapper spans on Publish / Subscribe / Request and the JetStream consumer paths |
| `OTEL_NATS_PROPAGATION_ENABLED` | module propagation | OFF | W3C `traceparent` / `tracestate` header inject on publish + extract on subscribe |

Truthy = any value other than `0`, `false`, `no`, `off` (case-insensitive, whitespace-trimmed). Cached for the process lifetime via `sync.Once`; env changes after the first gate read are ignored.

**Four observable states:**

| `OTEL_NATS_TRACING_ENABLED` | `OTEL_NATS_PROPAGATION_ENABLED` | Wrapper span | Wire `traceparent` |
|---|---|---|---|
| OFF (any) | (any) | — | — |
| ON | OFF (default) | yes | **no** |
| ON | ON | yes | yes |
| ON | falsy | yes | no |

When tracing is on but propagation is off (the new default), wrapper spans are still created — useful for local fan-out metrics / dead-letter diagnosis — but no headers are injected on publish and no Extract is performed on subscribe. Useful when the downstream consumer is non-OTel-aware or wire size dominates small payloads.

#### Propagation flag (env-var change from v0.3.x)

Deployments that previously enabled tracing without setting an explicit propagation flag **must add** `OTEL_NATS_PROPAGATION_ENABLED=true` to keep `traceparent` injection working — otherwise cross-service traces will fragment at every NATS hop. Find affected configs:

```bash
grep -rE 'OTEL_NATS_TRACING_ENABLED' deploy/ config/ docker-compose*.yml
```

For each match that also enables tracing, add the propagation env var alongside it. See `CHANGELOG.md` for before/after wire-output examples.

Per-connection overrides via `ConnectWithOptions(..., WithTracerProvider(tp), WithPropagators(p))` are still available; they have no effect when tracing is gated off.

### 1. Initialize provider and propagator (application responsibility)

Create a TracerProvider (e.g. OTLP) and set the global provider and propagator once at startup. See **examples/main.go** for a full runnable.

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// In main:
tp, err := newTracerProvider() // create with OTLP exporter + resource
if err != nil { log.Fatal(err) }
defer func() { _ = tp.Shutdown(ctx) }()

otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},
    propagation.Baggage{},
))
```

### 2. Core NATS: Connect, Publish, Subscribe

```go
import (
    "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

conn, err := otelnats.Connect(natsURL, nil)
if err != nil { log.Fatal(err) }
defer conn.Close()

conn.Publish(ctx, "subject", []byte("data"))
conn.Subscribe("subject", func(m otelnats.Msg) {
    // m.Msg, m.Context() — trace from headers in m.Context()
})
conn.QueueSubscribe("subject", "queue", handler)
```

Optional: pass **WithTracerProvider(tp)** or **WithPropagators(p)** to **ConnectWithOptions** for per-connection overrides.

### 3. JetStream

```go
import (
    "github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
    "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

conn, _ := otelnats.Connect(natsURL, nil)
defer conn.Close()

js, err := oteljetstream.New(conn)
// After creating stream/consumer:
cons.Consume(func(m oteljetstream.Msg) {
    // m.Data(), m.Ack(), m.Context() — trace from message headers
})

// Push consumer is also wrapped:
pushCons, _ := js.CreateOrUpdatePushConsumer(ctx, "MYSTREAM", oteljetstream.ConsumerConfig{
    Durable:        "push-consumer",
    DeliverSubject: "push.deliver",
    FilterSubject:  "events.push",
})
pushCons.Consume(func(m oteljetstream.Msg) {
    // same trace extraction behavior
    _ = m.Ack()
})
```

### 4. Tests

Set the global provider (and optionally propagator) before Connect; no InitTracer.

```go
otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(prop) // if testing propagation
conn, err := otelnats.Connect(url, nil)
```

---

## API summary

| Item | Description |
|------|-------------|
| **Connect** | `Connect(url string, natsOpts ...nats.Option)`. Uses `otel.GetTracerProvider()` and `otel.GetTextMapPropagator()` unless overridden via ConnectWithOptions. |
| **ConnectWithOptions** | Same with optional **WithTracerProvider(tp)** and **WithPropagators(p)**. |
| **ConnectTLS** | `ConnectTLS(url, certFile, keyFile, caFile string, natsOpts ...nats.Option)`. Connects with mutual TLS. |
| **ConnectWithCredentials** | `ConnectWithCredentials(url, credFile string, natsOpts ...nats.Option)`. Connects with JWT/NKey credentials. |
| **ScopeName / Version()** | Used when creating Tracer (OTel contrib guideline). |
| **JetStream consumer managers** | `JetStream` fully wraps `StreamConsumerManager`; `Stream` fully wraps `ConsumerManager`. Methods returning `Consumer`/`PushConsumer` remain trace-enabled wrappers. |
| **Tests** | Use `otel.SetTracerProvider(tp)` (and `otel.SetTextMapPropagator(prop)` if needed) before Connect. |

---

## Deliver Spans (Service Graph)

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, otelnats/oteljetstream creates synthetic "deliver" spans so NATS appears as a broker in Grafana service graph.

### Span hierarchy

```
send subject (PRODUCER, api)
  └── subject deliver (CONSUMER, nats://addr)  ← injected into headers

process subject (CONSUMER, worker)  ← links to deliver span
```

### Resulting service graph

```
api ──► nats ──► worker
```

Deliver spans use an independent TracerProvider with `service.name = "nats://{connected_addr}"`. This is initialised automatically during `Connect`; no extra configuration is needed beyond setting the OTLP endpoint.

The endpoint must be a **full URL** for HTTP (e.g. `http://otel-collector:4318`) or **host:port** for gRPC (e.g. `otel-collector:4317`). Bare hostnames without scheme or port are not supported.

---

## MessageBatch (`Fetch` / `FetchBytes` / `FetchNoWait`)

Iterate `Messages()` to receive each message with its extracted trace context. Drain the channel completely for each batch before the next `Fetch`.

```go
batch, err := consumer.Fetch(10)
if err != nil { ... }
for m := range batch.Messages() {
    _ = m.Context()
    _ = m.Ack()
}
if err := batch.Error(); err != nil { ... }
```

---

## Diagnostic logging

Uses [`log/slog`](https://pkg.go.dev/log/slog) — no output by default.

| Level | Events |
|-------|--------|
| `DEBUG` | Server address parse failure in `serverAttrsFromConn`; deliver tracer initialised successfully |
| `WARN` | Deliver tracer init failure (exporter or resource creation error) |

Enable with a debug-level slog handler at startup:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Log entries use the `otelnats:` prefix with `error`, `reason`, `service`, and `endpoint` key-value pairs.

---

## Dependencies

- `github.com/nats-io/nats.go` (includes JetStream)
- `go.opentelemetry.io/otel` and SDK (trace, propagation)
- Go 1.24+

Tests use `github.com/stretchr/testify` and `nats-server/v2` for integration tests.
