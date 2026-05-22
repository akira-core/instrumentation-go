# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Sibling module of `otel-nats`, `otel-gorilla-ws`. The repo-root `pkg/instrumentation-go/CLAUDE.md` covers cross-module conventions (wrapper pattern, env-flag matrix, semconv, deliver-span design, versioning policy, `internal/flags/` byte-identical rule, strategy-split layout). This file only documents what is specific to `otel-mongo`.

## Module identity

This directory hosts **two independent Go modules** that wrap MongoDB driver v1 and v2 respectively. They share a directory tree, a README, and this file, but they are tagged, versioned, and released independently:

| Module path | Driver | go.mod | Version constant |
|---|---|---|---|
| `github.com/Marz32onE/instrumentation-go/otel-mongo` | mongo-driver v1 (`go.mongodb.org/mongo-driver` 1.17.x) | `./go.mod` | `otelmongo/version.go` |
| `github.com/Marz32onE/instrumentation-go/otel-mongo/v2` | mongo-driver v2 (`go.mongodb.org/mongo-driver/v2` 2.6.x) | `./v2/go.mod` | `v2/version.go` |

Both currently at `instrumentationVersion = "0.5.2"`. The `/v2` suffix follows the standard Go module convention — it is a separate module, not a subpackage.

The v1 wrapper code lives in **`otelmongo/`** (note the directory name does NOT match the import-path suffix — root module imports as `.../otel-mongo/otelmongo`). The v2 wrapper code lives in **`v2/`** at the module root (imports as `.../otel-mongo/v2`).

`examples/basic/`, `tests/integration/`, and `v2/tests/integration/` are each **separate Go modules** with their own `go.mod`. Testcontainers dependencies live only inside `tests/integration/` modules to keep them out of consumers' transitive closure.

## Common commands

All commands must be run **inside the module directory** being changed — never from the parent.

```bash
# v1 wrapper (run from otel-mongo/)
go build ./...
go test -v -race ./...
go test -v -race -run TestName ./otelmongo/...           # single test
golangci-lint run ./...                                  # v2 syntax required

# v2 wrapper (run from otel-mongo/v2/)
cd v2 && go build ./... && go test -v -race ./... && golangci-lint run ./...

# Integration tests — testcontainers spawns standalone mongo (no replica set needed)
cd tests/integration   && go test -v -race ./...          # v1
cd v2/tests/integration && go test -v -race ./...         # v2

# Example (separate module; cannot be run from module root)
cd examples/basic && go run .
```

All three of `go build`, `go test -race`, `golangci-lint run` MUST pass with 0 issues on **both** v1 and v2 before any commit (see v1/v2 parity rule below). `goimports` requires: stdlib group → blank line → third-party group → blank line → local prefix `github.com/Marz32onE/instrumentation-go`.

## Architecture specifics

### Two strategy variants in the same module

Different facade types use different disabled-mode enforcement patterns — do not unify them blindly:

| Facade type | Variant | Storage |
|---|---|---|
| `Client`, `Database` | **Nullable traced-pointer** | `Client{ *mongo.Client; traced *traced.ClientState }` — `traced == nil` ⇔ disabled |
| `Collection`, `Cursor`, `SingleResult`, `ChangeStream` | **Full strategy-split (package boundary)** | Facade holds `impl <X>Impl` interface satisfied by `internal/direct.X` OR `internal/traced.X` |

`internal/direct/` is **forbidden** from importing `go.opentelemetry.io/otel/sdk/*` or `go.opentelemetry.io/otel/exporters/*`. This is enforced at compile time by the package boundary and double-checked by the repo-level `drift-check` CI job. When adding code under `internal/direct/`, if you find yourself reaching for the SDK, the code belongs in `internal/traced/` instead.

`internal/shared/impls.go` declares the polymorphic interfaces (`CollectionImpl`, `CursorImpl`, `SingleResultImpl`, `ChangeStreamImpl`). Methods on `CollectionImpl` return **raw driver types plus the polymorphic Cursor/SingleResult/ChangeStream impls** so `internal/{direct,traced}` never need to import the facade — this is what prevents a facade ↔ internal import cycle. The facade wraps raw types (`&Cursor{Cursor: raw, impl: cImpl}`) at the call site.

`internal/traced.Collection` has **exported fields** (`Coll`, `Tracer`, `Propagator`, `PropagationEnabled`, `DeliverTracer`, `ServerAddr`, `ServerPort`) and an exported `StartDeliverSpan` so facade-package tests can build literals and call them directly.

