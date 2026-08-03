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

Every module can be switched off, and a running one can be **revoked** through a
[GO Feature Flag](https://gofeatureflag.org) relay proxy reached via
[OpenFeature](https://openfeature.dev) — without restarting the application.

```
tracing = gate1 && OTEL_<MODULE>_TRACING_ENABLED && relay verdict
```

Three conjunctive tiers. The first two are environment-derived and fixed at construction; the
third is the only one that changes without a redeploy, and **it can only subtract**. Nothing on
the relay can enable what a deployment left off, and an application that installs no provider
behaves exactly as its environment says.

To connect a relay, set one environment variable — there is no code to write:

```sh
OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT=http://relay:1031
```

| Relay flag key | Paired env var | Scope |
|---|---|---|
| *(none — env only)* | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `gate1`, all modules. No relay counterpart: the brake that still works when the relay does not |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo` + `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `_oteltrace` written into your documents |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otelnats` + `oteljetstream` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

A switch is on only when set to `1`, `true`, `yes` or `on`. Everything else — including the empty
string — is off.

> **Everything else is in [feature-flags.md](feature-flags.md)**: the full resolution tables, the
> other two relay-connection variables, per-service targeting, revocation latency, the
> `WithTracingEnabled` mutual-exclusion rule, what a revocation does *not* stop, and the
> operational summary. One home, so the two cannot drift.
> 繁體中文:[feature-flags.zh-TW.md](feature-flags.zh-TW.md)

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
├── feature-flags.zh-TW.md   # …and its Traditional Chinese translation
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
