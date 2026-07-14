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

Each module also has `examples/` and `tests/integration/` sub-modules with their own `go.mod`. Integration tests use **testcontainers-go** (require Docker/Podman running). (`otel-mongo/v2` has no separate `examples/` of its own — the single `otel-mongo/examples/` module imports and demos the v2 package.)

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

- **`goimports`**: stdlib imports must be in their own group, separated from third-party by a blank line. Local prefix is `github.com/akira-core/instrumentation-go`.
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
| WebSocket | JSON message body | `{"header":{"traceparent":...,"tracestate":...},"data":<payload>}` envelope on write; non-JSON payloads are JSON-string-encoded (not base64) into `data`; legacy flat top-level `traceparent`/`tracestate` fields still accepted as a read-only fallback |

### Feature Flags (otel-mongo)

Three env vars plus optional `ConnectWithOptions` override (all default **disabled** when unset for the module-specific vars):

| Env var | Scope |
|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master switch (must be truthy for any mongo module flag or `WithTracePropagationEnabled` to apply) |
| `OTEL_MONGO_TRACING_ENABLED` | gates **both** wrapper **CLIENT** spans (noop vs real tracer) **and** `_oteltrace` document propagation for this package |
| `OTEL_MONGO_PROPAGATION_ENABLED` | only consulted when both global and `OTEL_MONGO_TRACING_ENABLED` are on; final say on `_oteltrace` inject/extract on Collection/Cursor/ChangeStream and **ContextFromDocument** / **ContextFromRawDocument** |

`flags.EnvEnabled()` (from the shared `internal/flags` package — see below) returns `false` when a var is absent. When `OTEL_MONGO_TRACING_ENABLED` is unset/disabled, this package uses a noop tracer for its wrapper spans **and** force-disables `_oteltrace` propagation — Mongo tracing and Mongo trace propagation share a single kill switch. `WithTracePropagationEnabled` only overrides the propagation default while both tracing gates are on; it **cannot** enable propagation when global or module tracing is off.

### Shared `internal/flags` package

All four modules vendor their own copy of `internal/flags` (`flags.go` + `flags_test.go`); its doc comment requires the file contents (excluding the `package` line) to stay byte-identical across every copy. It exports two primitives used by both the strategy-split and cached-gate enforcement patterns below:

- `EnvEnabled(name string) bool` — default-off env var read; unset or falsy (`0`/`false`/`no`/`off`, case-insensitive) → `false`.
- `Gate` — caches a resolver function's result once via `sync.Once`/`atomic.Bool`. `NewGate(fn)` constructs one, `Enabled()` returns the cached value, and `ResetForTest()` (not parallel-safe) exists only for tests that toggle env vars with `t.Setenv`.

### Consumer Context

Subscribers always receive a `Msg` (NATS/JetStream; call `.Context()` for the extracted trace) or a new `context.Context` return value (WebSocket `ReadMessage`) carrying the extracted remote trace. This context must be threaded into downstream calls to continue the trace chain.

### Span Links vs. Parent-Child

Async consumers (NATS subscribers, MongoDB change stream readers, WebSocket readers) use **span links** rather than parent-child relationships to connect to the producer span. This is intentional — preserves causality without implying synchronous nesting.

### Disabled-mode invariant (0.3.0+)

When any feature flag returns false, **no OTel SDK code path may run**: no `tracer.Start` on a real tracer, no `sdktrace.NewTracerProvider`, no `otlptracegrpc.New` / `otlptracehttp.New`, no `[]attribute.KeyValue` build, no propagator inject/extract. Two enforcement patterns coexist:

**1. Strategy split (preferred — otel-mongo Collection / Cursor / SingleResult / ChangeStream).** The facade type holds an `impl` interface satisfied by either `internal/direct.X` (passthrough) or `internal/traced.X` (instrumented). Construction picks the impl once; per-method runtime gates disappear. `internal/direct/*.go` imports no `go.opentelemetry.io/otel/sdk/*` and no `otel/exporters/*` — the disabled path is **compiler-enforced** by package boundary.

**2. Cached gate (otel-nats, otel-gorilla-ws, and otel-mongo Client/Database).** Connect/constructor reads env once → caches `tracingEnabled bool` on the wrapper struct. Every public method starts with `if !c.tracingEnabled { /* delegate to native */ }`. Reviewer-enforced. Migration of these wrappers to the strategy-split pattern is planned but not yet tracked in a written design doc.