### Adding a public method to Collection / Cursor / SingleResult / ChangeStream

Touch FOUR files in lockstep (per module — and mirror in the v1↔v2 sibling, see parity rule):

1. Add signature to the interface in `internal/shared/impls.go` (or to the local `collectionImpl` in `collection.go` if the method returns facade-wrapper types).
2. Implement passthrough in `internal/direct/<file>.go` — no `otel/sdk` or `otel/exporters` imports.
3. Implement instrumented version in `internal/traced/<file>.go`.
4. Add the single-line facade method (`c.impl.X(...)` or wrapping raw return into facade type) in `collection.go` / `cursor.go` / `results.go`.

Compile-time assertions (`var _ shared.CursorImpl = (*direct.Cursor)(nil)`, `var _ shared.CursorImpl = (*traced.Cursor)(nil)`, plus the parallel `collectionImpl` assertions in `collection.go`) fail the build if any impl misses a method.

### v1 and v2 parity rule (CRITICAL)

`otelmongo/` (v1) and `v2/` are **parallel implementations of the same wrapper contract**. Any logic change — new flags, new fields on `traced.ClientState`, new inject/extract paths, new strategy methods, new env-gate semantics, new helpers under `internal/shared/` — must be applied to **both** sub-packages identically, including their `internal/{direct,traced,shared}/` trees. The `internal/` trees are **intentionally duplicated** (separate `internal/` trees cannot share across modules). Run `go build`, `go test`, `golangci-lint` for both v1 AND v2 when either is touched. Drift between v1 and v2 is a bug.

The README's *v1 vs v2 API Differences* table documents intentional differences — anything not in that table should be byte-identical in behaviour.

### Propagation gate caching

`ContextFromDocument` / `ContextFromRawDocument` (`tracing.go` in both v1 and v2) sit in the change-stream hot loop, so the **three-tier gate is cached**:

`env_flags.go` declares `propEnabledGate = flags.NewGate(mongoPropagationEnabled)`. The first call reads env via `sync.Once`, stores the boolean in `atomic.Bool`, and every subsequent call is a single atomic load. The cached value reflects the **full three-tier** resolution: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED` AND `OTEL_MONGO_PROPAGATION_ENABLED`.

**Env changes after the first call are ignored for the rest of the process.** Tests that flip any of those three vars via `t.Setenv` MUST call `resetPropEnabledCacheForTest()` after the Setenv (defined in `env_flags_testhelper_test.go`). The helpers `enableTracing` and `enableDocumentPropagation` in `tracing_test.go` already wrap this with `t.Cleanup`. Do **NOT** add `t.Parallel()` to tests that touch these env vars — the reset is process-global.

### Three-tier gate semantics

The `WithTracePropagationEnabled` option **cannot bypass a disabled tracing gate** — see `resolveDocumentPropagation` in `env_flags.go`. The decision tree:

1. If global OR module tracing is OFF → propagation is forced OFF, regardless of any option or env.
2. If both tracing gates are ON → `WithTracePropagationEnabled` (option) wins if set; otherwise `OTEL_MONGO_PROPAGATION_ENABLED` is the default.

Rationale (documented in README): turning off Mongo tracing also turns off propagation so callers have a single predictable kill switch. There is no "wrapper spans noop while documents still carry `_oteltrace`" mode.

### Sampling gate on write (v0.5.2)

`InsertOne` / `InsertMany` / `ReplaceOne` / `UpdateOne` / `UpdateMany` / `UpdateByID` only inject `_oteltrace` when `ctx` carries a valid `SpanContext` **AND** `SpanContext.IsSampled() == true`. Unsampled writes skip `_oteltrace` entirely — the document goes to MongoDB unchanged, no ~100-byte propagation overhead. Tail-based sampling and debug-header forced sampling must therefore decide at the **write site**, not after the fact. When adding new write paths, mirror the `IsValid() || !IsSampled()` consolidated guard already present (see commit `02168ff` in v2 and the v1 sibling).

### Deliver spans and `OTEL_EXPORTER_OTLP_ENDPOINT`

Deliver spans require **both** `mongoTracingEnabled()` true AND `OTEL_EXPORTER_OTLP_ENDPOINT` set. Endpoint with `http://` / `https://` prefix → OTLP HTTP exporter; bare `host:port` → OTLP gRPC. Bare hostnames without scheme or port are unsupported.

