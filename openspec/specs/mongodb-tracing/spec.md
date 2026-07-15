# mongodb-tracing Specification

## Purpose
Defines the tracing behavior of `otel-mongo` (v1 `otelmongo/` and v2 `v2/`): provider/propagator fallback, feature-flag gating, `_oteltrace` document propagation, span kinds/attributes, server-address capture, and the disabled-mode invariant.

## Requirements
### Requirement: Provider and propagator fallback
The `otel-mongo` (v1) and `otel-mongo/v2` packages SHALL NOT construct or own a global `TracerProvider`. `Connect`, `NewClient`, and `ConnectWithOptions` SHALL use `otel.GetTracerProvider()` and `otel.GetTextMapPropagator()` unless the caller supplies `WithTracerProvider(tp)` and/or `WithPropagators(p)`.

#### Scenario: No options supplied
- **WHEN** an application calls `otelmongo.Connect(ctx, opts...)` (v1) or `otelmongo.Connect(opts...)` (v2) without `WithTracerProvider`/`WithPropagators`
- **THEN** the resulting client uses the process-global `otel.GetTracerProvider()` and `otel.GetTextMapPropagator()` at connect time

#### Scenario: Per-connection override
- **WHEN** an application calls `ConnectWithOptions` with `WithTracerProvider(tp)`
- **THEN** the client stores `tp` on the `Client` for its own spans and does not call `otel.SetTracerProvider`

### Requirement: Three-tier tracing feature-flag gating
The package SHALL gate all wrapper CLIENT spans and `_oteltrace` document propagation behind three environment variables: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global), `OTEL_MONGO_TRACING_ENABLED` (module tracing), and `OTEL_MONGO_PROPAGATION_ENABLED` (module propagation). An unset variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL be treated as disabled; any other set value SHALL be treated as enabled.

#### Scenario: Global flag disabled disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy
- **THEN** the wrapper uses a noop tracer for CLIENT spans and does not inject or extract `_oteltrace`, regardless of `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`, or `WithTracePropagationEnabled`

#### Scenario: Module tracing disabled forces propagation off
- **WHEN** the global flag is enabled but `OTEL_MONGO_TRACING_ENABLED` is unset or falsy
- **THEN** the wrapper uses a noop tracer for CLIENT spans and `_oteltrace` inject/extract is disabled, and `WithTracePropagationEnabled(true)` cannot override this

#### Scenario: Both tracing gates on, propagation flag decides the default
- **WHEN** the global flag and `OTEL_MONGO_TRACING_ENABLED` are both enabled
- **THEN** `OTEL_MONGO_PROPAGATION_ENABLED` sets the default for `_oteltrace` inject/extract, and `WithTracePropagationEnabled` passed to `ConnectWithOptions` can override that default

### Requirement: `_oteltrace` document propagation on write
When document propagation is enabled and an active span is present in the context, `InsertOne`, `InsertMany`, `ReplaceOne`, `UpdateOne`, `UpdateMany`, `UpdateByID`, and `BulkWrite` (for its `InsertOneModel`, `UpdateOneModel`, and `UpdateManyModel` write models) SHALL inject a reserved `_oteltrace` subdocument (`{ traceparent, tracestate }`) into the written document, or into `$set` for operator-style updates.

#### Scenario: Insert with active span
- **WHEN** `InsertOne` is called with a context carrying an active OTel span and propagation is enabled
- **THEN** the inserted document contains an `_oteltrace` field with the span's `traceparent` and `tracestate`

#### Scenario: No active span
- **WHEN** `InsertOne` is called with a context that has no active OTel span
- **THEN** no `_oteltrace` field is added to the document

### Requirement: Trace context restoration from documents
`ContextFromDocument(ctx, doc)` and `ContextFromRawDocument(ctx, raw)` SHALL restore a remote span context from a document's `_oteltrace` field, gated by a process-wide, `sync.Once`-cached resolution of the same three environment variables as writes (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`), and SHALL return `ok == false` (without modifying `ctx`) when that cached gate resolves to disabled. This cached gate reads environment variables only — it does **not** consult any per-connection `WithTracePropagationEnabled` override passed to `ConnectWithOptions`, so a `Client` whose `Collection` writes `_oteltrace` because of an override may still see `ContextFromDocument` return `ok == false` for that same document when the underlying env vars are off.

