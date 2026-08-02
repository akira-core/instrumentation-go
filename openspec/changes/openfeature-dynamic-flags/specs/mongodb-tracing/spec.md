## MODIFIED Requirements

### Requirement: Three-tier tracing feature-flag gating
The package SHALL gate all wrapper CLIENT spans and `_oteltrace` document propagation behind three tiers: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global, environment-only), the dynamic flag `otel-mongo-tracing` (module tracing), and the dynamic flag `otel-mongo-propagation` (module propagation). The two dynamic flags SHALL resolve through the module's `flags.Resolver` with `OTEL_MONGO_TRACING_ENABLED` and `OTEL_MONGO_PROPAGATION_ENABLED` respectively as their OpenFeature default values, so an application with no provider installed observes exactly the previous environment-driven behavior. An unset environment variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL be treated as disabled; any other set value SHALL be treated as enabled.

The global switch SHALL be a hard kill switch: when it is disabled and no `WithTracingEnabled` option is present, no OpenFeature evaluation SHALL occur and no relay value SHALL enable tracing or propagation.

When the caller passes the `WithTracingEnabled(v bool)` `ClientOption` to `ConnectWithOptions`, that value SHALL be authoritative for the resulting `Client` — and everything constructed from it (Databases, Collections including their strategy-split direct/traced impl selection, Cursors, ChangeStreams) — overriding the global switch and the dynamic flag in either direction per the shared `WithTracingEnabled` decision table in `shared-feature-flags`, and SHALL suppress OpenFeature evaluation for that client entirely. `WithTracePropagationEnabled` continues to govern only the propagation default, and propagation SHALL still require the client's effective tracing state to be enabled: `WithTracePropagationEnabled(true)` cannot enable propagation on a client whose effective tracing is off, whether that state came from the global switch, from the dynamic flag, or from `WithTracingEnabled(false)`. When effective tracing is on: absent prop option → the dynamic `otel-mongo-propagation` value; prop option present → that value. For a client constructed with `WithTracingEnabled`, no OpenFeature evaluation SHALL occur even for the propagation default — `OTEL_MONGO_PROPAGATION_ENABLED` alone SHALL serve as that client's default. This is required, not merely consistent: when the override is what carried tracing past a disabled global kill switch, a resolver read here would be the one path by which a relay value reaches a kill-switched process. This applies identically to v1 and v2 (parity rule), which share both flag keys.

#### Scenario: Global flag disabled disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the wrapper uses a noop tracer for CLIENT spans and does not inject or extract `_oteltrace`, regardless of `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`, `WithTracePropagationEnabled`, or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Module tracing disabled forces propagation off
- **WHEN** the global flag is enabled but the dynamic `otel-mongo-tracing` value resolves to disabled, and no `WithTracingEnabled` option is passed
- **THEN** the wrapper emits no CLIENT spans and `_oteltrace` inject/extract is disabled, and `WithTracePropagationEnabled(true)` cannot override this

#### Scenario: Both tracing gates on, propagation flag decides the default
- **WHEN** the global flag is enabled and the dynamic `otel-mongo-tracing` value resolves to enabled
- **THEN** the dynamic `otel-mongo-propagation` value sets the default for `_oteltrace` inject/extract, and `WithTracePropagationEnabled` passed to `ConnectWithOptions` can override that default

#### Scenario: No provider installed reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed and the tracing env vars are set to any combination
- **THEN** the resolved behavior is identical to the release preceding this change

#### Scenario: Relay enables tracing on a running client
- **WHEN** a `Client` was constructed without `WithTracingEnabled` while `OTEL_MONGO_TRACING_ENABLED` was unset, the global switch is enabled, and the relay subsequently resolves `otel-mongo-tracing` to `true`
- **THEN** operations issued after the resolver's TTL expires create CLIENT spans without the client being reconstructed

#### Scenario: Relay disables tracing on a running client
- **WHEN** a `Client` is tracing under a truthy `OTEL_MONGO_TRACING_ENABLED` and the relay subsequently resolves `otel-mongo-tracing` to `false`
- **THEN** operations issued after the resolver's TTL expires emit no spans and inject no `_oteltrace`

