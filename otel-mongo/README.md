# otel-mongo (otelmongo)

[繁體中文 (Traditional Chinese)](README.zh-TW.md)

---

OpenTelemetry wrapper around the [MongoDB Go Driver](https://www.mongodb.com/docs/drivers/go/current/). Injects **W3C Trace Context** into documents on write (`_oteltrace` field) and restores it on read so the same trace can be followed across services. Per [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation): the package accepts **TracerProvider** and **Propagators** via options; it does **not** provide InitTracer. Set the global provider and propagator at process startup (see **example/**).

Two driver versions are supported (Go convention: v2 lives under `/v2` for a clear import path):

| Import path | Driver | Use when |
|------------|--------|----------|
| `github.com/Marz32onE/instrumentation-go/otel-mongo/v2` | MongoDB Go Driver **v2** | New projects or v2 driver (recommended) |
| `github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo` | MongoDB Go Driver **v1** | Existing code using v1 driver |

Both packages expose the same API surface (Client, Collection, Cursor, ContextFromDocument, etc.) and the same `_oteltrace` document-level propagation.

---

## Layout

```
otel-mongo/
├── otelmongo/           # MongoDB driver v1 wrapper (root module)
│   ├── version.go, client.go, database.go, collection.go, cursor.go
│   ├── tracing.go, semconv.go, bulkwrite.go, results.go, filter_exporter.go
│   └── ...
├── v2/                  # MongoDB driver v2 wrapper (separate module, import .../v2)
│   ├── go.mod           # module .../otel-mongo/v2, requires go.mongodb.org/mongo-driver/v2
│   ├── version.go, client.go, database.go, collection.go, cursor.go
│   ├── tracing.go, semconv.go, bulkwrite.go, results.go, filter_exporter.go
│   └── *_test.go
├── example/             # TracerProvider + global + otelmongo (uses v2)
└── README.md
```

- **Trace storage:** Written/updated documents get a reserved **`_oteltrace`** field (W3C `traceparent` and optional `tracestate`). Use **ContextFromDocument(ctx, raw)** for raw BSON (e.g. change streams).
- **Two layers:** (1) **Driver:** Client uses contrib `otelmongo.NewMonitor` for connection/command spans. (2) **Document:** Collection CRUD injects `_oteltrace` on write and supports span links / propagation on read.

---

## Usage

### Tracing feature flags

`otel-mongo` (v1 + v2) supports one global switch and two module switches:

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global master switch)
- `OTEL_MONGO_TRACING_ENABLED` (wrapper **CLIENT** spans for this package, deliver-span path, and noop vs real tracer — driver/contrib command spans are separate)
- `OTEL_MONGO_PROPAGATION_ENABLED` (document `_oteltrace` injection/extraction on wrapped Collection/Cursor/ChangeStream, plus **ContextFromDocument** / **ContextFromRawDocument**)

Defaults: all disabled when unset. Values `false/0/no/off` disable.

Priority:
1. If global is disabled, mongo module flags and **`WithTracePropagationEnabled(true)`** do not enable document propagation (global cannot be overridden by that option).
2. If global is enabled, `OTEL_MONGO_TRACING_ENABLED` controls whether this package records wrapper CLIENT spans (when off, a noop tracer is used for those spans).
3. If global is enabled, `OTEL_MONGO_PROPAGATION_ENABLED` is the default for `_oteltrace`; **`WithTracePropagationEnabled`** in `ConnectWithOptions` overrides that default only while global stays on.

When tracing flags are unset/disabled, this package’s wrapper does not emit Mongo CLIENT spans to your configured TracerProvider (noop). Deliver spans still require tracing flags plus `OTEL_EXPORTER_OTLP_ENDPOINT` as documented below.

### 1. Initialize provider and propagator (application responsibility)

See **example/main.go**. In short: create TracerProvider (e.g. OTLP), set `otel.SetTracerProvider(tp)` and `otel.SetTextMapPropagator(prop)`, defer shutdown.

### 2. Connect and use

**MongoDB driver v2** (recommended; import path aligns with Go convention):

