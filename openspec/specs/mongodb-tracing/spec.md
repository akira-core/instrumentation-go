# mongodb-tracing Specification

## Purpose
TBD - created by archiving change document-otel-instrumentation. Update Purpose after archive.
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
The package SHALL gate all wrapper CLIENT spans and `_oteltrace` document propagation behind three environment variables: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global), `OTEL_MONGO_TRACING_ENABLED` (module tracing), and `OTEL_MONGO_PROPAGATION_ENABLED` (module propagation). An unset variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL be treated as disabled; any other set value SHALL be treated as enabled. The env-derived tracing result SHALL serve as the **default**: when the caller passes the `WithTracingEnabled(v bool)` `ClientOption` to `ConnectWithOptions`, that value SHALL be authoritative for the resulting `Client` — and everything constructed from it (Databases, Collections including their strategy-split direct/traced impl selection, Cursors, ChangeStreams) — overriding the global and module tracing gates in either direction per the shared `WithTracingEnabled` decision table in `shared-feature-flags`. `WithTracePropagationEnabled` continues to govern only the propagation default, and propagation SHALL still require the client's effective tracing state to be enabled: `WithTracePropagationEnabled(true)` cannot enable propagation on a client whose effective tracing is off, whether that state came from the env gates or from `WithTracingEnabled(false)`. When effective tracing is on: absent prop option → `OTEL_MONGO_PROPAGATION_ENABLED`; prop option present → that value. Clients constructed without `WithTracingEnabled` SHALL behave exactly as before. This applies identically to v1 and v2 (parity rule). The package-level `ContextFromDocument`/`ContextFromRawDocument` gate remains env-only and is unaffected by per-client options.

#### Scenario: Global flag disabled disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the wrapper uses a noop tracer for CLIENT spans and does not inject or extract `_oteltrace`, regardless of `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`, or `WithTracePropagationEnabled`

#### Scenario: Module tracing disabled forces propagation off
- **WHEN** the global flag is enabled but `OTEL_MONGO_TRACING_ENABLED` is unset or falsy, and no `WithTracingEnabled` option is passed
- **THEN** the wrapper uses a noop tracer for CLIENT spans and `_oteltrace` inject/extract is disabled, and `WithTracePropagationEnabled(true)` cannot override this

#### Scenario: Both tracing gates on, propagation flag decides the default
- **WHEN** the global flag and `OTEL_MONGO_TRACING_ENABLED` are both enabled
- **THEN** `OTEL_MONGO_PROPAGATION_ENABLED` sets the default for `_oteltrace` inject/extract, and `WithTracePropagationEnabled` passed to `ConnectWithOptions` can override that default

#### Scenario: Option enables tracing with env off (unset or falsy)
- **WHEN** `ConnectWithOptions(ctx, []ClientOption{WithTracingEnabled(true)}, mongoOpts)` is called with all tracing env vars unset or explicitly falsy
- **THEN** the client creates real CLIENT spans, its Collections select the traced impl, and `WithTracePropagationEnabled(true)` may enable `_oteltrace` propagation for that client

#### Scenario: Option disables tracing despite truthy env vars
- **WHEN** all env gates are truthy and the caller passes `WithTracingEnabled(false)`
- **THEN** that client uses the noop tracer, its Collections select the direct (passthrough) impl, and `_oteltrace` propagation is disabled for that client regardless of `WithTracePropagationEnabled`

#### Scenario: Package-level document extraction ignores per-client options
- **WHEN** a client writes `_oteltrace` because of `WithTracingEnabled(true)` + `WithTracePropagationEnabled(true)` while the underlying env vars are off, and `ContextFromDocument` is later called on such a document
- **THEN** `ContextFromDocument` still resolves its own env-only cached gate and returns `ok == false` — per-client options do not affect the package-level functions

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
`Cursor.DecodeAndTrace(ctx, v)` SHALL emit a `mongo.cursor.decode` INTERNAL span on a new, detached trace, and SHALL add a span link to the origin span when the decoded document's `_oteltrace` metadata is present and propagation is enabled. Plain `Cursor.Decode` SHALL behave exactly like the underlying driver's `Decode` and SHALL ignore `_oteltrace`.

#### Scenario: Change-stream document with trace metadata
- **WHEN** `DecodeAndTrace` decodes a document containing `_oteltrace` and propagation is enabled
- **THEN** a `mongo.cursor.decode` span is created with a link to the document's origin span

#### Scenario: Plain Decode ignores trace metadata
- **WHEN** `Cursor.Decode` is called on the same document
- **THEN** the call behaves identically to the underlying driver and does not create a span or read `_oteltrace`

### Requirement: SingleResult span lifecycle
`SingleResult.Decode` (and equivalently `Raw`/`TraceContext`) SHALL add a span link (not a parent-child relationship) to the `_oteltrace` recorded in the fetched document, and the originating FindOne-family span SHALL end when the first of `Decode`, `Raw`, or `TraceContext` is called.

#### Scenario: Decode called once
- **WHEN** a caller invokes `SingleResult.Decode` exactly once on a `FindOne` result
- **THEN** the FindOne span ends at that call and carries a link to the document's `_oteltrace` span, if present and propagation is enabled

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