Independent of pattern:
- For otel-mongo, `Connect` substitutes `noop.NewTracerProvider()` when disabled so any stray `tracer.Start` is inert.

**Adding a new public method to a strategy-split wrapper** (otel-mongo Collection/Cursor/SingleResult/ChangeStream) — touch THREE files in lockstep per module, mirror in v1↔v2 sibling:
1. Add signature to the facade's `collectionImpl` interface (in `collection.go`) or extend `shared.CursorImpl` / `shared.SingleResultImpl` / `shared.ChangeStreamImpl` in `internal/shared/impls.go`.
2. Implement passthrough in `internal/direct/<file>.go` — no `otel/sdk` or `otel/exporters` imports.
3. Implement instrumented version in `internal/traced/<file>.go`.

Compile-time `var _ shared.CursorImpl = (*traced.Cursor)(nil)` assertions in facade `cursor.go` / `results.go` (and `var _ collectionImpl = (*traced.Collection)(nil)` in `collection.go`) fail the build if any impl misses a method.

**Adding a new public method to a cached-gate wrapper** (otel-nats, otel-gorilla-ws) — fast-path gate is the first statement: `if !c.tracingEnabled { return c.nc.Publish(...) }`. Examples to copy: `otelnats.Conn.Publish`, `otelgorillaws.Conn.WriteMessage`.

### Strategy-split layout (otel-mongo)

Per module (`otelmongo/` v1 and `v2/`), the facade package contains thin wrappers + the `collectionImpl` interface; impls live under `internal/`:

```
otelmongo/
├── collection.go cursor.go results.go database.go client.go    # facade
├── tracing.go env_flags.go version.go                          # facade helpers
└── internal/
    ├── shared/    # impls.go (CursorImpl/SingleResultImpl/ChangeStreamImpl interfaces),
    │              # semconv.go, tracing.go, bulkwrite.go, monitor.go, hostport.go — helpers used by both paths
    ├── direct/    # collection.go cursor.go singleresult.go changestream.go
    │              # NO go.opentelemetry.io/otel/sdk/* or otel/exporters/* imports
    └── traced/    # collection.go cursor.go singleresult.go changestream.go
                   # full OTel SDK access
```

Key rules:
- `internal/shared/impls.go` declares the polymorphic interfaces (`CursorImpl`, `SingleResultImpl`, `ChangeStreamImpl`) satisfied by both `internal/direct.X` and `internal/traced.X`. Facade `Cursor` / `SingleResult` / `ChangeStream` hold an `impl shared.XImpl` field.
- Facade `collectionImpl` interface returns raw driver types (`*mongo.Cursor`, `*mongo.SingleResult`, `*mongo.ChangeStream`) + `shared.XImpl` — the impl packages never need to import the facade, preventing any facade ↔ internal cycle. Facade methods wrap raw types into facade wrappers (`&Cursor{Cursor: raw, impl: cImpl}`).
- `internal/traced.Collection` has **exported fields** (`Coll`, `Tracer`, `Propagator`, `PropagationEnabled`, `ServerAddr`, `ServerPort`) so facade-package tests can build literals and call them directly.
- v1/v2 parity extends to `internal/{direct,traced,shared}/`. The helpers in `internal/shared/{bulkwrite.go,semconv.go,tracing.go,impls.go,monitor.go,hostport.go}` are intentionally duplicated across modules (separate `internal/` trees cannot share). A drift-check CI step to catch divergence between the two copies is planned but not yet implemented.
- `internal/shared/monitor.go` builds the `event.CommandMonitor` (`shared.NewCommandMonitor`) that captures the real per-command server address from `CommandStartedEvent.ConnectionID` into a context-scoped holder (`shared.WithAddrCapture`/`*shared.AddrCapture.Resolve`), chaining any caller-supplied monitor rather than replacing it. `client.go`'s `ConnectWithOptions` registers it (tracing-enabled branch only, via `options.MergeClientOptions`); `internal/traced/collection.go` call sites read it back after the raw driver call to overwrite `server.address`/`server.port` on the span, falling back to the static URI-parsed value when nothing was captured. `internal/shared/hostport.go` (`SplitHostPort`) is the shared IPv6-aware host:port parser used by both `monitor.go` and `client.go`'s `parseServerFromURI`.

### Propagation flag caching (otel-mongo)

