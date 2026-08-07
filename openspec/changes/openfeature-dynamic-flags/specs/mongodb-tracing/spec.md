## MODIFIED Requirements

### Requirement: Three-tier tracing feature-flag gating
The package SHALL gate all wrapper CLIENT spans and `_oteltrace` document propagation behind three switches, composed by conjunction with short-circuit semantics:

```
tracing     := master && mongoTracing
propagation := tracing && mongoPropagation
```

Each switch SHALL be resolved down the precedence ladder defined in `shared-feature-flags` — `relay > env > option > default` — implemented as a single `Boolean` call whose evaluation default is the env-or-option-or-default value fixed at construction:

| Switch | Relay key | Option | Environment variable | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| tracing | `otel-mongo-tracing` | `WithTracingEnabled` | `OTEL_MONGO_TRACING_ENABLED` | `false` |
| propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

A relay value SHALL override the local value in **either** direction. Supplying an option and the paired environment variable together SHALL NOT be an error; the **environment variable** wins, and the option decides only when the variable is unset. `WithTracingEnabled` and `WithTracePropagationEnabled` SHALL supply their own module tiers only, never the master.

An environment value outside the recognised truthy and falsy lists, including the empty string, SHALL make `ConnectWithOptions` return an error wrapping `otelflags.ErrInvalidFlagValue`, per `shared-feature-flags`.

`WithTracingEnabled` SHALL NOT make a client static. A client constructed with it SHALL still resolve the master switch and both relay keys per operation, and SHALL change behaviour when either changes. This applies identically to v1 and v2 (parity rule), which share both flag keys.

#### Scenario: Master off disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is falsy, or the relay resolves `otel-instrumentation-go-tracing` to `false`
- **THEN** the wrapper uses a noop tracer for CLIENT spans and does not inject or extract `_oteltrace`, regardless of `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`, either option, or either module relay key

#### Scenario: Nothing configured traces nothing
- **WHEN** no environment variable is set, no option is passed, and no relay flag exists
- **THEN** the master resolves to `true`, tracing and propagation resolve to their defaults of `false`, and no CLIENT spans or `_oteltrace` fields are produced

#### Scenario: Module tracing disabled forces propagation off
- **WHEN** the master is on but the tracing switch resolves to `false`
- **THEN** the wrapper emits no CLIENT spans, `_oteltrace` inject/extract is disabled, and `WithTracePropagationEnabled(true)` cannot override this

#### Scenario: Relay disables tracing on a running client
- **WHEN** a `Client` is tracing and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** the next operation emits no span and injects no `_oteltrace`

#### Scenario: Relay enables tracing the deployment left off
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is unset, `relayPossible` is true, and the relay resolves `otel-mongo-tracing` to `true`
- **THEN** the next operation emits a CLIENT span

#### Scenario: Relay disables only propagation
- **WHEN** tracing is on and the relay resolves `otel-mongo-propagation` to `false`
- **THEN** CLIENT spans continue to be created and `_oteltrace` inject/extract stops

#### Scenario: Relay enables propagation the deployment left off
- **WHEN** tracing is on, `OTEL_MONGO_PROPAGATION_ENABLED` is unset, no `WithTracePropagationEnabled` is passed, and the relay resolves `otel-mongo-propagation` to `true`
- **THEN** subsequent writes carry an `_oteltrace` field, which is why the relay's ability to enable this tier is called out as a risk in the change's design

#### Scenario: Environment variable beats the option
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is truthy and `ConnectWithOptions` is passed `WithTracingEnabled(false)`, with no relay opinion
- **THEN** the client emits CLIENT spans, because the variable outranks the option

#### Scenario: Option decides when the environment is silent
- **WHEN** no Mongo environment variable is set and `ConnectWithOptions` is passed `WithTracingEnabled(true)`, with no relay opinion
- **THEN** the client emits CLIENT spans, and a second client built with `WithTracingEnabled(false)` in the same process does not

