## ADDED Requirements

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

### Requirement: Deliver spans for the MongoDB service graph
When document/tracing flags are enabled and `OTEL_EXPORTER_OTLP_ENDPOINT` is set to a valid full URL (HTTP) or `host:port` (gRPC), `Connect` and `NewClient` SHALL initialize a synthetic deliver-span `TracerProvider` with `service.name` derived from the connection's server address, and `Collection` CRUD operations plus `ChangeStream` decode SHALL emit CONSUMER/PRODUCER deliver spans representing MongoDB as a broker node. `Cursor.DecodeWithContext` is **not** part of this deliver-span path — it only creates its own `mongo.cursor.decode` INTERNAL span with an optional link (see *Cursor decode with trace linking*) and never touches the deliver tracer.

#### Scenario: Endpoint configured and tracing enabled
- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` and the global + module tracing flags are enabled
- **THEN** `Connect` initializes the deliver tracer and subsequent `Collection` CRUD operations emit a `db.coll deliver` CONSUMER span in addition to the CLIENT span

#### Scenario: Tracing disabled
- **WHEN** the global or module tracing flag is disabled, regardless of `OTEL_EXPORTER_OTLP_ENDPOINT`
- **THEN** no deliver-span `TracerProvider` is initialized and no exporter is constructed

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

### Requirement: Diagnostic logging via slog
The package SHALL use `log/slog` for diagnostics with no custom handler installed, logging deliver-tracer initialization success at `DEBUG` and OTLP exporter/resource creation failures at `WARN`, using an `otelmongo:` prefix and structured `error`/`reason`/`service`/`endpoint` fields. Because Go's default `slog` handler filters at `LevelInfo`, `DEBUG`-level success logs are silent by default, but `WARN`-level failure logs print to stderr by default — the package does not suppress them.

#### Scenario: Default handler silences DEBUG but not WARN
- **WHEN** no custom `slog` handler is configured and deliver-tracer initialization succeeds
- **THEN** no `DEBUG` log line is visible (below the default `LevelInfo` threshold)

#### Scenario: OTLP exporter creation fails
- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT` is set to a malformed value and deliver-tracer initialization fails
- **THEN** a `WARN`-level log entry with the `otelmongo:` prefix and an `error` field is emitted