```go
import (
    "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"
    "go.mongodb.org/mongo-driver/v2/mongo/options"
)

client, err := otelmongo.Connect(options.Client().ApplyURI(uri))
if err != nil { log.Fatal(err) }
defer client.Disconnect(ctx)

db := client.Database("mydb")
coll := db.Collection("mycoll")
// InsertOne, Find, UpdateOne, etc. handle _oteltrace automatically
```

**MongoDB driver v1** (same API, different import and Connect signature):

```go
import (
    "context"
    "github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo"
    "go.mongodb.org/mongo-driver/mongo/options"
)

client, err := otelmongo.Connect(ctx, options.Client().ApplyURI(uri))
if err != nil { log.Fatal(err) }
defer client.Disconnect(ctx)

db := client.Database("mydb")
coll := db.Collection("mycoll")
// Same CRUD and _oteltrace behaviour as v2 wrapper
```

Optional: **ConnectWithOptions(ctx, traceOpts, mongoOpts)** (v1) or **ConnectWithOptions(traceOpts, mongoOpts)** (v2) with **WithTracerProvider(tp)** or **WithPropagators(p)**.

### 3. Restore trace from document (e.g. change streams)

Requires the same propagation env gates as writes (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_MONGO_PROPAGATION_ENABLED`, or global on plus `WithTracePropagationEnabled(true)` via `ConnectWithOptions`). When the gates are off, `ContextFromDocument`/`ContextFromRawDocument` return zero/unchanged — existing callers that ignored the `ok` return value will silently no-op.

```go
fullDoc := changeStreamEvent.FullDocument
if sc, ok := otelmongo.ContextFromDocument(ctx, fullDoc); ok {
	next := trace.ContextWithRemoteSpanContext(ctx, sc)
	_ = next // use next for downstream spans or forwarding (e.g. to NATS)
}
```

### 4. Tests

```go
otel.SetTracerProvider(trace.NewTracerProvider())
client, err := otelmongo.Connect(opts)
```

### 5. Reduce noisy driver spans (e.g. `getMore`)

The MongoDB driver monitor (`contrib otelmongo.NewMonitor`) emits command spans for all operations, including frequent cursor operations like `getMore`.

Use `SkipDBOperationsExporter` to drop selected DB operation spans before export:

```go
exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint))
if err != nil { log.Fatal(err) }

// Drop db.operation.name in skip list (case-insensitive).
exp = otelmongo.SkipDBOperationsExporter(exp, []string{"getMore"})

tp := sdktrace.NewTracerProvider(
    sdktrace.WithBatcher(exp),
    sdktrace.WithResource(res),
)
otel.SetTracerProvider(tp)
```

This only filters exported spans; client CRUD behavior and `_oteltrace` propagation are unchanged.

---

## API summary

| Item | Description |
|------|-------------|
| **Connect / ConnectWithOptions** | Uses `otel.GetTracerProvider()` unless **WithTracerProvider(tp)** is passed. |
| **NewClient** | Same; accepts optional **WithTracerProvider**, **WithPropagators**. |
| **ContextFromDocument** | Restores trace context from document’s `_oteltrace` (e.g. for change streams). |
| **ScopeName / Version()** | Used when creating Tracer (OTel contrib guideline). |
| **SkipDBOperationsExporter** | Wraps a `SpanExporter`; drops spans whose `db.operation.name` is in the skip list (export-only). |

---

## Deliver Spans (Service Graph)

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, otelmongo creates synthetic "deliver" spans representing MongoDB as a broker node in Grafana service graph. Both `Connect` and `NewClient` support this — the server address is parsed from the URI provided via `options.Client().ApplyURI(uri)`.

The endpoint must be a **full URL** for HTTP (e.g. `http://otel-collector:4318`) or **host:port** for gRPC (e.g. `otel-collector:4317`). Bare hostnames without scheme or port are not supported.

Deliver spans are emitted for all CRUD operations (insert, find, update, delete, replace, aggregate, bulk write, distinct, count, etc.) as well as cursor decode and change stream paths.

### Write path

```
InsertOne (CLIENT, api)
  └── db.coll deliver (CONSUMER, mongodb)  ← injected into _oteltrace
```

### Read / delete path

```
FindOne / DeleteOne / ... (CLIENT, api)
  └── db.coll deliver (CONSUMER, mongodb)
```