`ContextFromDocument` / `ContextFromRawDocument` (`tracing.go`, both v1 and v2) call `cachedPropagationEnabled()`, which reads env **once** via `sync.Once` and stores in `atomic.Bool` (`env_flags.go`). The cached value reflects the full gate: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED` AND `OTEL_MONGO_PROPAGATION_ENABLED`. **Env changes after first call are ignored.** Tests that toggle any of those three vars via `t.Setenv` **must** call `resetPropEnabledCacheForTest()` after the Setenv to reset the cache. Helpers `enableTracing` / `enableDocumentPropagation` in `tracing_test.go` already invoke reset + `t.Cleanup` (and `enableDocumentPropagation` now sets all three flags). Do **not** add `t.Parallel()` to tests that touch these env vars — the reset is not parallel-safe.

### `oteljetstream.MessageBatch.Stop()`

`MessageBatch` interface (`oteljetstream/consumer.go`) includes a `Stop()` method (added 0.3.0; **breaking** for custom implementations). Callers that drain `Messages()` to channel close need not call it; callers that `break` / `return` early **must** `defer batch.Stop()` to release the internal goroutine and end the in-flight span. The disabled-tracing path uses `directMessageBatch` (no spans, no attributes, but still 1 goroutine for `jetstream.Msg → Msg` type adaptation).

## Versioning

Each module is tagged independently as `<module>/v<x.y.z>`. Version strings live in:

- `otel-nats/otelnats/conn.go` — `instrumentationVersion` const
- `otel-mongo/otelmongo/version.go` — `instrumentationVersion` const
- `otel-mongo/v2/version.go` — `instrumentationVersion` const
- `otel-gorilla-ws/version.go` — return literal from `Version()`

Bump on any code change to a module before pushing release tag. Module pre-1.0 (`0.x.y`): minor bump allowed for breaking changes.

## Module-Specific Notes

### otel-mongo

- `_oteltrace` field adds ~100–120 bytes per document.
- Use `Cursor.DecodeWithContext(ctx, v)` (not `Decode`) when reading in a change-stream context — it extracts the trace from the document and links spans correctly.
- `ContextFromDocument(ctx, doc)` extracts trace from an already-decoded document map; it respects the same propagation env gates as the Collection wrapper (not a bypass).
- **Strategy-split layout:** Collection / Cursor / SingleResult / ChangeStream all live in `internal/{direct,traced}/` (see *Strategy-split layout (otel-mongo)* above). Client and Database still use the cached-gate pattern.
- **v1 and v2 parity rule:** `otelmongo/` (v1) and `v2/` are parallel implementations. All logic changes — new flags, new fields, new inject/extract paths, new strategy methods — must be applied to **both** sub-packages identically, including their `internal/{direct,traced,shared}/` trees. Run lint and tests for both when either is touched.

### otel-nats

- `otelnats` wraps core NATS; `oteljetstream` wraps JetStream. Both live in the same `go.mod` (`otel-nats/`).
- `Conn.Subscribe` handler signature is `MsgHandler` (`func(Msg)`) — not the native `func(*nats.Msg)`.
- JetStream `Consumer.Messages()` returns an iterator; call `.Context()` on each item for the trace context.
- `WithTraceDestination(subject)` enables NATS 2.11+ infrastructure trace events.

### otel-gorilla-ws

- `NewConn` wraps an already-dialed `*websocket.Conn`; the package-level `Dial` function dials and wraps in one step.
- The JSON envelope is an internal wire format — applications see the original payload from `ReadMessage`.
- Subprotocol negotiation scenarios (client/server × otel-ws-aware/unaware, including the empty-subprotocol edge case) are documented in `otel-ws.md` at the repo root — consult it when touching `Dial`'s or `Upgrader.Upgrade`'s negotiation logic.

## CI

`.github/workflows/ci.yml` runs on every push/PR to `main`, `master`, or `feat/*`, Go 1.25 on Ubuntu, with two jobs:

- `test-and-lint` — matrix over all four modules: `go build`, `go test -race`, `golangci-lint`. For `otel-mongo` and `otel-mongo/v2` only, an additional "Verify direct/ has no OTel SDK imports" step greps `internal/direct/` for `go.opentelemetry.io/otel` imports and fails the build if any are found — this is the CI-enforced half of the disabled-mode invariant described above (the strategy-split package boundary is the compiler-enforced half).
- `integration-test` — gated on `needs: test-and-lint`; matrix over `otel-nats/tests/integration`, `otel-mongo/tests/integration`, `otel-mongo/v2/tests/integration`, and `otel-gorilla-ws/tests/integration`, running `go test -v -race -timeout 120s ./...` (testcontainers-based, requires Docker).
