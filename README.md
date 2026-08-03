# instrumentation-go

OpenTelemetry instrumentation for **NATS** (core + JetStream), **MongoDB** (driver v1 and v2), and **gorilla/websocket**, aligned with [OTel Go Contrib instrumentation guidelines](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation).

This repository contains **four independent instrumentation modules** (`go.mod` per module), each **versioned and tagged separately**, plus **two supporting modules** — `otel-sampler` (a released consistent-probability sampler applications import) and `otel-testkit` (an untagged, test-only E2E harness). Modules target **Go 1.25**. CI runs `go build`, `go test -race`, and **golangci-lint** per module, then **integration** and **consistent-sampling E2E** jobs (testcontainers; Docker required) — see [.github/workflows/ci.yml](.github/workflows/ci.yml).

Instrumentation packages **do not** create a global `TracerProvider`. They use `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` unless you pass `WithTracerProvider` / `WithPropagators`. **Applications** must install a provider and W3C propagator at startup (see each module’s **examples/**).

**Languages:** [繁體中文說明（README.zh-TW.md）](README.zh-TW.md)

## Packages

| Package | Import path | Version (source) | Description |
|---------|-------------|------------------|-------------|
| **otel-mongo** (v1) | `github.com/akira-core/instrumentation-go/otel-mongo/otelmongo` | 0.7.0 | MongoDB driver v1 wrapper; `_oteltrace` on writes; `ContextFromDocument` / decode helpers. |
| **otel-mongo/v2** | `github.com/akira-core/instrumentation-go/otel-mongo/v2` | 0.7.0 | MongoDB driver v2 wrapper; parity with v1. |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/otelnats` | 0.7.0 | Core NATS; W3C context in message headers. |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/oteljetstream` | 0.7.0 | JetStream publish/consume/fetch. |
| **otel-gorilla-ws** | `github.com/akira-core/instrumentation-go/otel-gorilla-ws` | 0.7.0 | Trace context in JSON message body (envelope); `NewConn` / `Dial`. |

### Supporting modules

| Package | Import path | Version (source) | Description |
|---------|-------------|------------------|-------------|
| **otel-sampler** | `github.com/akira-core/instrumentation-go/otel-sampler/otelsampler` | 0.1.1 | Consistent probability sampler (`ot=th:`/`ot=rv:`) + `WithSingleLinkSeed`, so span-link consumers sample like parent-child ones. Emits no spans. |
| **otel-testkit** | `github.com/akira-core/instrumentation-go/otel-testkit/harness` | untagged | Black-box E2E harness (in-process OTLP sink + collector + assertions) used by this repo's sampling suites. Test-only, no stability guarantee. |

`otel-sampler`'s published `v0.1.0` tag points at a pre-rebase commit and is superseded — start at `0.1.1`. See [VERSIONING.md](VERSIONING.md).

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

> Full reference: **[feature-flags.md](feature-flags.md)** — every resolution table, the
> truthiness rules, the provider wiring requirements, and the operational summary. That document
> describes the model being introduced in `otel-mongo` 0.9.0 / `otel-nats` 0.8.0 /
> `otel-gorilla-ws` 0.8.0, in which the relay becomes a **revoke-only** kill switch. The summary
> below describes the currently released behaviour.

Switches resolve at **runtime** through [OpenFeature](https://openfeature.dev), so an operator can turn tracing on or off through a GO Feature Flag relay proxy **without restarting the application**. When no OpenFeature provider is installed, every switch falls back to its environment variable and behavior is identical to before dynamic flags existed.

Environment variables are **opt-in**: if a variable is **unset**, it is treated as **off**. Set it to any value other than `0`, `false`, `no`, or `off` (case-insensitive) to turn **on**.

| OpenFeature flag key | Fallback env var | Scope | Effect |
|---|---|---|---|
| *(none — env only)* | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | All modules | **Kill switch.** Off ⇒ no OpenFeature evaluation at all and no relay value can enable anything. |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo` + `otel-mongo/v2` | CLIENT spans for the wrapper. |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo` + `otel-mongo/v2` | Inject/extract `_oteltrace` on writes/reads; still gated by effective tracing. |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otelnats` + `oteljetstream` | NATS/JetStream wrapper tracing. |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` | WebSocket span creation. |

The global switch deliberately has **no relay counterpart**: it is the out-of-band brake that still works when the relay is unreachable or misconfigured.

### Wiring a provider

The instrumentation modules never install an OpenFeature provider — the same rule that keeps them from initializing a `TracerProvider`. Applications wire one at startup, next to their existing OTel setup:

```go
import (
    "github.com/open-feature/go-sdk/openfeature"
    gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
)

provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint: "http://relay-proxy:1031",
    // Required. The collector buffers one event per evaluation and, once full
    // (100k by default), flushes synchronously from the evaluating goroutine
    // while holding a mutex. With the relay down that flush fails after the
    // HTTP timeout and the buffer never drains, so every instrumented
    // operation ends up queueing behind a doomed 10 s request.
    DataCollectorDisabled: true,
})
if err != nil {
    return err
}
// Blocking install: an unresolvable flag means "allow", so a provider that has
// not fetched yet cannot revoke anything.
if err := openfeature.SetProviderAndWait(provider); err != nil {
    // Do NOT fail startup. The relay is a brake, not a prerequisite: come up at
    // the state the environment declares, without relay control until a restart.
    logger.Error("feature flag provider unavailable; continuing without relay control", "error", err)
}
// optional, for process-level targeting on the relay:
openfeature.SetEvaluationContext(openfeature.NewTargetlessEvaluationContext(map[string]any{
    "service.name": "checkout-api",
}))
```

The GO Feature Flag provider is an **application** dependency; the instrumentation modules depend only on `github.com/open-feature/go-sdk`.

Resolved values are cached per module for **one second**, so hot paths never enter the OpenFeature evaluation pipeline. Because that cache is process-wide, targeting can key on process-level attributes (service, environment, host) but **not** on per-request attributes.

### Precedence

| Priority | Source | Notes |
|---|---|---|
| 1 | `WithTracingEnabled(v)` | Connection/client becomes fully **static** — no OpenFeature evaluation runs for it and no relay change reaches it. |
| 2 | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | Kill switch. Off ⇒ off, unconditionally. |
| 3 | Relay flag | Decides when the relay has an opinion. |
| 4 | Module env var | The value passed to OpenFeature as the evaluation default. |

Every module accepts `WithTracingEnabled(v bool)` at construction (`ConnectWithOptions` / `NewConn` / `Dial` / `Upgrade`, etc.):

| Kill switch | Relay / module flag | `WithTracingEnabled` | Effective tracing |
|---|---|---|---|
| off | any | *(absent)* | **off** — no evaluation performed |
| off | any | `true` | **on** |
| off | any | `false` | **off** |
| on | on | *(absent)* | **on** |
| on | off | *(absent)* | **off** |
| on | any | `false` | **off** |
| on | any | `true` | **on** |

Mongo-only: `WithTracePropagationEnabled` controls `_oteltrace` on that client only while effective tracing is **on**; it cannot enable propagation when effective tracing is off. Package-level `ContextFromDocument` / `ContextFromRawDocument` follow the relay (they have no client to consult, so they ignore per-client options). See [otel-mongo/README.md](otel-mongo/README.md) for the propagation sub-table.

WebSocket-only: `otel-ws` subprotocol negotiation is gated on the **kill switch alone**, never on the relay flag. The handshake cannot be revisited, so a connection established while the relay flag is off would otherwise never be able to propagate trace context after it flips on. Consequence: two peers both running this library with the kill switch on exchange the JSON envelope even while tracing is off.

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
├── feature-flags.md         # Full tracing feature-flag reference
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
