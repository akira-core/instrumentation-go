## ADDED Requirements

### Requirement: `otel-mongo` v1 and v2 use the shared `internal/flags/` helper

Both `otel-mongo/otelmongo/` (v1) and `otel-mongo/v2/` SHALL replace their local `env_flags.go` resolvers with calls to a per-module `internal/flags/` package. The three-tier gate (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED` AND `OTEL_MONGO_PROPAGATION_ENABLED`) SHALL remain unchanged in observable behaviour.

#### Scenario: v1 tracing gate

- **WHEN** code calls `mongoTracingEnabled()` (or equivalent) in v1
- **THEN** the function SHALL be implemented in terms of `flags.EnvEnabled` and composed by a `flags.Gate`
- **AND** the result SHALL equal `flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED") && flags.EnvEnabled("OTEL_MONGO_TRACING_ENABLED")`

#### Scenario: v2 tracing gate matches v1

- **WHEN** the same env-var combinations are tested against v1 and v2
- **THEN** both modules SHALL return the same boolean result for every combination
- **AND** the resolver code SHALL be byte-identical between v1's `env_flags.go` and v2's `env_flags.go`

#### Scenario: Propagation resolver respects functional-option override

- **WHEN** both tracing gates are on AND the caller supplies `WithTracePropagationEnabled(true)` while `OTEL_MONGO_PROPAGATION_ENABLED` is unset
- **THEN** `resolveDocumentPropagation` SHALL return `true`

#### Scenario: Propagation override cannot bypass disabled tracing

- **WHEN** the global or module tracing flag is off AND the caller supplies `WithTracePropagationEnabled(true)`
- **THEN** `resolveDocumentPropagation` SHALL return `false`
- **AND** the `_oteltrace` field SHALL NOT be injected on writes

### Requirement: Propagation gate depends on tracing gate

`OTEL_MONGO_PROPAGATION_ENABLED` SHALL only have effect when both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED` are truthy. When the tracing gate is off, the propagation gate SHALL be force-disabled — `_oteltrace` document field MUST NOT be injected on writes, MUST NOT be extracted on reads, and `ContextFromDocument` / `ContextFromRawDocument` MUST return the input context unchanged.

#### Scenario: Propagation env on, tracing env off → propagation off

- **WHEN** `OTEL_MONGO_PROPAGATION_ENABLED=true` but `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset
- **THEN** `cachedPropagationEnabled()` SHALL return `false`
- **AND** writes via the wrapper SHALL NOT include an `_oteltrace` field in the BSON document
- **AND** `ContextFromDocument(ctx, doc)` SHALL return `ctx` unchanged even if `doc` contains an `_oteltrace` field

#### Scenario: Propagation env on, module tracing off → propagation off

- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` and `OTEL_MONGO_PROPAGATION_ENABLED=true` but `OTEL_MONGO_TRACING_ENABLED` is unset
- **THEN** `cachedPropagationEnabled()` SHALL return `false`
- **AND** the wrapper SHALL behave identically to the unwrapped `*mongo.Collection` for write and read paths

#### Scenario: All three flags on → propagation on

- **WHEN** all of `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED` are set to a truthy value
- **THEN** writes SHALL inject `_oteltrace = { traceparent, tracestate }` into the document
- **AND** `ContextFromDocument` SHALL extract the trace context and return a context carrying the remote span

### Requirement: Wrapper spans use `noop.TracerProvider` when tracing disabled

When `mongoTracingEnabled()` returns `false`, `Connect` and `Wrap` SHALL substitute `noop.NewTracerProvider()` for any user-supplied or default tracer provider so any stray `tracer.Start` call in the disabled path is inert. The disabled `Client` / `Database` / `Collection` / `Cursor` / `SingleResult` / `ChangeStream` impls SHALL NOT call `tracer.Start` themselves — the strategy-split layout keeps span starts confined to `internal/traced/*`.

#### Scenario: Disabled mode produces zero spans

- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset and a caller invokes `InsertOne`, `Find`, `UpdateOne`, `DeleteOne`, `Aggregate`, `Watch`, `BulkWrite`, or any other wrapper method
- **THEN** no spans SHALL be emitted to the configured exporter
- **AND** the wrapper SHALL return the same result the upstream `*mongo.Collection` would have returned

#### Scenario: Disabled deliver-span goroutine never starts

- **WHEN** tracing is disabled
- **THEN** the deliver-tracer initialiser (`initMongoProvider`) SHALL NOT run
- **AND** no `BatchSpanProcessor` or `otlptracegrpc` / `otlptracehttp` exporter SHALL be created by the module

#### Scenario: Disabled `Client.Disconnect` performs no `TracerProvider.Shutdown`

- **WHEN** tracing is disabled and a caller invokes `Client.Disconnect(ctx)` on the strategy-split direct impl
- **THEN** the direct impl `Disconnect` SHALL NOT call `Shutdown` on any `TracerProvider`, `BatchSpanProcessor`, or `SimpleSpanProcessor`
- **AND** the direct impl `Client` struct SHALL NOT hold a `*sdktrace.TracerProvider` field at all (no field to shut down)
- **AND** the disabled `Disconnect` SHALL delegate straight to the upstream `*mongo.Client.Disconnect(ctx)` with no additional work
- **AND** any `Shutdown` of an SDK `TracerProvider` SHALL live exclusively in `internal/traced/client.go` (where the deliver-tracer `mongoTP` is held)

Rationale: the disabled-mode invariant is that no OTel SDK code path runs. Holding even an unused `*sdktrace.TracerProvider` field on the disabled impl would risk a future maintainer threading a `Shutdown` call into the disabled path. The strategy-split package boundary enforces this by keeping the field on the traced impl only.

### Requirement: Strategy-split layout already in place for Collection / Cursor / SingleResult / ChangeStream is preserved

The existing `internal/{direct,traced,shared}/` package layout in `otel-mongo/otelmongo/` and `otel-mongo/v2/` SHALL NOT be reorganised by this change. Only the env-flag plumbing inside `env_flags.go` and `tracing.go` is replaced.

#### Scenario: Internal packages preserved

- **WHEN** the change lands
- **THEN** `otel-mongo/otelmongo/internal/direct/`, `internal/traced/`, and `internal/shared/` directories SHALL exist with the same Go files as before the change
- **AND** the same compile-time assertions (`var _ shared.CursorImpl = (*traced.Cursor)(nil)` and equivalents) SHALL remain

### Requirement: Client and Database migrate to strategy-split

`Client` and `Database` in both `otel-mongo/otelmongo/` and `otel-mongo/v2/` SHALL migrate from the cached-gate `tracingEnabled bool` field to the strategy-split layout (`internal/{direct,traced}.Client` / `.Database`) so the disabled-mode invariant is compiler-enforced for these types in line with Collection/Cursor.

#### Scenario: `Client.Database` returns strategy-split `Database`

- **WHEN** a caller invokes `client.Database(name)`
- **THEN** the returned `*Database` SHALL hold a `databaseImpl` interface field pointing at either `internal/direct.Database` or `internal/traced.Database`
- **AND** the choice SHALL be inherited from the parent `Client`'s gate result (set at `Connect` time)

#### Scenario: `Database.Collection` returns strategy-split `Collection`

- **WHEN** a caller invokes `db.Collection(name)`
- **THEN** the returned `*Collection` SHALL hold a `collectionImpl` matching the parent `Database`'s impl flavour
- **AND** no runtime `if tracingEnabled` check SHALL appear in `Database.Collection`

### Requirement: `cachedPropagationEnabled` migrates to `flags.Gate`

The package-level `cachedPropagationEnabled` function (used by `ContextFromDocument` / `ContextFromRawDocument`) SHALL be rewritten in terms of `flags.NewGate` and `Gate.Enabled()`. The cached value SHALL still reflect the full three-tier gate.

#### Scenario: Cached value reflects all three flags

- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true`, `OTEL_MONGO_TRACING_ENABLED=true`, `OTEL_MONGO_PROPAGATION_ENABLED=false`
- **THEN** the cached gate SHALL return `false`

#### Scenario: Reset hook still works after migration

- **WHEN** a test calls `t.Setenv` for any of the three flags and then calls `resetPropEnabledCacheForTest`
- **THEN** the next `cachedPropagationEnabled()` call SHALL re-evaluate the gate and return the new combined result