#### Scenario: All propagation gates enabled
- **WHEN** `ContextFromDocument` is called on a document containing a valid `_oteltrace` field and all three tracing/propagation env vars are enabled
- **THEN** it returns a valid remote `SpanContext` and `ok == true`

#### Scenario: Propagation gate disabled
- **WHEN** any of `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, `OTEL_MONGO_TRACING_ENABLED`, or `OTEL_MONGO_PROPAGATION_ENABLED` is disabled
- **THEN** `ContextFromDocument`/`ContextFromRawDocument` return the input context unchanged and `ok == false`

#### Scenario: Per-connection override does not extend to document restoration
- **WHEN** a `Client` is constructed via `ConnectWithOptions` with `WithTracePropagationEnabled(true)` while `OTEL_MONGO_PROPAGATION_ENABLED` is unset, causing that client's `Collection` to inject `_oteltrace` on write
- **THEN** `ContextFromDocument`/`ContextFromRawDocument` still return `ok == false` for a document written by that collection, because the cached gate ignores the override and reflects only the environment variables

### Requirement: Cursor decode with trace linking
`Cursor.DecodeWithContext(ctx, v)` SHALL emit a `mongo.cursor.decode` INTERNAL span on a new, detached trace, and SHALL add a span link to the origin span when the decoded document's `_oteltrace` metadata is present and propagation is enabled. Plain `Cursor.Decode` SHALL behave exactly like the underlying driver's `Decode` and SHALL ignore `_oteltrace`.

#### Scenario: Change-stream document with trace metadata
- **WHEN** `DecodeWithContext` decodes a document containing `_oteltrace` and propagation is enabled
- **THEN** a `mongo.cursor.decode` span is created with a link to the document's origin span

#### Scenario: Plain Decode ignores trace metadata
- **WHEN** `Cursor.Decode` is called on the same document
- **THEN** the call behaves identically to the underlying driver and does not create a span or read `_oteltrace`

### Requirement: SingleResult span lifecycle
`SingleResult.Decode` (and equivalently `Raw`/`TraceContext`) SHALL add a span link (not a parent-child relationship) to the `_oteltrace` recorded in the fetched document, and the originating FindOne-family span SHALL end when the first of `Decode`, `Raw`, or `TraceContext` is called.

#### Scenario: Decode called once
- **WHEN** a caller invokes `SingleResult.Decode` exactly once on a `FindOne` result
- **THEN** the FindOne span ends at that call and carries a link to the document's `_oteltrace` span, if present and propagation is enabled

### Requirement: No deliver spans or deliver TracerProvider
`otel-mongo` (v1 `otelmongo/` and `v2/`) SHALL NOT emit synthetic "deliver" spans and SHALL NOT construct an independent deliver `TracerProvider`. No exported or internal identifier (`StartDeliverSpan`, `DeliverTracer`, `DeliverAttributes`, `initMongoProvider`, and any `WithDeliver*` option) SHALL remain. The packages SHALL NOT read `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission.

#### Scenario: No deliver span on a write
- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT` is set and tracing is enabled and a caller invokes `InsertOne`, `InsertMany`, `UpdateOne`, `DeleteOne`, `Aggregate`, `BulkWrite`, or `Watch`
- **THEN** exactly one span (the operation span) SHALL be emitted for that call
- **AND** no span named `"* deliver"` SHALL be emitted
- **AND** no separate `BatchSpanProcessor` or OTLP exporter SHALL be created by the module

#### Scenario: Deliver identifiers removed from the API
- **WHEN** the module source is compiled
- **THEN** no reference to `StartDeliverSpan`, `DeliverTracer`, `DeliverAttributes`, or `initMongoProvider` SHALL exist in `otelmongo/` or `v2/`

### Requirement: Span kind per MongoDB operation
The wrapper SHALL set span kind per the OTel database semantic conventions (Mongo is a database, not a messaging system): synchronous DB calls use `CLIENT`, and local-only work uses `INTERNAL`.

#### Scenario: CRUD operations use CLIENT
- **WHEN** a caller invokes any CRUD/command wrapper (`InsertOne`, `Find`, `UpdateOne`, `DeleteOne`, `ReplaceOne`, `Aggregate`, `CountDocuments`, `Distinct`, `BulkWrite`, etc.)
- **THEN** the emitted span SHALL have `SpanKind == CLIENT`

#### Scenario: Change-stream read uses CLIENT
- **WHEN** a caller reads from a change stream (`Watch` and subsequent cursor advance/decode)
- **THEN** the emitted operation span SHALL have `SpanKind == CLIENT`
- **AND** it SHALL NOT use `CONSUMER` or `PRODUCER`

#### Scenario: Cursor decode uses INTERNAL
- **WHEN** the cursor performs a local decode with no round trip
- **THEN** the emitted span SHALL have `SpanKind == INTERNAL`

### Requirement: MongoDB span attribute set
Operation spans SHALL carry only OTel database-semconv attributes: `db.system.name = "mongodb"`, `db.collection.name`, `db.operation.name`, `db.namespace` (when known), `db.operation.batch.size` (only when batch ≥ 2), and `server.address` / `server.port` (port omitted when the default 27017). On error, `db.response.status_code` and `error.type` SHALL be set. No deliver-only attribute set SHALL be produced.

#### Scenario: Successful insert attributes
- **WHEN** `InsertMany` succeeds with 5 documents against `mydb.things` on `mongodb://host:27017`
- **THEN** the span SHALL carry `db.system.name=mongodb`, `db.collection.name=things`, `db.operation.name=insert`, `db.namespace=mydb`, `db.operation.batch.size=5`, `server.address=host`
- **AND** `server.port` SHALL be omitted (default 27017)

