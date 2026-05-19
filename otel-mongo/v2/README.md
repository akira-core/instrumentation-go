# otel-mongo/v2

OpenTelemetry wrapper around the [MongoDB Go Driver **v2**](https://www.mongodb.com/docs/drivers/go/current/). Sibling of [otel-mongo (v1)](../README.md). Injects **W3C Trace Context** into documents on write (`_oteltrace` field) and restores it on read so the same trace can be followed across services. Per [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation): the package accepts **TracerProvider** and **Propagators** via options; it does **not** provide InitTracer. Set the global provider and propagator at process startup.

```go
import "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"
```

Public API surface mirrors the v1 wrapper (`Client`, `Database`, `Collection`, `Cursor`, `SingleResult`, `ChangeStream`, `ContextFromDocument`, `ContextFromRawDocument`) — diffs documented under **v1 vs v2 API Differences**.

---

## Quick start

```go
import (
    "context"
    otelmongo "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"
    "go.mongodb.org/mongo-driver/v2/mongo/options"
)

client, err := otelmongo.Connect(options.Client().ApplyURI(uri))
if err != nil { log.Fatal(err) }
defer client.Disconnect(context.Background())

coll := client.Database("mydb").Collection("mycoll")
// InsertOne / Find / UpdateOne / DeleteOne / Aggregate / BulkWrite / Watch ...
// _oteltrace injection + extraction happen automatically when feature flags are on
```

`ConnectWithOptions(traceOpts []ClientOption, opts ...*options.ClientOptions)` accepts `WithTracerProvider(tp)`, `WithPropagators(p)`, `WithTracePropagationEnabled(bool)`.

---

## Tracing feature flags

Identical surface to v1 — one global switch and two module switches; **all default to OFF when unset**:

| Variable | Tier | Default | Effect |
|---|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master | OFF | hard prerequisite for every per-module flag |
| `OTEL_MONGO_TRACING_ENABLED` | module tracing | OFF | wrapper CLIENT spans + deliver-span path; noop vs real tracer |
| `OTEL_MONGO_PROPAGATION_ENABLED` | module propagation | OFF | `_oteltrace` inject/extract on wrapped Collection/Cursor/ChangeStream + `ContextFromDocument` / `ContextFromRawDocument` |

Truthy = any value other than `0`, `false`, `no`, `off` (case-insensitive, whitespace-trimmed). Cached for the process lifetime via `sync.Once`; env changes after the first gate read are ignored.

Priority:
1. Global off → every module flag and `WithTracePropagationEnabled(true)` is force-disabled. No wrapper spans, no `_oteltrace` inject/extract.
2. Global on + `OTEL_MONGO_TRACING_ENABLED` off → wrapper CLIENT spans use noop tracer; `_oteltrace` inject/extract disabled. `WithTracePropagationEnabled(true)` cannot bypass.
3. Both tracing gates on → `OTEL_MONGO_PROPAGATION_ENABLED` becomes the default for `_oteltrace`; `ConnectWithOptions` + `WithTracePropagationEnabled` overrides per-client.

When tracing flags are unset/disabled, the wrapper does not emit Mongo CLIENT spans **and** documents are written without `_oteltrace`. Deliver spans additionally require `OTEL_EXPORTER_OTLP_ENDPOINT`.

---

## Disabled-mode behaviour

When any required gate is OFF, the v2 wrapper is **observationally indistinguishable** from raw `go.mongodb.org/mongo-driver/v2/mongo`:
- No `tracer.Start` on a real tracer (substituted with `noop.NewTracerProvider()`).
- No `propagator.Inject` / `propagator.Extract`.
- No `_oteltrace` field written to documents.
- No deliver-span goroutines.
- Direct-mode impls under `internal/direct/` carry zero `otel/sdk` / `otel/exporters` imports — compiler-enforced.

---

## Internals overview

```
otel-mongo/v2/
├── client.go database.go collection.go cursor.go results.go     # facade
├── tracing.go env_flags.go version.go doc.go                    # facade helpers
├── go.mod                                                        # module .../otel-mongo/v2
└── internal/
    ├── flags/              # shared gate helper (byte-identical across all four modules)
    ├── shared/             # impls.go (CursorImpl / ChangeStreamImpl interfaces),
    │                       # bulkwrite.go semconv.go tracing.go — helpers used by both paths
    ├── direct/             # disabled-mode impls — ZERO otel/sdk or otel/exporters imports
    └── traced/             # enabled-mode impls — full instrumentation + ClientState / DatabaseState
```

Client + Database use the **nullable traced-pointer** variant (`facade.Client{*mongo.Client; traced *traced.ClientState}` — `nil` ⇔ disabled). Collection / Cursor / SingleResult / ChangeStream use the **full strategy-split** variant (facade holds `impl <X>Impl` interface). Compile-time assertions in `cursor.go`, `results.go`, `collection.go` (`var _ shared.CursorImpl = (*direct.Cursor)(nil)` etc.) fail the build if any impl misses a method.

---

## v1 vs v2 API Differences

| Difference | v1 (`otelmongo`) | v2 (`.../v2`) |
|------------|------------------|---------------|
| `Connect` signature | `Connect(ctx, opts...)` | `Connect(opts...)` |
| `NewClient` signature | `NewClient(ctx, uri, traceOpts...)` | `NewClient(uri, traceOpts...)` |
| `Distinct` return | `([]interface{}, error)` | `*mongo.DistinctResult` |
| `StartSession` return | `mongo.Session, error` | `*mongo.Session, error` |
| `Cursor.DecodeWithContext` | Creates INTERNAL span + new TraceID | Pure context enrichment (no extra span) |
| `Connect` server address | Not parsed | Auto-parses URI for `server.address` attribute |

---

## Important notes

### `_oteltrace` field in documents

Every `InsertOne`, `InsertMany`, `ReplaceOne`, and `UpdateOne` / `UpdateMany` / `UpdateByID` call injects a reserved `_oteltrace` subdocument into the document (or into `$set` for operator updates) when **all** of the following hold:

1. propagation gates are on (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_MONGO_TRACING_ENABLED` + `OTEL_MONGO_PROPAGATION_ENABLED`), and
2. `ctx` carries a valid `SpanContext`, and
3. that `SpanContext.IsSampled() == true`.

```bson
{ "traceparent": "00-<traceId>-<spanId>-01", "tracestate": "" }
```

Unsampled writes do **not** embed `_oteltrace` — the document goes to MongoDB unchanged and avoids the ~100-byte propagation overhead. If your deployment uses tail-based sampling or forced sampling via debug headers, ensure your sampler decides at the write site, not after.

~100–120 bytes per document. Add `_oteltrace` to your projection allowlist if you use strict schema validation.

### `Cursor.DecodeWithContext` vs `Decode`

`DecodeWithContext(ctx, v)` extracts the producer's trace context from `_oteltrace` and returns an **enriched context** (no extra span — pure enrichment in v2). Plain `Decode(v)` works exactly like the underlying driver's `Decode` and ignores `_oteltrace`.

### Span links on `FindOne`

`SingleResult.Decode` adds a **span link** (not a parent-child relationship) to the `_oteltrace` stored in the fetched document. The FindOne span ends when `Decode`, `Raw`, or `TraceContext` is first called. Call exactly one of these per `SingleResult`.

### Deliver Spans (Service Graph)

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, `otelmongo` creates synthetic CONSUMER spans representing MongoDB as a broker node in Grafana service graph. The server address is parsed from the URI provided via `options.Client().ApplyURI(uri)` — v2 auto-parses (v1 does not).

The endpoint must be a **full URL** for HTTP (e.g. `http://otel-collector:4318`) or **host:port** for gRPC (e.g. `otel-collector:4317`).

---

## Diagnostic logging

Uses [`log/slog`](https://pkg.go.dev/log/slog) — no output by default.

| Level | Events |
|-------|--------|
| `DEBUG` | Deliver tracer initialised successfully |
| `WARN` | OTLP exporter creation failure; resource creation failure |

Log entries use the `otelmongo:` prefix with `error`, `reason`, `service`, `endpoint` key-value pairs.

---

## Dependencies

- `go.mongodb.org/mongo-driver/v2`
- `go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo`
- `go.opentelemetry.io/otel` + SDK
- Go 1.24+

See `v2/go.mod` for full pinned versions.

---

## Versioning

Tagged as `otel-mongo/v2/v<x.y.z>`. Version constant lives in `version.go`. Bump on any code change before pushing release tag (per-package event-driven bump policy).
