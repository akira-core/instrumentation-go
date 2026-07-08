# otel-nats (otelnats + oteljetstream)

[繁體中文 (Traditional Chinese)](README.zh-TW.md)

---

OpenTelemetry tracing for [NATS](https://nats.io/) and [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream), aligned with the official `nats.go` / `nats.go/jetstream` APIs. Propagates W3C Trace Context in message headers. `oteljetstream` now fully wraps JetStream consumer management APIs (`StreamConsumerManager` on `JetStream` and `ConsumerManager` on `Stream`) while keeping message publish/consume tracing behavior unchanged. Per [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation): packages accept **TracerProvider** and **Propagators** via options; they do **not** provide InitTracer. Set the global provider and propagator at process startup (see **examples/**).

---

## Layout

```
otel-nats/
├── otelnats/               # Core NATS: Connect, Conn, Publish, Subscribe, Request, HeaderCarrier
│   ├── connect.go          # Connect, ConnectWithOptions, ConnectTLS, ConnectWithCredentials
│   ├── conn.go             # Conn, connImpl interface, Options (WithTracerProvider, WithPropagators, WithTraceDestination)
│   ├── conn_traced.go      # tracedConn: instrumented connImpl (spans, propagation, deliver spans)
│   ├── conn_direct.go      # directConn: passthrough connImpl used when tracing is disabled
│   ├── traceevent.go       # WithTraceDestination / SubscribeTraceEvents / TraceEvent / TraceHop (NATS 2.11+ trace events)
│   ├── propagation.go      # HeaderCarrier (nats.Header ↔ TextMapCarrier)
│   ├── env_flags.go        # tracing feature-flag gate (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED + OTEL_NATS_TRACING_ENABLED)
│   ├── internal/flags/     # shared EnvEnabled/Gate helpers (byte-identical across instrumentation modules)
│   └── doc.go
├── oteljetstream/          # JetStream: New, JetStream, Stream, Consumer, Consume, Messages, Fetch
│   ├── jetstream.go        # New(conn), JetStream interface, shared types (ConsumerConfig, StreamConfig, ...)
│   ├── jetstream_traced.go # tracedJSImpl: instrumented JetStream impl
│   ├── jetstream_direct.go # directJSImpl: passthrough JetStream impl
│   ├── stream.go           # Stream interface (consumer-manager methods)
│   ├── stream_traced.go    # tracedStream: instrumented Stream impl
│   ├── stream_direct.go    # directStream: passthrough Stream impl
│   ├── consumer.go         # Consumer interface, Msg, MessageBatch, MessagesContext
│   ├── consumer_traced.go  # tracedConsumer: Consume/Messages/Next/Fetch with spans
│   ├── consumer_direct.go  # directConsumer: passthrough Consumer impl
│   └── doc.go
├── examples/            # How to create TracerProvider + set global + use otelnats/oteljetstream
├── go.mod
└── README.md
```

---

## Usage

### Tracing feature flags

`otel-nats` (`otelnats` + `oteljetstream`) supports:

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global master switch)
- `OTEL_NATS_TRACING_ENABLED` (nats module switch)

Defaults: **DISABLED** when unset — both vars must be explicitly set to a truthy value to enable tracing. Values `false/0/no/off` (case-insensitive) disable; any other set value is truthy.

Priority:
1. Global off disables all nats tracing regardless of module flag.
2. Otherwise module flag controls nats tracing.

When disabled, both span creation and W3C header propagation are turned off.

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
    "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
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

### 3. Request/Reply

`Conn.Request` / `RequestWithContext` / `RequestMsg` / `RequestMsgWithContext` mirror the equivalent `nats.Conn` methods exactly, but open a CLIENT span for the RPC and a CONSUMER span for the reply:

```go
reply, err := conn.RequestWithContext(ctx, "subject", []byte("ping"))
if err != nil { log.Fatal(err) }
// reply.Data — trace context for the request/reply pair is recorded on the CLIENT span;
// the reply itself is recorded as a linked CONSUMER span.
```

`Request` / `RequestMsg` have no `context.Context` parameter (mirroring `nats.Conn`), so their producer span is rooted at `context.Background()` — use `RequestWithContext` / `RequestMsgWithContext` to chain into an existing trace.

### 4. JetStream

```go
import (
    "github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
    "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

conn, _ := otelnats.Connect(natsURL, nil)
defer conn.Close()

js, err := oteljetstream.New(conn)
// After creating stream/consumer:
cons.Consume(func(m oteljetstream.Msg) {
    // m.Data(), m.Ack(), m.Context() — trace from message headers
})
```

Or iterate manually with `Messages()`:

```go
iter, err := cons.Messages()
if err != nil { log.Fatal(err) }
defer iter.Stop() // release the iterator goroutine and end any in-flight span

for {
    ctx, msg, err := iter.Next()
    if err != nil { break } // iterator stopped/drained
    _ = ctx // trace context extracted from msg headers
    _ = msg.Ack()
}
```