#### Scenario: Option enables tracing with env off (unset or falsy)
- **WHEN** `ConnectWithOptions(ctx, []ClientOption{WithTracingEnabled(true)}, mongoOpts)` is called with all tracing env vars unset or explicitly falsy
- **THEN** the client creates real CLIENT spans, its Collections are constructed on the traced path, `WithTracePropagationEnabled(true)` may enable `_oteltrace` propagation for that client, and no OpenFeature evaluation is performed for that client

#### Scenario: Static client's propagation default ignores the relay
- **WHEN** a client is constructed with `WithTracingEnabled(true)` and no `WithTracePropagationEnabled` while `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is falsy and the relay resolves `otel-mongo-propagation` to `true`
- **THEN** that client's `_oteltrace` propagation follows `OTEL_MONGO_PROPAGATION_ENABLED` alone and no OpenFeature evaluation is performed

#### Scenario: Option disables tracing despite truthy env vars and a truthy relay flag
- **WHEN** all env gates are truthy, the relay resolves `otel-mongo-tracing` to `true`, and the caller passes `WithTracingEnabled(false)`
- **THEN** that client uses the noop tracer, its Collections use the direct (passthrough) implementation, and `_oteltrace` propagation is disabled for that client regardless of `WithTracePropagationEnabled` or any subsequent relay change

### Requirement: Trace context restoration from documents
`ContextFromDocument(ctx, doc)` and `ContextFromRawDocument(ctx, raw)` SHALL restore a remote span context from a document's `_oteltrace` field, gated by the same resolution the `Collection` path uses: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` ANDed with the dynamic `otel-mongo-tracing` and `otel-mongo-propagation` values read from the module's `flags.Resolver` snapshot. They SHALL return `ok == false` (without modifying `ctx`) when that resolution is disabled. Because these are package-level functions with no connection to consult, they SHALL NOT observe any per-connection option — neither `WithTracingEnabled` nor `WithTracePropagationEnabled` — so a `Client` whose `Collection` writes `_oteltrace` because of an override may still see `ContextFromDocument` return `ok == false` for that same document. They SHALL observe relay changes within the resolver's TTL, unlike the permanently cached gate they replace.

#### Scenario: All propagation gates enabled
- **WHEN** `ContextFromDocument` is called on a document containing a valid `_oteltrace` field, the global switch is enabled, and both dynamic Mongo flags resolve to enabled
- **THEN** it returns a valid remote `SpanContext` and `ok == true`

#### Scenario: Propagation gate disabled
- **WHEN** the global switch is disabled, or either dynamic Mongo flag resolves to disabled
- **THEN** `ContextFromDocument`/`ContextFromRawDocument` return the input context unchanged and `ok == false`

#### Scenario: Relay disables propagation for a running change-stream reader
- **WHEN** a change-stream loop is calling `ContextFromDocument` successfully and the relay subsequently resolves `otel-mongo-propagation` to `false`
- **THEN** calls made after the resolver's TTL expires return `ok == false`, matching the `Collection` path in the same loop

#### Scenario: Per-connection override does not extend to document restoration
- **WHEN** a `Client` is constructed via `ConnectWithOptions` with `WithTracePropagationEnabled(true)` while the dynamic `otel-mongo-propagation` value resolves to disabled, causing that client's `Collection` to inject `_oteltrace` on write
- **THEN** `ContextFromDocument`/`ContextFromRawDocument` still return `ok == false` for a document written by that collection, because the package-level functions ignore per-connection options

### Requirement: Disabled-mode invariant via strategy split
`Collection`, `Cursor`, and `ChangeStream` SHALL hold both an `internal/direct` (passthrough) and an `internal/traced` (instrumented) implementation, and SHALL select between them per operation according to the wrapper's effective tracing state, such that `internal/direct` imports no `go.opentelemetry.io/otel` package of any kind (API, SDK, or exporters) and `internal/traced` contains no feature-flag gating of its own.