#### Scenario: Error attributes on write failure
- **WHEN** a write returns a `mongo.WriteException` with code 11000
- **THEN** the span status SHALL be `Error` and it SHALL carry `db.response.status_code=11000` and `error.type=11000`

### Requirement: Per-command server address capture
When tracing is enabled, `otel-mongo` (v1 `otelmongo/` and v2 `v2/`) SHALL derive the `server.address`/`server.port` attributes on a Collection CRUD CLIENT span from the actual MongoDB connection that carried that specific command, not from a value parsed once at `Connect` time.

#### Scenario: Command served by a non-first replica-set member
- **WHEN** a `Client` is connected with a multi-host replica-set URI and a Collection operation (e.g. `FindOne`) is served by a host other than the first host listed in the connection string
- **THEN** the operation's CLIENT span's `server.address` (and `server.port`, when non-default) reflect the host that actually served the command, not the first host in the URI

#### Scenario: Failover changes the serving host between two operations
- **WHEN** two sequential Collection operations on the same `Client` are served by different hosts (e.g. after a primary failover)
- **THEN** each operation's span independently reflects the host that served it — the second span's `server.address` differs from the first's if the serving host changed

#### Scenario: mongodb+srv:// connection string
- **WHEN** a `Client` is connected via a `mongodb+srv://` URI
- **THEN** Collection operation spans carry the resolved connection's actual host, not the unresolved SRV record name

#### Scenario: Retried operation
- **WHEN** a retryable Collection operation is retried once by the driver before succeeding
- **THEN** the operation's span's `server.address`/`server.port` reflect the connection used by the attempt that produced the returned result

### Requirement: Fallback to static URI-derived address
When no per-command server address was captured for an operation, `otel-mongo` SHALL fall back to the existing statically-parsed `Client.serverAddr`/`serverPort` (derived from the connection URI at `Connect`/`ConnectWithOptions` time) so the span still carries a best-effort `server.address` rather than omitting it.

#### Scenario: No command event captured
- **WHEN** a Collection operation completes without a corresponding `CommandStartedEvent` having been observed for its context (e.g. defensive/edge-case path)
- **THEN** the operation's span uses the statically-parsed `Client.serverAddr`/`serverPort` as `server.address`/`server.port`, identical to pre-change behavior

### Requirement: Caller-supplied CommandMonitor is chained, not replaced
When a caller passes their own `*options.ClientOptions` with `SetMonitor(...)` already set to `Connect`/`ConnectWithOptions`, `otel-mongo` SHALL preserve the caller's monitor callbacks by chaining: the package's own address-capture logic runs first, then the caller's original `Started`/`Succeeded`/`Failed` callbacks (for whichever of those the caller set) run unmodified with the same event.

#### Scenario: Caller has their own command monitor for APM
- **WHEN** a caller constructs `*options.ClientOptions` with `SetMonitor(&event.CommandMonitor{Started: myStartedFn, Succeeded: mySucceededFn})` and passes it to `otelmongo.ConnectWithOptions`
- **THEN** `myStartedFn` and `mySucceededFn` are still invoked for every command, receiving the same events they would have received without `otel-mongo`'s instrumentation

