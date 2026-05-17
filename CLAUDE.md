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

### Feature Flags

**Universal default-OFF posture:** every env var across every module defaults to OFF when unset. A binary that links these wrapper packages but sets no env vars MUST behave indistinguishably from a binary using the unwrapped upstream client — no extra header bytes on the wire, no extra spans, no extra goroutines, no allocation. Truthy = any non-falsy value; falsy set = `0` / `false` / `no` / `off` (case-insensitive, whitespace-trimmed).

Env-var surface per module:

| Variable | Module | Tier | Default | Purpose |
|---|---|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | all | global master | OFF | hard prerequisite for every per-module flag |
| `OTEL_MONGO_TRACING_ENABLED` | otel-mongo v1+v2 | module tracing | OFF | wrapper CLIENT spans (noop vs real tracer) + deliver-span wiring; force-disables propagation when off |
| `OTEL_MONGO_PROPAGATION_ENABLED` | otel-mongo v1+v2 | module propagation | OFF | final say on `_oteltrace` inject/extract on Collection/Cursor/ChangeStream + `ContextFromDocument` / `ContextFromRawDocument` |
| `OTEL_NATS_TRACING_ENABLED` | otel-nats | module tracing | OFF | wrapper spans on Publish/Subscribe/Request + JetStream consumer paths |
| `OTEL_NATS_PROPAGATION_ENABLED` | otel-nats | module propagation | OFF | final say on W3C `traceparent` / `tracestate` header inject (publish) + extract (subscribe). When OFF (default), traced impl still emits wrapper spans but skips header propagation. **Default-behaviour change from v0.3.x** — deployments previously relying on implicit injection MUST set this to a truthy value. |
| `OTEL_GORILLA_WS_TRACING_ENABLED` | otel-gorilla-ws | module tracing | OFF | wrapper send/receive spans + envelope wrap/unwrap (subject to subprotocol negotiation) |

3-tier for mongo + nats; 2-tier for ws. WS keeps 2-tier because envelope construction is inline in `marshalWire` — no "spans-on / wire-propagation-off" mode that fits the existing wire format; the subprotocol-negotiation runtime check serves the same purpose as a propagation flag.