#### Scenario: An operator stops document writes the code asked for
- **WHEN** an application passes `WithTracePropagationEnabled(true)` and the deployment sets `OTEL_MONGO_PROPAGATION_ENABLED=false`, with no relay opinion
- **THEN** no `_oteltrace` field is written, because the operator's variable outranks the application's option — the reason the option sits below the environment

#### Scenario: Option and environment variable together are legal
- **WHEN** `OTEL_MONGO_PROPAGATION_ENABLED` is set to any recognised value and `ConnectWithOptions` is passed `WithTracePropagationEnabled(v)`
- **THEN** construction succeeds and the propagation tier takes the variable's value, whatever the option said

#### Scenario: Invalid environment value fails construction
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is set to `enabled`, `2` or the empty string
- **THEN** `ConnectWithOptions` returns an error wrapping `otelflags.ErrInvalidFlagValue` naming the variable and the value, and no `Client` is created

#### Scenario: No relay configured reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed, no endpoint variable is set, and the switches are configured through environment variables and options only
- **THEN** the resolved behaviour is the conjunction of those sources with the hardcoded defaults, and no OpenFeature evaluation is performed

### Requirement: `_oteltrace` document propagation on write
When document propagation is enabled and an active span is present in the context, `InsertOne`, `InsertMany`, `ReplaceOne`, `UpdateOne`, `UpdateMany`, `UpdateByID`, and `BulkWrite` (for its `InsertOneModel`, `UpdateOneModel`, and `UpdateManyModel` write models) SHALL inject a reserved `_oteltrace` subdocument (`{ traceparent, tracestate }`) into the written document, or into `$set` for operator-style updates.

The field SHALL NOT be removed by any read path: the module reads `_oteltrace` to restore trace context but never strips it from a decoded document, so once written it is visible to the application on every subsequent read. Disabling propagation SHALL stop further writes but SHALL NOT remove fields already written; removing them is an application-side `$unset` migration.

Because enabling this behaviour changes what is persisted — approximately 90 bytes of BSON per document, more when a `tracestate` is present, with no undo and a hard write failure against a collection using `$jsonSchema` with `additionalProperties: false` — the propagation switch's hardcoded default SHALL be `false`. Something SHALL have to say `true` explicitly: `OTEL_MONGO_PROPAGATION_ENABLED`, `WithTracePropagationEnabled`, or a relay flag the site deliberately created. Absence in every source SHALL never enable it.

The relay SHALL be able to enable this tier, unlike in the superseded revoke-only model. The documentation SHALL state that consequence plainly next to the field's size, its permanence and the `$unset` migration, so a site that cannot accept it knows to withhold the relay key or set the master switch falsy.

**Injection SHALL produce exactly one `_oteltrace` field.** `InjectTraceIntoDocument` currently appends unconditionally, so a document read, modified and written back — the ordinary read-modify-write cycle, since the field is never stripped on read — carries the field twice in the resulting `bson.D`. It SHALL remove any existing `_oteltrace` key before appending.

The read side makes this a correctness defect independent of how the server treats a duplicate key: `ExtractMetadataFromRaw` resolves the field with `bson.Raw.LookupErr`, which returns the **first** match, so a duplicated field yields the trace context from the original write rather than the current one, and a read-modify-write loop pins the linkage there permanently. Both modules SHALL apply the same fix.

#### Scenario: Re-injection replaces rather than duplicates
- **WHEN** a document already containing `_oteltrace` is written back through a propagating write method with an active span
- **THEN** the resulting document contains exactly one `_oteltrace` field, carrying the current span's context

#### Scenario: Extraction returns the current context after a read-modify-write
- **WHEN** a document is read, modified, written back, and then read again
- **THEN** `ContextFromDocument` returns the span context written by the most recent write

#### Scenario: Insert with active span
- **WHEN** `InsertOne` is called with a context carrying an active OTel span and propagation is enabled
- **THEN** the inserted document contains an `_oteltrace` field with the span's `traceparent` and `tracestate`

#### Scenario: No active span
- **WHEN** `InsertOne` is called with a context that has no active OTel span
- **THEN** no `_oteltrace` field is added to the document