### Change stream path

```
db.coll deliver (PRODUCER, mongodb)  ← links to producer deliver
  └── watch coll (CONSUMER, dbwatcher) ← child of deliver
```

### Resulting service graph

```
api ──► mongodb ──► dbwatcher
```

---

## v1 vs v2 API Differences

| Difference | v1 (`otelmongo`) | v2 (`.../v2`) |
|------------|------------------|---------------|
| `Connect` signature | `Connect(ctx, opts...)` | `Connect(opts...)` |
| `NewClient` signature | `NewClient(ctx, uri, traceOpts...)` | `NewClient(uri, traceOpts...)` |
| `Distinct` return | `([]interface{}, error)` | `*mongo.DistinctResult` |
| `StartSession` return | `mongo.Session, error` | `*mongo.Session, error` |
| `Cursor.DecodeWithContext` | Creates INTERNAL span + new TraceID | Pure context enrichment (no extra span) |
| `Connect` server address | Not parsed (no `server.address` attribute) | Auto-parses URI |

---

## Important notes

### `_oteltrace` field in documents

Every `InsertOne`, `InsertMany`, `ReplaceOne`, and `UpdateOne`/`UpdateMany`/`UpdateByID` call injects a reserved **`_oteltrace`** field into the document (or into `$set` for operator updates) when an active OTel span is present in the context. This field is a subdocument:

```bson
{ "traceparent": "00-<traceId>-<spanId>-01", "tracestate": "" }
```

**Impact on your schema:** any application or query that uses strict schema validation or projects specific fields will see this extra field. Add `_oteltrace` to your allowlist or projection if needed.

**Impact on document size:** approximately 100–120 bytes per document. When there is no active span (e.g. in tests without a TracerProvider), no `_oteltrace` field is injected.

### Global OTel state

`WithTracerProvider` and `WithPropagators` (passed to `ConnectWithOptions`) are stored on the `Client` only; they do **not** call `otel.SetTracerProvider` / `otel.SetTextMapPropagator`. If you omit them, the client uses `otel.GetTracerProvider()` and `otel.GetTextMapPropagator()` at connect time. For most applications, set the globals once at startup and call `Connect` / `NewClient` without trace options.

### `NewCollection` vs `Connect`

`NewCollection` sets **document** `_oteltrace` behaviour from the same env gates as `Connect` (global + `OTEL_MONGO_PROPAGATION_ENABLED`). There is no per-collection functional option for propagation; use **`ConnectWithOptions`** with **`WithTracePropagationEnabled`** when you need to override the env default for a client.

### DecodeWithContext vs Decode on Cursor

`Cursor.DecodeWithContext` extracts the producer's trace context from `_oteltrace` and returns an enriched context — use it when you need to link downstream work to the document's origin trace. Plain `Cursor.Decode` works exactly like the underlying driver's `Decode` and ignores `_oteltrace`.

### Span links on FindOne

`SingleResult.Decode` adds a **span link** (not a parent-child relationship) to the `_oteltrace` stored in the fetched document. The FindOne span ends when `Decode`, `Raw`, or `TraceContext` is first called. Call exactly one of these per `SingleResult`.

---

## Diagnostic logging

Uses [`log/slog`](https://pkg.go.dev/log/slog) — no output by default.

| Level | Events |
|-------|--------|
| `DEBUG` | Deliver tracer initialised successfully (logs `service` and `endpoint`) |
| `WARN` | OTLP exporter creation failure; resource creation failure |

Enable with a debug-level slog handler at startup:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Log entries use the `otelmongo:` prefix with `error`, `reason`, `service`, and `endpoint` key-value pairs.

---

## Dependencies

- **v2** (`.../otel-mongo/v2`): `go.mongodb.org/mongo-driver/v2`, `go.opentelemetry.io/contrib/instrumentation/.../v2/mongo/otelmongo`, `go.opentelemetry.io/otel` and SDK. See `v2/go.mod`.
- **otelmongo** (v1, root): `go.mongodb.org/mongo-driver` v1, `go.opentelemetry.io/contrib/instrumentation/.../mongo/otelmongo`, `go.opentelemetry.io/otel` and SDK. See root `go.mod`.
- Go 1.24+