#### Scenario: Caller sets only a subset of monitor callbacks
- **WHEN** a caller's `event.CommandMonitor` only sets `Succeeded` (leaving `Started`/`Failed` nil)
- **THEN** `otel-mongo`'s own `Started` callback still runs (to capture the address) and the caller's `Succeeded` callback still runs unmodified; no nil-function-call panic occurs

### Requirement: No new tracing behavior when tracing is disabled
When `OTEL_MONGO_TRACING_ENABLED` (combined with the global gate) is off, `otel-mongo` SHALL NOT register a `CommandMonitor` for address capture, consistent with the existing disabled-mode invariant that the disabled path performs no additional instrumentation-related work.

#### Scenario: Disabled tracing registers no command monitor
- **WHEN** `Connect`/`ConnectWithOptions` is called with tracing disabled (module or global gate off)
- **THEN** no address-capture `CommandMonitor` is attached to the resulting `*mongo.Client`'s options, and any caller-supplied `SetMonitor` passes through completely untouched

### Requirement: Disabled tracing emits no spans or SDK objects
When the tracing gate is off (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_MONGO_TRACING_ENABLED` are not both truthy), the wrapper SHALL delegate to the native driver and run no OTel SDK code path — no real-tracer `Start`, no `TracerProvider`, no exporter, no `_oteltrace` inject/extract — consistent with the module-wide disabled-mode invariant. This applies to both the strategy-split path (`internal/direct/*`, which imports no `otel/sdk` or `otel/exporters`) and the cached-gate Client/Database. Removing the deliver `TracerProvider` shrinks this disabled surface (its init is gone, not merely gated off).

#### Scenario: Tracing disabled delegates to native driver
- **WHEN** the tracing gate is off and a caller invokes `InsertOne`, `Find`, `Aggregate`, or `Watch` (v1 or v2)
- **THEN** the wrapper SHALL delegate to the native `*mongo.Collection` via the `internal/direct` impl
- **AND** no span SHALL be emitted
- **AND** no `TracerProvider`, `BatchSpanProcessor`, or OTLP exporter SHALL be constructed
- **AND** no `_oteltrace` field SHALL be injected or stripped

### Requirement: Disabled-mode invariant via strategy split
`Collection`, `Cursor`, `SingleResult`, and `ChangeStream` SHALL be constructed with one of two implementations chosen once at construction time — `internal/direct` (passthrough) when tracing is disabled, `internal/traced` (instrumented) when enabled — such that `internal/direct` imports no `go.opentelemetry.io/otel` package of any kind (API, SDK, or exporters).

#### Scenario: Tracing disabled at construction
- **WHEN** the resolved tracing gate is disabled at the time a `Collection` is constructed
- **THEN** the facade's `impl` field is set to an `internal/direct` implementation and no OTel SDK code path can execute for that collection's lifetime

#### Scenario: CI enforcement of the direct package boundary
- **WHEN** any file under `otel-mongo/otelmongo/internal/direct/` or `otel-mongo/v2/internal/direct/` imports any `go.opentelemetry.io/otel` package (the CI grep pattern matches the bare `go.opentelemetry.io/otel` prefix, not just `sdk`/`exporters` subpaths)
- **THEN** the CI "Verify direct/ has no OTel SDK imports" step SHALL fail the build

### Requirement: v1/v2 API parity
`otel-mongo` (v1, package `otelmongo`) and `otel-mongo/v2` SHALL expose the same wrapper API surface (`Client`, `Database`, `Collection`, `Cursor`, `SingleResult`, `ChangeStream`, `ContextFromDocument`) and identical `_oteltrace` behavior, differing only in driver-imposed signatures (`Connect(ctx, opts...)` vs `Connect(opts...)`; `Distinct` returning `([]interface{}, error)` vs `*mongo.DistinctResult`; `StartSession` returning `mongo.Session, error` vs `*mongo.Session, error`).

#### Scenario: Equivalent CRUD behavior
- **WHEN** the same CRUD operation is invoked through the v1 wrapper and the v2 wrapper with equivalent inputs
- **THEN** both inject/extract `_oteltrace` identically and both honor the same three-tier feature-flag gate