Service name comes from `parseServerFromClientOptions(opts)` (v2 auto-parses URI; v1 does not — see README *v1 vs v2 API Differences* table). The independent deliver-TP is shut down in `Client.Disconnect` — tests that build a `Client` directly bypassing `Connect` and skip `Disconnect` will leak the deliver-TP goroutine.

### `Connect` signature drift between v1 and v2

v1 takes `ctx` because mongo-driver v1's `mongo.Connect` does; v2 dropped it because mongo-driver v2's `mongo.Connect` did. This is the most common source of v1↔v2 mistakes when copy-pasting code:

```go
// v1
client, err := otelmongo.Connect(ctx, options.Client().ApplyURI(uri))
clientV1, err := otelmongo.ConnectWithOptions(ctx, traceOpts, mongoOpts)

// v2 — no ctx
client, err := otelmongoV2.Connect(options.Client().ApplyURI(uri))
clientV2, err := otelmongoV2.ConnectWithOptions(traceOpts, mongoOpts)
```

`NewClient` follows the same split (v1 takes `ctx`, v2 does not).

### `DecodeWithContext` behavioural difference (intentional)

- **v1 `Cursor.DecodeWithContext`**: creates an INTERNAL span with a new TraceID (legacy behaviour).
- **v2 `Cursor.DecodeWithContext`**: pure context enrichment, no extra span.

This is one of the documented intentional differences in README. Do **not** "fix" this drift without an explicit decision to change the v1 contract.

### `Distinct` return type differs

v1: `Distinct(...) ([]interface{}, error)`. v2: `Distinct(...) *mongo.DistinctResult`. Mirrors upstream driver — do not unify.

### `_oteltrace` document field

Reserved field name. Subdocument shape: `{ "traceparent": "00-<traceId>-<spanId>-01", "tracestate": "" }`. Adds ~100–120 bytes per document. Stripped on read. Schema-strict consumers MUST add `_oteltrace` to their allowlist or projection.

`extractMetadataFromMap` / `extractMetadataFromBsonD` / `ExtractMetadataFromRaw` (in `internal/shared/`) handle the read-side fast paths. `bson.D` lookup uses **reverse scan** because `_oteltrace` is conventionally appended at the end of the document. When adding new fast paths, the contract is **strict** — unrecognized inner shapes do NOT fall back to slow path (this was tried in v0.5.1 dev and reverted; see commits `1748`–`1751` in memory). The `_oteltrace` field, if present, must conform exactly.

### `SingleResult` lifecycle

`SingleResult.Decode` adds a **span link** (not parent-child) to the `_oteltrace` stored in the fetched document. The FindOne span ends when **`Decode`, `Raw`, or `TraceContext` is first called**. Call exactly one of these per `SingleResult`. When adding methods that touch `SingleResult`, ensure the end-on-first-touch invariant holds.

### `SkipDBOperationsExporter`

Export-only filter — wraps a `SpanExporter` and drops spans where `db.operation.name` is in the (case-insensitive) skip list. Primary use case: silencing high-volume `getMore` cursor spans. Does NOT affect client behaviour or `_oteltrace` propagation. Test changes here against both v1 and v2.

## Version bump checklist

Any code change to a module requires bumping that module's `instrumentationVersion`:
- v1: `otelmongo/version.go`
- v2: `v2/version.go`

Both should typically bump together (parity rule) before pushing release tags `otel-mongo/v<x.y.z>` and `otel-mongo/v2/v<x.y.z>`. Pre-1.0 (`0.x.y`), a minor bump is allowed for breaking changes. Update root-level `CHANGELOG.md` (if present, else README) for any default-behaviour change such as the v0.5.2 sampling gate.

## Local development against sibling consumers

The repo-root `otel-traces-test` services consume this module via `replace` directives in each service's `go.mod` pointing to `../pkg/instrumentation-go/otel-mongo` (v1) or `../pkg/instrumentation-go/otel-mongo/v2` (v2). After editing this module, no rebuild of this module is needed — just `go build` / `go test` in the consuming service. Note however that the `tests/integration/` sub-module has its own `go.mod` with mongo-driver pinned; if you bump mongo-driver in the parent, run `go mod tidy` inside `tests/integration/` to keep them aligned (this caused PR #16 CI to fail — see commit `d94ce42`).