#### Scenario: Absence never enables the field
- **WHEN** `OTEL_MONGO_PROPAGATION_ENABLED` is unset, no option is passed, and the relay defines no `otel-mongo-propagation` flag
- **THEN** no write carries an `_oteltrace` field, whatever the tracing switch resolves to

#### Scenario: Field survives being disabled
- **WHEN** documents were written with `_oteltrace` and the propagation switch subsequently resolves to `false`
- **THEN** new writes carry no `_oteltrace`, and documents written earlier still contain the field and still expose it to the application on read

#### Scenario: Read never strips the field
- **WHEN** a document containing `_oteltrace` is decoded into a `bson.M`
- **THEN** the `_oteltrace` key is present in the decoded map

### Requirement: Trace context restoration from documents
`ContextFromDocument(ctx, doc)` and `ContextFromRawDocument(ctx, raw)` SHALL restore a remote span context from a document's `_oteltrace` field and SHALL NOT be gated by any feature flag — not the master switch, not the module environment variables, not the options, and not the relay values.

The justification is that they emit nothing: they start no span, build no attributes, initialise no part of the OTel SDK, and write to no document. They read a field out of a value the caller already holds and return what it encodes. The feature flags govern work the library performs on the caller's behalf as a side effect of a business operation; these functions do only the thing the caller invoked them for.

`Cursor.DecodeAndTrace` and `ChangeStream.DecodeAndTrace` SHALL remain gated, because they start and end a real `mongo.cursor.decode` span on every call. The two surfaces are not equivalent and SHALL NOT be given the same rule on the grounds that both read `_oteltrace`.

Because they observe no configuration at all, these functions SHALL continue to ignore per-connection options, SHALL perform no OpenFeature evaluation, and SHALL behave identically however the switches were supplied.

**Disabling a module therefore does not stop trace-context extraction, and the documentation SHALL say so in those words.** The gate on `DecodeAndTrace` governs the span it emits, not the linking, and is bypassable by design through `Decode` followed by `ContextFromDocument` — the documented alternative for a caller who wants linking to survive the library being silenced.

#### Scenario: Extraction works with every switch off
- **WHEN** `ContextFromDocument` is called on a document containing a valid `_oteltrace` field while every switch resolves to disabled
- **THEN** it returns the document's span context and `ok == true`, no span is created, and no `Client.Boolean` call is made

#### Scenario: Extraction is unaffected by a relay change
- **WHEN** a change-stream loop is calling `ContextFromDocument` and the relay disables `otel-mongo-tracing` and `otel-mongo-propagation`
- **THEN** the calls keep returning the document's span context, while the `Collection` path in the same loop stops emitting spans and stops injecting `_oteltrace`

#### Scenario: Missing or malformed metadata still reports failure
- **WHEN** the document has no `_oteltrace` field, or its `traceparent` is absent or invalid
- **THEN** `ContextFromDocument` returns a zero `SpanContext` and `ok == false`, and `ContextFromRawDocument` returns the input context unchanged

#### Scenario: The gated sibling keeps its gate
- **WHEN** `Cursor.DecodeAndTrace` is called on the same document while the tracing switch resolves to `false`
- **THEN** it decodes through the passthrough implementation, returns `ctx` unchanged, and emits no `mongo.cursor.decode` span

#### Scenario: Configuration spelling does not matter
- **WHEN** a deployment supplies the tracing switch through `WithTracingEnabled` and leaves the environment variables unset
- **THEN** both functions behave exactly as they would under the environment-variable spelling, because neither reads it

### Requirement: Disabled-mode invariant via strategy split
`Collection`, `Cursor`, and `ChangeStream` SHALL hold both an `internal/direct` (passthrough) and an `internal/traced` (instrumented) implementation, and SHALL select between them per operation according to the wrapper's effective tracing state, such that `internal/direct` imports no `go.opentelemetry.io/otel` package of any kind (API, SDK, or exporters) and `internal/traced` contains no feature-flag gating of its own.

Implementation selection at construction SHALL key on whether a relay can exist at all:

```
useTracedImpl = relayPossible || (masterLocal && tracingLocal)
```

