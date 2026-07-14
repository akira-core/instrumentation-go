# instrumentation-go

OpenTelemetry instrumentation for **NATS** (core + JetStream), **MongoDB** (driver v1 and v2), and **gorilla/websocket**, aligned with [OTel Go Contrib instrumentation guidelines](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation).

This repository contains **four independent Go modules** (`go.mod` per module), each **versioned and tagged separately**. Modules target **Go 1.24**. CI runs `go build`, `go test -race`, and **golangci-lint** per module, then **integration** jobs (testcontainers; Docker required) — see [.github/workflows/ci.yml](.github/workflows/ci.yml).

Instrumentation packages **do not** create a global `TracerProvider`. They use `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` unless you pass `WithTracerProvider` / `WithPropagators`. **Applications** must install a provider and W3C propagator at startup (see each module’s **examples/**).

**Languages:** [繁體中文說明（README.zh-TW.md）](README.zh-TW.md)

## Packages

| Package | Import path | Version (source) | Description |
|---------|-------------|------------------|-------------|
| **otel-mongo** (v1) | `github.com/akira-core/instrumentation-go/otel-mongo/otelmongo` | 0.6.2 | MongoDB driver v1 wrapper; `_oteltrace` on writes; `ContextFromDocument` / decode helpers. |
| **otel-mongo/v2** | `github.com/akira-core/instrumentation-go/otel-mongo/v2` | 0.6.2 | MongoDB driver v2 wrapper; parity with v1. |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/otelnats` | 0.6.2 | Core NATS; W3C context in message headers. |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/oteljetstream` | 0.6.2 | JetStream publish/consume/fetch. |
| **otel-gorilla-ws** | `github.com/akira-core/instrumentation-go/otel-gorilla-ws` | 0.6.0 | Trace context in JSON message body (envelope); `NewConn` / `Dial`. |

Per-module docs: [otel-mongo/README.md](otel-mongo/README.md), [otel-nats/README.md](otel-nats/README.md), [otel-gorilla-ws/README.md](otel-gorilla-ws/README.md) (each also ships a [README.zh-TW.md](otel-mongo/README.zh-TW.md): [otel-nats](otel-nats/README.zh-TW.md), [otel-gorilla-ws](otel-gorilla-ws/README.zh-TW.md)).

## Install

Use the module path and a **git tag** that matches the release you want (tag prefix matches the module, e.g. `otel-mongo/v0.6.0`):

```bash
go get github.com/akira-core/instrumentation-go/otel-mongo@otel-mongo/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-mongo/v2@otel-mongo/v2/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-nats@otel-nats/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-gorilla-ws@otel-gorilla-ws/v0.6.0
```

Then import subpackages as needed (`.../otelmongo`, `.../otelnats`, `.../oteljetstream`, root `otel-gorilla-ws`).

## Tracing feature flags

Switches are **opt-in via environment variables**: if a variable is **unset**, it is treated as **off**. Set it to any value other than `0`, `false`, `no`, or `off` (case-insensitive) to turn **on**.

| Env var | Scope | When unset | Effect |
|---------|-------|------------|--------|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | All modules | off | Global gate. Must be on for module-level tracing flags and (for Mongo) document propagation to apply. |
| `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo` + `otel-mongo/v2` | off | CLIENT spans, non-noop tracer for the wrapper. |
| `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo` + `otel-mongo/v2` | off | Inject/extract `_oteltrace` on writes/reads; still gated by effective tracing. |
| `OTEL_NATS_TRACING_ENABLED` | `otelnats` + `oteljetstream` | off | NATS/JetStream wrapper tracing. |
| `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` | off | WebSocket wrapper tracing. |

### Env × `WithTracingEnabled` decision table

Every module accepts `WithTracingEnabled(v bool)` at construction (`ConnectWithOptions` / `NewConn` / `Dial` / `Upgrade`, etc.). When the option is **present**, it is **authoritative** (overrides env in either direction). When **absent**, env decides (`GLOBAL` AND module switch).

| Env (`GLOBAL` ∧ module) | `WithTracingEnabled` | Effective tracing |
|-------------------------|----------------------|-------------------|
| off (unset or falsy) | *(absent)* | **off** |
| off (unset or falsy) | `true` | **on** |
| off (unset or falsy) | `false` | **off** |
| on | *(absent)* | **on** |
| on | `false` | **off** |
| on | `true` | **on** |

Mongo-only: `WithTracePropagationEnabled` controls `_oteltrace` on that client only while effective tracing is **on**; it cannot enable propagation when effective tracing is off. Package-level `ContextFromDocument` / `ContextFromRawDocument` stay **env-only** (all three Mongo env vars) and ignore per-client options. See [otel-mongo/README.md](otel-mongo/README.md) for the propagation sub-table.

## Layout

```
instrumentation-go/
├── otel-mongo/
│   ├── otelmongo/           # v1 wrapper (module root)
│   ├── v2/                  # v2 wrapper (separate go.mod, own tests/integration/)
│   │   └── tests/integration/
│   ├── examples/
│   ├── tests/integration/   # Docker: testcontainers (v1)
│   └── README.md
├── otel-nats/
│   ├── otelnats/
│   ├── oteljetstream/
│   ├── examples/
│   ├── tests/integration/
│   ├── go.mod
│   └── README.md
├── otel-gorilla-ws/
│   ├── examples/
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

Runnable examples: **otel-nats/examples**, **otel-mongo/examples**, **otel-gorilla-ws/examples**.

## Diagnostic logging

Packages use [`log/slog`](https://pkg.go.dev/log/slog); with the default handler, **nothing is printed** unless you raise the log level.

| Package | Level | Events |
|---------|-------|--------|
| `otel-nats` | `DEBUG` | Server address parse failure |
| `otel-nats` | `DEBUG`/`WARN` | Trace-event unmarshal failure (when `WithTraceDestination` is used) |

Example:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Prefix: `otelnats:` with structured fields (`reason`, `error`, `addr`).
