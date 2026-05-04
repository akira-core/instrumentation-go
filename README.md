# instrumentation-go

OpenTelemetry instrumentation for **NATS** (core + JetStream), **MongoDB** (driver v1 and v2), and **gorilla/websocket**, aligned with [OTel Go Contrib instrumentation guidelines](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation).

This repository contains **four independent Go modules** (`go.mod` per module), each **versioned and tagged separately**. Modules target **Go 1.24**. CI runs `go build`, `go test -race`, and **golangci-lint** per module, then **integration** jobs (testcontainers; Docker required) — see [.github/workflows/ci.yml](.github/workflows/ci.yml).

Instrumentation packages **do not** create a global `TracerProvider`. They use `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` unless you pass `WithTracerProvider` / `WithPropagators`. **Applications** must install a provider and W3C propagator at startup (see each module’s **example/**).

**Languages:** [繁體中文說明（README.zh-TW.md）](README.zh-TW.md)

## Packages

| Package | Import path | Version (source) | Description |
|---------|-------------|------------------|-------------|
| **otel-mongo** (v1) | `github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo` | 0.2.11 | MongoDB driver v1 wrapper; `_oteltrace` on writes; `ContextFromDocument` / decode helpers; optional deliver spans. |
| **otel-mongo/v2** | `github.com/Marz32onE/instrumentation-go/otel-mongo/v2` | 0.2.11 | MongoDB driver v2 wrapper; parity with v1. |
| **otel-nats** | `github.com/Marz32onE/instrumentation-go/otel-nats/otelnats` | 0.2.11 | Core NATS; W3C context in message headers; deliver spans. |
| **otel-nats** | `github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream` | 0.2.11 | JetStream publish/consume/fetch; deliver spans. |
| **otel-gorilla-ws** | `github.com/Marz32onE/instrumentation-go/otel-gorilla-ws` | 0.2.11 | Trace context in JSON message body (envelope); `NewConn` / `Conn.Dial`. |

Per-module docs: [otel-mongo/README.md](otel-mongo/README.md), [otel-nats/README.md](otel-nats/README.md), [otel-gorilla-ws/README.md](otel-gorilla-ws/README.md) (Mongo and NATS also ship [README.zh-TW.md](otel-mongo/README.zh-TW.md) / [README.zh-TW.md](otel-nats/README.zh-TW.md)).

## Install

Use the module path and a **git tag** that matches the release you want (tag prefix matches the module, e.g. `otel-mongo/v0.2.11`):

```bash
go get github.com/Marz32onE/instrumentation-go/otel-mongo@otel-mongo/v0.2.11
go get github.com/Marz32onE/instrumentation-go/otel-mongo/v2@otel-mongo/v2/v0.2.11
go get github.com/Marz32onE/instrumentation-go/otel-nats@otel-nats/v0.2.11
go get github.com/Marz32onE/instrumentation-go/otel-gorilla-ws@otel-gorilla-ws/v0.2.11
```

Then import subpackages as needed (`.../otelmongo`, `.../otelnats`, `.../oteljetstream`, root `otel-gorilla-ws`).

## Tracing feature flags

Switches are **opt-in via environment variables**: if a variable is **unset**, it is treated as **off**. Set it to any value other than `0`, `false`, `no`, or `off` (case-insensitive) to turn **on**.

| Env var | Scope | When unset | Effect |
|---------|-------|------------|--------|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | All modules | off | Global gate. Must be on for module-level tracing flags and (for Mongo) document propagation to apply. |
| `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo` + `otel-mongo/v2` | off | CLIENT spans, deliver-span wiring, non-noop tracer for the wrapper. |
| `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo` + `otel-mongo/v2` | off | Inject/extract `_oteltrace` on writes/reads; still gated by the global switch above. |
| `OTEL_NATS_TRACING_ENABLED` | `otelnats` + `oteljetstream` | off | NATS/JetStream wrapper tracing. |
| `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` | off | WebSocket wrapper tracing. |

If the **global** switch is off, module flags are ignored. Mongo `WithTracePropagationEnabled` cannot enable propagation when the global switch is off.

## Layout

```
instrumentation-go/
├── otel-mongo/
│   ├── otelmongo/           # v1 wrapper (module root)
│   ├── v2/                  # v2 wrapper (separate go.mod)
│   ├── example/
│   ├── tests/integration/   # Docker: testcontainers
│   └── README.md
├── otel-nats/
│   ├── otelnats/
│   ├── oteljetstream/
│   ├── example/
│   ├── tests/integration/
│   ├── go.mod
│   └── README.md
├── otel-gorilla-ws/
│   ├── example/
│   ├── tests/integration/
│   ├── go.mod
│   └── README.md
├── otel-ws.md               # Subprotocol / propagation design notes (cross-language)
├── CLAUDE.md                # Contributor / agent notes
└── README.md
```

## Usage pattern

1. **Application** builds a `TracerProvider` (e.g. OTLP), calls `otel.SetTracerProvider(tp)` and `otel.SetTextMapPropagator(propagation.TraceContext{})` (or your stack’s setup), and shuts down on exit.
2. **Application** wraps clients: `otelnats.Connect(url, nil)`, `otelmongo.Connect(ctx, opts...)`, `otelgorillaws.NewConn(raw, opts...)`, etc.

Runnable examples: **otel-nats/example**, **otel-mongo/example**, **otel-gorilla-ws/example**.

## Diagnostic logging

Packages use [`log/slog`](https://pkg.go.dev/log/slog); with the default handler, **nothing is printed** unless you raise the log level.

| Package | Level | Events |
|---------|-------|--------|
| `otel-nats` | `DEBUG` | Server address parse failure, deliver tracer init success |
| `otel-nats` | `WARN` | Deliver tracer init failure (endpoint missing or unreachable) |
| `otel-mongo` | `DEBUG` | Deliver tracer init success |
| `otel-mongo` | `WARN` | OTLP exporter / resource creation failure |

Example:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Prefixes include `otelnats:` and `otelmongo:` with structured fields (`reason`, `error`, `service`, `endpoint`).

---

## `OTEL_EXPORTER_OTLP_ENDPOINT` format

**Deliver spans** (otel-mongo, otel-nats) use this env var to build a small separate exporter for synthetic “broker” spans. Use an explicit endpoint:

| Protocol | Format | Example |
|----------|--------|---------|
| OTLP/HTTP | Full URL with scheme | `http://otel-collector:4318` |
| OTLP/gRPC | `host:port` (no scheme) | `otel-collector:4317` |

Bare hostnames without scheme or port (e.g. `otel-collector` alone) are **not** supported.