When it is false the relay is structurally incapable of returning anything but the value passed to it, so only the `internal/direct` implementation SHALL be constructed, no OTel SDK code path SHALL be reachable, no OpenFeature client SHALL be created, and `shared.NewCommandMonitor` SHALL NOT be registered. When it is true both implementations SHALL be constructed and the per-operation resolution SHALL select between them, because the relay may enable a tier the environment left off. No wrapper SHALL be pinned to one implementation because a `WithTracingEnabled` option was supplied.

For a single public operation, the tracing boolean used to select the implementation SHALL also be the tracing input to document-propagation resolution for that operation — the propagation path SHALL NOT independently re-resolve the master or the tracing switch via additional resolver reads. Fail-safe composition remains: when that tracing value is false, propagation SHALL be false.

The unexported `collectionImpl` methods `Find`, `Aggregate`, and `Watch` SHALL return only the raw driver cursor/change-stream plus error; the facade SHALL construct dual direct/traced wrappers. Those methods SHALL NOT return a throwaway `shared.CursorImpl` / `shared.ChangeStreamImpl` that the facade discards. `FindOne` continues to return `shared.SingleResultImpl` for the live-span exception below.

`SingleResult` is the documented exception: its implementation SHALL be fixed by whichever path executed the originating `FindOne`. `internal/traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`, so no instrumented implementation can be constructed for a `FindOne` that ran through the passthrough path — there is no span to hold. Selecting per call would also be incoherent: a change between `FindOne` and `Decode` would leave an already-started span that the passthrough path would never end.

#### Scenario: No relay possible and switches off constructs only the passthrough implementation
- **WHEN** no endpoint variable is set, no provider is installed, and the master and tracing switches resolve locally to disabled at the time a `Collection` is constructed
- **THEN** only an `internal/direct` implementation is constructed, no OTel SDK code path can execute for that collection's lifetime, no evaluation is ever performed, and no command monitor is registered

#### Scenario: Relay possible constructs both implementations regardless of the environment
- **WHEN** the endpoint variable is set and `OTEL_MONGO_TRACING_ENABLED` is unset at the time a `Collection` is constructed
- **THEN** both implementations are constructed, so a later relay `true` takes effect without reconstruction

#### Scenario: A flag change selects the implementation per operation
- **WHEN** a `Collection` was constructed with `relayPossible` true and the relay changes `otel-mongo-tracing` between two operations
- **THEN** the two operations run through different implementations, without the `Collection` being reconstructed

#### Scenario: Long-lived change stream follows the change
- **WHEN** a `ChangeStream` opened while tracing was enabled outlives a relay change of `otel-mongo-tracing` to `false`
- **THEN** subsequent iterations run through the `internal/direct` implementation and emit no spans

#### Scenario: Option does not pin the implementation
- **WHEN** a `Client` is constructed with `WithTracingEnabled(true)` and the relay later resolves `otel-mongo-tracing` to `false`
- **THEN** its Collections switch to `internal/direct` and stop emitting spans

#### Scenario: SingleResult keeps the implementation its FindOne ran through
- **WHEN** `FindOne` executes through the instrumented path and the relay disables `otel-mongo-tracing` before the caller calls `Decode`
- **THEN** `Decode` still runs through `internal/traced.SingleResult` and ends the span that `FindOne` started, because leaving it unended would leak an open span

#### Scenario: SingleResult from a passthrough FindOne never becomes instrumented
- **WHEN** `FindOne` executes through the passthrough path and the relay value changes before the caller calls `Decode`
- **THEN** `Decode` runs through `internal/direct.SingleResult` and emits no span, because no `FindOne` span exists to end

#### Scenario: CI enforcement of the direct package boundary
- **WHEN** any file under `otel-mongo/otelmongo/internal/direct/` or `otel-mongo/v2/internal/direct/` imports any `go.opentelemetry.io/otel` package (the CI grep pattern matches the bare `go.opentelemetry.io/otel` prefix, not just `sdk`/`exporters` subpaths)
- **THEN** the CI "Verify direct/ has no OTel SDK imports" step SHALL fail the build