> **Push consumers** are wrapped (`PushConsumer`/`CreatePushConsumer`/`CreateOrUpdatePushConsumer`/`UpdatePushConsumer` on both `JetStream` and `Stream`); the returned `PushConsumer.Consume` carries trace context. Management-only APIs (`PauseConsumer`/`ResumeConsumer`/`UnpinConsumer`/`ResetConsumer`/`ResetConsumerToSequence`) are exposed directly on `Stream` as untraced passthroughs; `Unwrap()` exists only on `JetStream`, for APIs the wrapper does not re-expose (`KeyValue`/`ObjectStore`/`AccountInfo`/`Conn`/`Options`). Async publish (`PublishAsync`/`PublishMsgAsync`) is not wrapped: these take no `context.Context` and return a non-blocking `PubAckFuture` instead of a synchronous ack, which doesn't fit this wrapper's context-propagation model (see `oteljetstream/doc.go`).

### 5. Tests

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
| **Request / RequestWithContext / RequestMsg / RequestMsgWithContext** | RPC helpers mirroring `nats.Conn`; open a CLIENT span for the request and a linked CONSUMER span for the reply. |
| **JetStream consumer managers** | `JetStream` fully wraps `StreamConsumerManager`; `Stream` fully wraps `ConsumerManager`. Methods returning `Consumer` or `PushConsumer` remain trace-enabled wrappers (see JetStream section). |
| **WithTraceDestination / SubscribeTraceEvents** | Convert NATS 2.11+ infrastructure trace events into OTel spans (see **NATS 2.11+ Trace Events**). |
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

Deliver spans additionally require tracing itself to be enabled: both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` must be truthy (see **Tracing feature flags**). When tracing is disabled, `Connect` never initialises the deliver-span TracerProvider, regardless of `OTEL_EXPORTER_OTLP_ENDPOINT`.

---

## NATS 2.11+ Trace Events

NATS server 2.11+ can publish infrastructure-level trace events (ingress, egress, JetStream store, subject-mapping, stream-export, service-import) for any message carrying a `Nats-Trace-Dest` header. `otel-nats` can consume these events and convert each hop into an OTel span.

### Producer: set the trace destination

```go
conn, err := otelnats.ConnectWithOptions(natsURL, nil,
    otelnats.WithTraceDestination("nats.trace.events"),
)
```

While tracing is enabled, every message sent via `conn.Publish`/`conn.PublishMsg` carries the `Nats-Trace-Dest` header, so the server emits a `TraceEvent` payload to `nats.trace.events` for each hop the message takes.

### Consumer: convert events into spans

```go
sub, err := otelnats.SubscribeTraceEvents(conn, "nats.trace.events")
if err != nil { log.Fatal(err) }
defer sub.Unsubscribe()
```

Each `otelnats.TraceEvent` payload covers one server and carries a list of `otelnats.TraceHop`s. `SubscribeTraceEvents` emits one point-in-time span per hop (named `nats.<KIND>.<type>`, e.g. `nats.CLIENT.ingress`), linked to the original publisher span via the `traceparent` header embedded in the event's request headers.

Requires NATS server 2.11+. `SubscribeTraceEvents` only emits spans when the connection's tracing gate is on; with tracing disabled it discards events instead (subscription still succeeds so `Unsubscribe` lifecycle works either way).

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

`MessageBatch.Stop()` releases the internal goroutine and ends any in-flight span. Callers that fully drain the channel until it closes need not call it; callers that `break`/`return` before the channel closes **must** call it (typically via `defer`) to avoid leaking the goroutine and the last consumer span:

```go
batch, err := consumer.Fetch(10)
if err != nil { ... }
defer batch.Stop()

for m := range batch.Messages() {
    if shouldStopEarly(m) {
        break // deferred batch.Stop() ends the in-flight span and stops the goroutine
    }
    _ = m.Context()
    _ = m.Ack()
}
```

---

## Diagnostic logging

Uses [`log/slog`](https://pkg.go.dev/log/slog) — no output by default.

| Level | Events |
|-------|--------|
| `DEBUG` | Server address parse failure in `serverAttrsFromConn`; deliver tracer initialised successfully; trace event received (`traceevent.go`) |
| `WARN` | Deliver tracer init failure (exporter or resource creation error); trace event JSON unmarshal failure (`traceevent.go`) |

Enable with a debug-level slog handler at startup:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Log entries use the `otelnats:` prefix. Connection/deliver-span log lines (`conn.go`) use `error`, `reason`, `service`, and `endpoint` key-value pairs; trace-event log lines (`traceevent.go`) additionally use `raw`, `server`, `hops`, `events`, and `request_headers`.

---

## Dependencies

- `github.com/nats-io/nats.go` (includes JetStream)
- `go.opentelemetry.io/otel` and SDK (trace, propagation)
- Go 1.24+

Tests use `github.com/stretchr/testify` and `nats-server/v2` for integration tests.
