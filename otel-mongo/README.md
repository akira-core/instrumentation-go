# otel-mongo (otelmongo)

[繁體中文 (Traditional Chinese)](README.zh-TW.md)

---

OpenTelemetry wrapper around the [MongoDB Go Driver](https://www.mongodb.com/docs/drivers/go/current/). Injects **W3C Trace Context** into documents on write (`_oteltrace` field) and restores it on read so the same trace can be followed across services. Per [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation): the package accepts **TracerProvider** and **Propagators** via options; it does **not** provide InitTracer. Set the global provider and propagator at process startup (see **examples/**).

Two driver versions are supported (Go convention: v2 lives under `/v2` for a clear import path):

| Import path | Driver | Use when |
|------------|--------|----------|
| `github.com/akira-core/instrumentation-go/otel-mongo/v2` | MongoDB Go Driver **v2** | New projects or v2 driver (recommended) |
| `github.com/akira-core/instrumentation-go/otel-mongo/otelmongo` | MongoDB Go Driver **v1** | Existing code using v1 driver |

Both packages expose the same API surface (Client, Collection, Cursor, ContextFromDocument, etc.) and the same `_oteltrace` document-level propagation.

---

## Layout

```
otel-mongo/
├── otelmongo/           # MongoDB driver v1 wrapper (root module)
│   ├── version.go, client.go, database.go, collection.go, cursor.go
│   ├── tracing.go, results.go, env_flags.go
│   └── internal/
│       ├── shared/     # semconv.go, bulkwrite.go, tracing.go, impls.go — used by both direct and traced
│       ├── direct/     # passthrough impls (no otel/sdk imports) — used when tracing is disabled
│       └── traced/     # fully instrumented impls
├── v2/                  # MongoDB driver v2 wrapper (separate module, import .../v2)
│   ├── go.mod           # module .../otel-mongo/v2, requires go.mongodb.org/mongo-driver/v2
│   ├── version.go, client.go, database.go, collection.go, cursor.go
│   ├── tracing.go, results.go, env_flags.go
│   └── internal/        # shared/, direct/, traced/ — mirrors otelmongo/internal/ above
├── examples/             # TracerProvider + global + otelmongo (uses v2)
└── README.md
```

- **Trace storage:** Written/updated documents get a reserved **`_oteltrace`** field (W3C `traceparent` and optional `tracestate`). Use **ContextFromDocument(ctx, raw)** for raw BSON (e.g. change streams).
- **Two layers:** (1) **Client spans:** each Collection method (insert/find/update/delete/aggregate/distinct/bulkWrite/etc.) creates its own span directly in `internal/traced/collection.go`; a **chained** driver `CommandMonitor` (registered only when tracing is enabled, and chained after any monitor you set yourself) captures the real per-command server address for the span's `server.*` attributes. (2) **Document:** Collection CRUD injects `_oteltrace` on write and supports span links / propagation on read.

---

## Usage

### Tracing feature flags

```
tracing     = master && mongoTracing
propagation = tracing && mongoPropagation
```

Each switch resolves down a four-step ladder, first source with an opinion winning:

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

The relay is authoritative in **both** directions — it can disable a running module and enable one
the deployment left off. Safety comes from the defaults: the master switch
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` defaults to `true` and is a **veto** (only `false` has an
effect; it accepts no option), while every per-module switch defaults to **off**.

**The option sits below its environment variable**, reversing `0.7.0`. ``OTEL_MONGO_TRACING_ENABLED`=false` disables
this module even where the Go code passed `WithTracingEnabled(true)`, so an operator has a per-module
setting application code cannot override. With the variable unset the option decides, so two
connections in one process can still differ.

A switch is decided only by `1`/`true`/`yes`/`on` or `0`/`false`/`no`/`off`. Unset means "no opinion".
**Anything else — including the empty string — fails construction** with an error wrapping
`otelflags.ErrInvalidFlagValue`.

`WithTracingEnabled` does **not** pin anything: a wrapper carrying it resolves the master switch and
the relay on every operation.

The mutual-exclusion rule and both `Err*ConfigConflict` sentinels are **gone**: supplying an option
alongside its variable is ordinary configuration, and the variable wins.

Two module-specific points:

- **`_oteltrace` changes what is persisted.** Roughly 90 bytes per document, written by `InsertOne`,
  `InsertMany`, `UpdateOne`, `UpdateMany`, `UpdateByID`, `ReplaceOne` and `BulkWrite`. **Nothing
  removes it** — turning propagation off stops new writes but does not undo old ones; cleanup is a
  `$unset` migration. A collection with `$jsonSchema` + `additionalProperties: false` rejects every
  write while it is on. Re-injecting into a document that already has the field replaces it rather
  than duplicating it.
- **`ContextFromDocument` / `ContextFromRawDocument` carry no gate at all.** They start no span,
  write nothing, and initialise no part of the OTel SDK — they read a field out of a value you
  already hold. **A revocation does not stop them**, which makes them the supported way to keep
  trace linking while the library is silenced. `Cursor.DecodeAndTrace` /
  `ChangeStream.DecodeAndTrace` *are* gated, because each emits a span.

> Full reference — every resolution table, connecting a relay with no application code, revocation
> latency, per-service targeting, and the operational summary:
> **[docs/feature-flags.md](../docs/feature-flags.md)** · 繁體中文:**[docs/feature-flags.zh-TW.md](../docs/feature-flags.zh-TW.md)**

### 1. Initialize provider and propagator (application responsibility)

See **examples/main.go**. In short: create TracerProvider (e.g. OTLP), set `otel.SetTracerProvider(tp)` and `otel.SetTextMapPropagator(prop)`, defer shutdown.

### 2. Connect and use

**MongoDB driver v2** (recommended; import path aligns with Go convention):

```go
import (
    "github.com/akira-core/instrumentation-go/otel-mongo/v2"
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
    "github.com/akira-core/instrumentation-go/otel-mongo/otelmongo"
    "go.mongodb.org/mongo-driver/mongo/options"
)

client, err := otelmongo.Connect(ctx, options.Client().ApplyURI(uri))
if err != nil { log.Fatal(err) }
defer client.Disconnect(ctx)

db := client.Database("mydb")
coll := db.Collection("mycoll")
// Same CRUD and _oteltrace behaviour as v2 wrapper
```

Optional: **ConnectWithOptions(ctx, traceOpts, mongoOpts)** (v1) or **ConnectWithOptions(traceOpts, mongoOpts)** (v2) with **WithTracerProvider(tp)**, **WithPropagators(p)**, or **WithTracingEnabled(v bool)**.

### 3. Restore trace from document (e.g. change streams)

`ContextFromDocument` / `ContextFromRawDocument` carry **no feature-flag gate at all**. They start no span, write nothing, and perform no OpenFeature evaluation, so there is nothing for a switch to protect you from — and turning this module off does not stop them. That is deliberate: `Decode` + `ContextFromDocument` is the supported way to keep trace linking while the library is silenced. They return zero / `ok == false` only when the document has no `_oteltrace`, or its `traceparent` is absent or invalid.

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

---

## API summary

| Item | Description |
|------|-------------|
| **Connect / ConnectWithOptions** | Uses `otel.GetTracerProvider()` unless **WithTracerProvider(tp)** is passed. |
| **NewClient** | Same; accepts optional **WithTracerProvider**, **WithPropagators**, **WithTracingEnabled**. |
| **ContextFromDocument** | Restores trace context from document’s `_oteltrace` (e.g. for change streams). |
| **ScopeName / Version()** | Used when creating Tracer (OTel contrib guideline). |

---

## Span kinds

MongoDB is a database, not a messaging system: every operation span uses `CLIENT` (`Watch`'s change-stream read span included — it is a synchronous `getMore` call, not an async delivery). Local-only work (`Cursor.DecodeAndTrace` on a document with no round trip) uses `INTERNAL`.

```
InsertOne / FindOne / UpdateOne / ... (CLIENT)
Watch → change-stream read (CLIENT)
Cursor.DecodeAndTrace (INTERNAL, linked to the origin span when `_oteltrace` is present)
```

---

## v1 vs v2 API Differences

| Difference | v1 (`otelmongo`) | v2 (`.../v2`) |
|------------|------------------|---------------|
| `Connect` signature | `Connect(ctx, opts...)` | `Connect(opts...)` |
| `NewClient` signature | `NewClient(ctx, uri, traceOpts...)` | `NewClient(uri, traceOpts...)` |
| `Distinct` return | `([]interface{}, error)` | `*mongo.DistinctResult` |
| `StartSession` return | `mongo.Session, error` | `*mongo.Session, error` |
| `Cursor.DecodeAndTrace` | Identical behavior in both: always emits a `mongo.cursor.decode` INTERNAL span on a new (detached) trace, with a link to the origin span when the document's `_oteltrace` metadata is present and propagation is enabled. | (same) |

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

`NewCollection` accepts no options, so it resolves the switches from the environment alone. Whether the instrumented implementation is built at all depends on whether a relay could ever enable this module, or whether the environment already does; the effective per-operation answer is the master switch AND `OTEL_MONGO_TRACING_ENABLED`, each down the full ladder, with `OTEL_MONGO_PROPAGATION_ENABLED` a further switch below that for `_oteltrace`. There is no per-collection functional option for propagation; use **`ConnectWithOptions`** with **`WithTracePropagationEnabled`** to supply that rung for a client from code instead of the environment — note that it loses to `OTEL_MONGO_PROPAGATION_ENABLED` and to the relay, and cannot bypass a disabled tracing switch.

### DecodeAndTrace vs Decode on Cursor

`Cursor.DecodeAndTrace` extracts the producer's trace context from `_oteltrace` and returns an enriched context — use it when you need to link downstream work to the document's origin trace. Plain `Cursor.Decode` works exactly like the underlying driver's `Decode` and ignores `_oteltrace`.

### Span links on FindOne

`SingleResult.Decode` adds a **span link** (not a parent-child relationship) to the `_oteltrace` stored in the fetched document. The FindOne span ends when `Decode`, `Raw`, or `TraceContext` is first called. Call exactly one of these per `SingleResult`.

### `server.address` / `server.port` attribution

When tracing is enabled, Collection CRUD CLIENT spans (`InsertOne`, `Find`, `UpdateOne`, `Aggregate`, `Watch`, etc.) carry the `server.address`/`server.port` of the MongoDB connection that **actually served that specific command** — captured via an `event.CommandMonitor` registered on the underlying driver client, not just parsed once from the connection URI at `Connect` time. This makes the attribute accurate for multi-host replica-set URIs, `mongodb+srv://` connection strings, and after a primary failover, where the first host in the URI may not be the host that served a given command.

If no command event was observed for a call (e.g. a defensive/edge-case code path), the span falls back to the statically-parsed address from the connection URI — identical to pre-0.6.1 behavior.

**Caller-supplied `SetMonitor` is chained, not replaced.** If you pass your own `*options.ClientOptions` with `SetMonitor(...)` to `Connect`/`ConnectWithOptions`, otelmongo's address-capture callback runs first and then delegates to your `Started`/`Succeeded`/`Failed` callbacks unmodified — nothing is silently dropped.

This capture only runs on the tracing-enabled path; when tracing is disabled, no `CommandMonitor` is registered and any monitor you supply passes through completely untouched.

---

## Dependencies

- **v2** (`.../otel-mongo/v2`): `go.mongodb.org/mongo-driver/v2`, `go.opentelemetry.io/otel` and SDK. See `v2/go.mod`.
- **otelmongo** (v1, root): `go.mongodb.org/mongo-driver` v1, `go.opentelemetry.io/otel` and SDK. See root `go.mod`.
- Go 1.24+