`flags.EnvEnabled(name)` (internal helper, byte-identical across all four modules' `internal/flags/flags.go`) returns `false` when a var is absent. For otel-mongo, `WithTracePropagationEnabled` only overrides the propagation default while both tracing gates are on; it **cannot** enable propagation when global or module tracing is off.

### Deliver Spans

All three transports implement an optional "deliver span" pattern: a synthetic span is created with a service name equal to the system identifier (`nats://host:port`, `mongodb://host:port`). This creates a visible broker node in the service graph. For otel-mongo, deliver spans require **both** `mongoTracingEnabled()` to return true AND `OTEL_EXPORTER_OTLP_ENDPOINT` to be set — the function checks `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED`.

### Consumer Context

Subscribers always receive a `MsgWithContext` (NATS/JetStream) or a new `context.Context` return value (WebSocket `ReadMessage`) carrying the extracted remote trace. This context must be threaded into downstream calls to continue the trace chain.

### Span Links vs. Parent-Child

Async consumers (NATS subscribers, MongoDB change stream readers, WebSocket readers) use **span links** rather than parent-child relationships to connect to the producer span. This is intentional — preserves causality without implying synchronous nesting.

### Disabled-mode invariant (0.3.0+)

When any feature flag returns false, **no OTel SDK code path may run**: no `tracer.Start` on a real tracer, no `sdktrace.NewTracerProvider`, no `otlptracegrpc.New` / `otlptracehttp.New`, no `[]attribute.KeyValue` build, no propagator inject/extract. Two enforcement patterns coexist:

**1. Strategy split (preferred — otel-mongo Collection / Cursor / SingleResult / ChangeStream).** The facade type holds an `impl` interface satisfied by either `internal/direct.X` (passthrough) or `internal/traced.X` (instrumented). Construction picks the impl once; per-method runtime gates disappear. `internal/direct/*.go` imports no `go.opentelemetry.io/otel/sdk/*` and no `otel/exporters/*` — the disabled path is **compiler-enforced** by package boundary.

**2. Cached gate (otel-nats, otel-gorilla-ws, and otel-mongo Client/Database).** Connect/constructor reads env once → caches `tracingEnabled bool` on the wrapper struct. Every public method starts with `if !c.tracingEnabled { /* delegate to native */ }`. Reviewer-enforced. Planned migration to strategy split tracked in `DIRECTORY_LAYOUT_PLAN.html` at module root.

Independent of pattern:
- For otel-mongo, `Connect` substitutes `noop.NewTracerProvider()` when disabled so any stray `tracer.Start` is inert.
- Deliver-tracer init (`initNATSProvider`, `initMongoProvider`) is gated behind the same `tracingEnabled` check — no exporter or batched SDK provider is spun up when disabled.

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
    │              # semconv.go, tracing.go, bulkwrite.go — helpers used by both paths
    ├── direct/    # collection.go cursor.go singleresult.go changestream.go
    │              # NO go.opentelemetry.io/otel/sdk/* or otel/exporters/* imports
    └── traced/    # collection.go cursor.go singleresult.go changestream.go
                   # full OTel SDK access
```

Key rules:
- `internal/shared/impls.go` declares the polymorphic interfaces (`CursorImpl`, `SingleResultImpl`, `ChangeStreamImpl`) satisfied by both `internal/direct.X` and `internal/traced.X`. Facade `Cursor` / `SingleResult` / `ChangeStream` hold an `impl shared.XImpl` field.
- Facade `collectionImpl` interface returns raw driver types (`*mongo.Cursor`, `*mongo.SingleResult`, `*mongo.ChangeStream`) + `shared.XImpl` — the impl packages never need to import the facade, preventing any facade ↔ internal cycle. Facade methods wrap raw types into facade wrappers (`&Cursor{Cursor: raw, impl: cImpl}`).
- `internal/traced.Collection` has **exported fields** (`Coll`, `Tracer`, `Propagator`, `PropagationEnabled`, `DeliverTracer`, `ServerAddr`, `ServerPort`) and exported `StartDeliverSpan` so facade-package tests can build literals and call them directly.
- v1/v2 parity extends to `internal/{direct,traced,shared}/`. The helpers in `internal/shared/{bulkwrite.go,semconv.go,tracing.go,impls.go}` are intentionally duplicated across modules (separate `internal/` trees cannot share). Drift-check CI step planned (`DIRECTORY_LAYOUT_PLAN.html` §6).

### Propagation flag caching (otel-mongo)

`ContextFromDocument` / `ContextFromRawDocument` (`tracing.go`, both v1 and v2) call `cachedPropagationEnabled()`, which reads env **once** via `sync.Once` and stores in `atomic.Bool` (`env_flags.go`). The cached value reflects the full gate: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED` AND `OTEL_MONGO_PROPAGATION_ENABLED`. **Env changes after first call are ignored.** Tests that toggle any of those three vars via `t.Setenv` **must** call `resetPropEnabledCacheForTest()` after the Setenv to reset the cache. Helpers `enableTracing` / `enableDocumentPropagation` in `tracing_test.go` already invoke reset + `t.Cleanup` (and `enableDocumentPropagation` now sets all three flags). Do **not** add `t.Parallel()` to tests that touch these env vars — the reset is not parallel-safe.

### `oteljetstream.MessageBatch.Stop()`

`MessageBatch` interface (`oteljetstream/consumer.go`) includes a `Stop()` method (added 0.3.0; **breaking** for custom implementations). Callers that drain `Messages()` to channel close need not call it; callers that `break` / `return` early **must** `defer batch.Stop()` to release the internal goroutine and end the in-flight span. The disabled-tracing path uses `directMessageBatch` (no spans, no attributes, but still 1 goroutine for `jetstream.Msg → Msg` type adaptation).

## Module Layout

Canonical directory tree shared by all four modules (`otel-mongo/otelmongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`). Aligned with `golang-standards/project-layout` and the wider Go community convention (kubernetes, grpc-go, helm, cobra). Per-module READMEs reference this section rather than duplicate the tree.

```
<module>/                       # module root (one go.mod per module)
├── go.mod / go.sum
├── README.md / README.zh-TW.md
├── CHANGELOG.md                # release notes (when module is published)
├── doc.go                      # package overview shown by `go doc` and pkg.go.dev
├── version.go                  # `Version()` and `instrumentationVersion` const
├── <facade>.go                 # public types and constructors (conn.go / collection.go / ...)
├── tracing.go                  # public tracing helpers reused by callers
├── env_flags.go                # gate composition, calls into internal/flags
├── options.go                  # functional `With*` options
├── internal/                   # compiler-enforced privacy
│   ├── flags/                  # shared gate helpers — BYTE-IDENTICAL across modules (CI drift-check)
│   ├── shared/                 # interfaces + helpers reused by both impls
│   ├── direct/                 # disabled-mode impls — ZERO `otel/sdk` / `otel/exporters` imports
│   └── traced/                 # enabled-mode impls — full instrumentation
├── examples/                   # runnable demos, plural per Go convention
│   └── <demo>/main.go          # each demo is its own Go module (separate go.mod)
└── tests/                      # integration tests
    └── integration/            # separate Go module (testcontainers does not pollute root closure)
```

Subpackage names are **fixed** — `flags`, `shared`, `direct`, `traced`. No synonyms (`gate`, `common`, `disabled`, `instrumented`). Module-root `.go` files MAY only define exported identifiers consumed by users of the module; purely-internal helpers move under `internal/`.

The `internal/direct/` package boundary is **load-bearing**: it MUST NOT import `go.opentelemetry.io/otel/sdk/*` or `go.opentelemetry.io/otel/exporters/*` so the compiler can prove no SDK code is reachable on the disabled call path. Enforced by the `drift-check` CI job (`.github/workflows/ci.yml`).

Two pattern variants coexist for the strategy-split decision:

| Variant | Used by | Rationale |
|---|---|---|
| **Full strategy-split** — facade holds `impl <X>Impl` interface; `internal/direct.X` + `internal/traced.X` packages | otel-mongo Collection / Cursor / SingleResult / ChangeStream; otel-gorilla-ws Conn | Many public methods diverge on instrumentation — package boundary pays its weight |
| **Nullable traced-pointer** — facade holds `traced *traced.XState`; nil ⇔ disabled; constructor-site `if d.traced == nil { direct } else { traced }` selection | otel-mongo Client / Database | Only one truly instrumentation-divergent method per type; field-duplication of full split is overkill |
| **File-level split (transitional)** — `conn_direct.go` / `conn_traced.go` in same package; constructor picks once | otel-nats Conn / oteljetstream Consumer / MessageBatch | Functional intent met (no per-call gate); package-boundary upgrade tracked as follow-up |

See `otel-mongo-flag-wiring`, `otel-nats-flag-wiring`, `otel-gorilla-ws-flag-wiring` spec for per-module Requirement text.

## Versioning

Each module is tagged independently as `<module>/v<x.y.z>`. Version strings live in:

- `otel-nats/otelnats/conn.go` — `instrumentationVersion` const
- `otel-mongo/otelmongo/version.go` — `instrumentationVersion` const
- `otel-mongo/v2/version.go` — `instrumentationVersion` const
- `otel-gorilla-ws/version.go` — return literal from `Version()`

Bump on any code change to a module before pushing release tag. Module pre-1.0 (`0.x.y`): minor bump allowed for breaking changes.

## Module-Specific Notes

### otel-mongo

- `_oteltrace` field adds ~100–120 bytes per document. Schema-aware callers can use `SkipDBOperationsExporter` to suppress noisy spans (e.g., `getMore`).
- Use `Cursor.DecodeWithContext(ctx, v)` (not `Decode`) when reading in a change-stream context — it extracts the trace from the document and links spans correctly.
- `ContextFromDocument(ctx, doc)` extracts trace from an already-decoded document map; it respects the same propagation env gates as the Collection wrapper (not a bypass).
- **Strategy-split layout:** Collection / Cursor / SingleResult / ChangeStream all live in `internal/{direct,traced}/` (see *Strategy-split layout (otel-mongo)* above). Client and Database still use the cached-gate pattern.
- **v1 and v2 parity rule:** `otelmongo/` (v1) and `v2/` are parallel implementations. All logic changes — new flags, new fields, new inject/extract paths, new strategy methods — must be applied to **both** sub-packages identically, including their `internal/{direct,traced,shared}/` trees. Run lint and tests for both when either is touched.

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