For a single public operation, the tracing boolean used to select the implementation SHALL also be the tracing input to document-propagation resolution for that operation — the propagation path SHALL NOT independently re-resolve module tracing via a second `Resolver.Enabled` call that could cross a TTL boundary mid-operation. `ContextFromDocument` and `ContextFromRawDocument` SHALL likewise resolve tracing once before consulting propagation. Fail-safe composition remains: when that tracing value is false, propagation SHALL be false without reading a separate "propagation on while tracing off" combination into effect.

The unexported `collectionImpl` methods `Find`, `Aggregate`, and `Watch` SHALL return only the raw driver cursor/change-stream plus error; the facade SHALL construct dual direct/traced wrappers. Those methods SHALL NOT return a throwaway `shared.CursorImpl` / `shared.ChangeStreamImpl` that the facade discards. `FindOne` continues to return `shared.SingleResultImpl` for the live-span exception above.

`SingleResult` is the documented exception: its implementation SHALL be fixed by whichever path executed the originating `FindOne`. `internal/traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`, so no instrumented implementation can be constructed for a `FindOne` that ran through the passthrough path — there is no span to hold. Selecting per call would also be incoherent: a flag flip between `FindOne` and `Decode` would leave an already-started span that the passthrough path would never end. `SingleResult` represents one already-executed operation, so the flag value at `FindOne` time is the only meaningful answer.

When a `WithTracingEnabled` option is present on the originating `Client`, the selection SHALL be fixed at construction to the option's value and no OpenFeature evaluation SHALL occur. When the option is absent and `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is disabled, only the `internal/direct` implementation SHALL be constructed and no OTel SDK code path SHALL be reachable. When the option is absent and the global switch is enabled, both implementations SHALL be constructed and the dynamic `otel-mongo-tracing` value SHALL select between them on each operation.

#### Scenario: Global switch off constructs only the passthrough implementation
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is present at the time a `Collection` is constructed
- **THEN** only an `internal/direct` implementation is constructed and no OTel SDK code path can execute for that collection's lifetime

#### Scenario: Dynamic value selects the implementation per operation
- **WHEN** the global switch is enabled, a `Collection` was constructed without `WithTracingEnabled`, and the dynamic `otel-mongo-tracing` value changes between two operations
- **THEN** the first operation runs through the implementation selected by the old value and the second through the implementation selected by the new value, without the `Collection` being reconstructed

#### Scenario: Long-lived change stream follows the flag
- **WHEN** a `ChangeStream` opened while tracing was enabled outlives a relay change that disables `otel-mongo-tracing`
- **THEN** iterations after the resolver's TTL expires run through the `internal/direct` implementation and emit no spans

#### Scenario: SingleResult keeps the implementation its FindOne ran through
- **WHEN** `FindOne` executes through the instrumented path and the relay disables `otel-mongo-tracing` before the caller calls `Decode`
- **THEN** `Decode` still runs through `internal/traced.SingleResult` and ends the span that `FindOne` started, because leaving it unended would leak an open span

#### Scenario: SingleResult from a passthrough FindOne never becomes instrumented
- **WHEN** `FindOne` executes through the passthrough path and the relay enables `otel-mongo-tracing` before the caller calls `Decode`
- **THEN** `Decode` runs through `internal/direct.SingleResult` and emits no span, because no `FindOne` span exists to end

#### Scenario: Option pins the implementation for the wrapper's lifetime
- **WHEN** a `Client` is constructed with `WithTracingEnabled(true)` and the relay later resolves `otel-mongo-tracing` to `false`
- **THEN** its Collections continue to run through `internal/traced` and continue to emit spans

#### Scenario: CI enforcement of the direct package boundary
- **WHEN** any file under `otel-mongo/otelmongo/internal/direct/` or `otel-mongo/v2/internal/direct/` imports any `go.opentelemetry.io/otel` package (the CI grep pattern matches the bare `go.opentelemetry.io/otel` prefix, not just `sdk`/`exporters` subpaths)
- **THEN** the CI "Verify direct/ has no OTel SDK imports" step SHALL fail the build
