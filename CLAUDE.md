# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Four independent Go modules providing OpenTelemetry instrumentation for MongoDB, NATS/JetStream, and gorilla/websocket. Each module has its own `go.mod`, versioning, and CI job — they are developed and tagged separately.

| Module dir | Import path suffix | What it wraps |
|---|---|---|
| `otel-mongo/` | `.../otel-mongo/otelmongo` | MongoDB Go driver v1 |
| `otel-mongo/v2/` | `.../otel-mongo/v2` (separate `go.mod`) | MongoDB Go driver v2 |
| `otel-nats/` | `.../otel-nats/otelnats` + `oteljetstream` | NATS core + JetStream |
| `otel-gorilla-ws/` | `.../otel-gorilla-ws` | gorilla/websocket |

Each module also has `example/` and `tests/integration/` sub-modules with their own `go.mod`. Integration tests use **testcontainers-go** (require Docker/Podman running).

## Common Commands

All commands must be run **inside the module directory** being changed.

```bash
# Build
go build ./...

# Test (race detector enabled)
go test -v -race ./...

# Single test
go test -v -race -run TestFunctionName ./...

# Lint (golangci-lint v2 required)
golangci-lint run ./...
```

**Mandatory after any `.go` change:** run all three (`go build`, `go test`, `golangci-lint`) before considering work complete. All three must pass with 0 issues.

```bash
# Integration tests (require Docker; run inside tests/integration/)
cd otel-mongo/tests/integration && go test -v -race ./...
```

## Lint Rules to Know

Config is in `.golangci.yml` (v2 syntax). Common failure modes:

- **`goimports`**: stdlib imports must be in their own group, separated from third-party by a blank line. Local prefix is `github.com/Marz32onE/instrumentation-go`.
- **`errcheck`**: every returned error must be handled (disabled in `_test.go`).
- **`govet`**: includes shadow, printf format checks.
- **`staticcheck`**: full suite enabled.

## Architecture

### Wrapper Pattern

All packages wrap the upstream client type and expose the same API surface with trace instrumentation added:

```go
// caller creates upstream client, wraps it:
wsConn := otelgorillaws.NewConn(rawWebsocketConn, opts...)
nc, _ := otelnats.Connect(url)
client, _ := otelmongo.Connect(ctx, mongoOpts...)
```

### TracerProvider & Propagator

Packages **never** initialize a TracerProvider. They fall back to `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` by default. Override per-connection via functional options:

```go
WithTracerProvider(tp)
WithPropagators(p)
```

Applications call `otelsetup.Init()` at startup to configure the global provider.

### Trace Propagation by Transport

| Transport | Carrier | Where context lives |
|---|---|---|
| MongoDB | Document field `_oteltrace` | `{ traceparent, tracestate }` injected on every write; stripped on read |
| NATS/JetStream | Message headers | `traceparent`, `tracestate` headers via `HeaderCarrier` |
| WebSocket | JSON message body | Top-level `traceparent`/`tracestate` fields + `payload` (base64); non-JSON passthrough |

### Feature Flags (otel-mongo)

Three env vars plus optional `ConnectWithOptions` override (all default **disabled** when unset for the module-specific vars):

| Env var | Scope |
|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master switch (must be truthy for any mongo module flag or `WithTracePropagationEnabled` to apply) |
| `OTEL_MONGO_TRACING_ENABLED` | wrapper **CLIENT** spans, deliver-span wiring, and noop vs real tracer for this package |
| `OTEL_MONGO_PROPAGATION_ENABLED` | `_oteltrace` inject/extract on Collection/Cursor/ChangeStream and **ContextFromDocument** / **ContextFromRawDocument** |

`envEnabledByDefault()` returns `false` when a var is absent. When `OTEL_MONGO_TRACING_ENABLED` is unset/disabled, this package uses a noop tracer for its wrapper spans — **no Mongo CLIENT spans from otel-mongo** (driver/contrib monitor spans are unchanged). Document propagation still follows `OTEL_MONGO_PROPAGATION_ENABLED` and global when using `Connect`; `WithTracePropagationEnabled` only overrides the module propagation default and **cannot** enable propagation if the global switch is off.

### Deliver Spans

All three transports implement an optional "deliver span" pattern: a synthetic span is created with a service name equal to the system identifier (`nats://host:port`, `mongodb://host:port`). This creates a visible broker node in the service graph. For otel-mongo, deliver spans require **both** `mongoTracingEnabled()` to return true AND `OTEL_EXPORTER_OTLP_ENDPOINT` to be set — the function checks `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED`.

### Consumer Context

Subscribers always receive a `MsgWithContext` (NATS/JetStream) or a new `context.Context` return value (WebSocket `ReadMessage`) carrying the extracted remote trace. This context must be threaded into downstream calls to continue the trace chain.

### Span Links vs. Parent-Child

Async consumers (NATS subscribers, MongoDB change stream readers, WebSocket readers) use **span links** rather than parent-child relationships to connect to the producer span. This is intentional — preserves causality without implying synchronous nesting.

## Module-Specific Notes

### otel-mongo

- `_oteltrace` field adds ~100–120 bytes per document. Schema-aware callers can use `SkipDBOperationsExporter` to suppress noisy spans (e.g., `getMore`).
- Use `Cursor.DecodeWithContext(ctx, v)` (not `Decode`) when reading in a change-stream context — it extracts the trace from the document and links spans correctly.
- `ContextFromDocument(ctx, doc)` extracts trace from an already-decoded document map; it respects the same propagation env gates as the Collection wrapper (not a bypass).
- **v1 and v2 parity rule:** `otelmongo/` (v1) and `v2/` are parallel implementations. All logic changes — new flags, new fields, new inject/extract paths — must be applied to **both** sub-packages identically. Run lint and tests for both when either is touched.

### otel-nats

- `otelnats` wraps core NATS; `oteljetstream` wraps JetStream. Both live in the same `go.mod` (`otel-nats/`).
- `Conn.Subscribe` handler signature is `func(MsgWithContext)` — not the native `func(*nats.Msg)`.
- JetStream `Consumer.Messages()` returns an iterator; call `.Context()` on each item for the trace context.
- `WithTraceDestination(subject)` enables NATS 2.11+ infrastructure trace events.

### otel-gorilla-ws

- `NewConn` wraps an already-dialed `*websocket.Conn`; `Conn.Dial` dials and wraps in one step.
- The JSON envelope is an internal wire format — applications see the original payload from `ReadMessage`.

## CI

`.github/workflows/ci.yml` runs a matrix job for all four modules on every push/PR to `main`, `master`, or `feat/*`. Each job: `go build`, `go test -race`, `golangci-lint`. Go 1.24, Ubuntu.
