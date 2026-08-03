## MODIFIED Requirements

### Requirement: Three-tier tracing feature-flag gating
The package SHALL gate all wrapper CLIENT spans and `_oteltrace` document propagation behind a conjunction of tiers, evaluated with short-circuit semantics:

```
tracing     := gate1 && EnvEnabled(OTEL_MONGO_TRACING_ENABLED)     && resolver.Allowed(idxTracing)
propagation := tracing && gateProp && resolver.Allowed(idxPropagation)
```

`gate1` SHALL be `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` or the `WithTracingEnabled(v bool)` `ClientOption`, which are mutually exclusive per the shared `shared-feature-flags` rule; supplying both SHALL make `ConnectWithOptions` return an error. `gateProp` SHALL likewise be `OTEL_MONGO_PROPAGATION_ENABLED` or the `WithTracePropagationEnabled(v bool)` `ClientOption`, mutually exclusive on the same terms and reported through a distinct sentinel error.

The relay flags `otel-mongo-tracing` and `otel-mongo-propagation` SHALL be resolved with an evaluation default of `true` and SHALL only ever subtract: a `false` on the relay disables a tier the deployment enabled; no relay value SHALL enable a tier the deployment left off. When an environment tier resolves to disabled the module SHALL NOT consult the resolver for that flag.

Environment truthiness SHALL follow the allow-list in `shared-feature-flags`: only `1`, `true`, `yes`, `on` (trimmed, case-insensitive) enable; every other value, including the empty string, disables.

`WithTracingEnabled` SHALL NOT make a client static. A client constructed with it SHALL still read both environment tiers and both relay verdicts per operation, and SHALL stop tracing when the relay revokes. This applies identically to v1 and v2 (parity rule), which share both flag keys.

#### Scenario: First tier disabled disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the wrapper uses a noop tracer for CLIENT spans and does not inject or extract `_oteltrace`, regardless of `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`, `WithTracePropagationEnabled`, or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Module tracing disabled forces propagation off
- **WHEN** `gate1` is enabled but `OTEL_MONGO_TRACING_ENABLED` is unset or falsy
- **THEN** the wrapper emits no CLIENT spans, `_oteltrace` inject/extract is disabled, `WithTracePropagationEnabled(true)` cannot override this, and no relay evaluation is performed

#### Scenario: Relay revokes tracing on a running client
- **WHEN** a `Client` is tracing under a truthy `OTEL_MONGO_TRACING_ENABLED` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** operations issued after the resolver's TTL expires emit no spans and inject no `_oteltrace`

#### Scenario: Relay cannot enable tracing the deployment left off
- **WHEN** `gate1` is enabled, `OTEL_MONGO_TRACING_ENABLED` is unset, and the relay resolves `otel-mongo-tracing` to `true`
- **THEN** no CLIENT spans are created and no evaluation is performed

#### Scenario: Relay revokes only propagation
- **WHEN** both environment tiers are truthy and the relay resolves `otel-mongo-tracing` to `true` and `otel-mongo-propagation` to `false`
- **THEN** CLIENT spans continue to be created and `_oteltrace` inject/extract stops

#### Scenario: Relay cannot enable propagation the deployment left off
- **WHEN** tracing is enabled, `OTEL_MONGO_PROPAGATION_ENABLED` is unset, no `WithTracePropagationEnabled` is passed, and the relay resolves `otel-mongo-propagation` to `true`
- **THEN** no `_oteltrace` field is written to any document

#### Scenario: No provider installed reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed and the tracing environment variables are set to any combination of allow-list values
- **THEN** the resolved behavior is identical to the release preceding this change

#### Scenario: Option and environment variable together are rejected
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set to any value and `ConnectWithOptions` is passed `WithTracingEnabled(v)` for either value of `v`
- **THEN** `ConnectWithOptions` returns an error matching the module's tracing-conflict sentinel and no `Client` is created

#### Scenario: Propagation option and environment variable together are rejected
- **WHEN** `OTEL_MONGO_PROPAGATION_ENABLED` is set to any value and `ConnectWithOptions` is passed `WithTracePropagationEnabled(v)`
- **THEN** `ConnectWithOptions` returns an error matching the module's propagation-conflict sentinel and no `Client` is created

#### Scenario: Both conflicts are reported in one error
- **WHEN** both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_MONGO_PROPAGATION_ENABLED` are set and `ConnectWithOptions` is passed both `WithTracingEnabled(v)` and `WithTracePropagationEnabled(w)`
- **THEN** one joined error is returned that satisfies `errors.Is` for both sentinels, with the tracing conflict first, and no `Client` is created

#### Scenario: Option supplies the first tier when the environment is silent
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset, `OTEL_MONGO_TRACING_ENABLED` is truthy, and `ConnectWithOptions` is passed `WithTracingEnabled(true)`
- **THEN** the client creates real CLIENT spans, its Collections are constructed on the traced path, and a subsequent relay revocation still stops them

### Requirement: `_oteltrace` document propagation on write
When document propagation is enabled and an active span is present in the context, `InsertOne`, `InsertMany`, `ReplaceOne`, `UpdateOne`, `UpdateMany`, `UpdateByID`, and `BulkWrite` (for its `InsertOneModel`, `UpdateOneModel`, and `UpdateManyModel` write models) SHALL inject a reserved `_oteltrace` subdocument (`{ traceparent, tracestate }`) into the written document, or into `$set` for operator-style updates.

The field SHALL NOT be removed by any read path: the module reads `_oteltrace` to restore trace context but never strips it from a decoded document, so once written it is visible to the application on every subsequent read. Disabling propagation SHALL stop further writes but SHALL NOT remove fields already written; removing them is an application-side `$unset` migration.

Because enabling this behavior changes what is persisted — approximately 90 bytes of BSON per document, more when a `tracestate` is present — it SHALL be enablable only by the deployment (`OTEL_MONGO_PROPAGATION_ENABLED` or `WithTracePropagationEnabled`), never by a relay value.

#### Scenario: Insert with active span
- **WHEN** `InsertOne` is called with a context carrying an active OTel span and propagation is enabled
- **THEN** the inserted document contains an `_oteltrace` field with the span's `traceparent` and `tracestate`

#### Scenario: No active span
- **WHEN** `InsertOne` is called with a context that has no active OTel span
- **THEN** no `_oteltrace` field is added to the document

#### Scenario: Field survives a revocation
- **WHEN** documents were written with `_oteltrace` and the relay subsequently resolves `otel-mongo-propagation` to `false`
- **THEN** new writes carry no `_oteltrace`, and documents written earlier still contain the field and still expose it to the application on read

#### Scenario: Read never strips the field
- **WHEN** a document containing `_oteltrace` is decoded into a `bson.M`
- **THEN** the `_oteltrace` key is present in the decoded map

### Requirement: Trace context restoration from documents
`ContextFromDocument(ctx, doc)` and `ContextFromRawDocument(ctx, raw)` SHALL restore a remote span context from a document's `_oteltrace` field and SHALL NOT be gated by any feature flag — not `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, not the module environment variables, and not the relay verdicts.

The justification is that they emit nothing: they start no span, build no attributes, initialise no part of the OTel SDK, and write to no document. They read a field out of a value the caller already holds and return what it encodes. The feature flags govern work the library performs on the caller's behalf as a side effect of a business operation; these functions do only the thing the caller invoked them for.

`Cursor.DecodeAndTrace` and `ChangeStream.DecodeAndTrace` SHALL remain gated, because they start and end a real `mongo.cursor.decode` span on every call. The two surfaces are not equivalent and SHALL NOT be given the same rule on the grounds that both read `_oteltrace`.

Because they observe no configuration at all, these functions SHALL continue to ignore per-connection options, and SHALL behave identically however `gate1` was supplied.

#### Scenario: Extraction works with every switch off
- **WHEN** `ContextFromDocument` is called on a document containing a valid `_oteltrace` field while `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and both module environment variables are unset
- **THEN** it returns the document's span context and `ok == true`, and no span is created

#### Scenario: Extraction is unaffected by a relay revocation
- **WHEN** a change-stream loop is calling `ContextFromDocument` and the relay revokes `otel-mongo-tracing` and `otel-mongo-propagation`
- **THEN** the calls keep returning the document's span context, while the `Collection` path in the same loop stops emitting spans and stops injecting `_oteltrace`

#### Scenario: Missing or malformed metadata still reports failure
- **WHEN** the document has no `_oteltrace` field, or its `traceparent` is absent or invalid
- **THEN** `ContextFromDocument` returns a zero `SpanContext` and `ok == false`, and `ContextFromRawDocument` returns the input context unchanged

#### Scenario: The gated sibling keeps its gate
- **WHEN** `Cursor.DecodeAndTrace` is called on the same document while the relay has revoked `otel-mongo-tracing`
- **THEN** it decodes through the passthrough implementation, returns `ctx` unchanged, and emits no `mongo.cursor.decode` span

#### Scenario: Configuration spelling does not matter
- **WHEN** a deployment supplies `gate1` through `WithTracingEnabled` and leaves `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` unset
- **THEN** both functions behave exactly as they would under the environment-variable spelling, because neither reads it

### Requirement: Disabled-mode invariant via strategy split
`Collection`, `Cursor`, and `ChangeStream` SHALL hold both an `internal/direct` (passthrough) and an `internal/traced` (instrumented) implementation, and SHALL select between them per operation according to the wrapper's effective tracing state, such that `internal/direct` imports no `go.opentelemetry.io/otel` package of any kind (API, SDK, or exporters) and `internal/traced` contains no feature-flag gating of its own.

Implementation selection at construction SHALL key on the whole static part of the decision, `gate1 && EnvEnabled(OTEL_MONGO_TRACING_ENABLED)` — the same expression `otel-gorilla-ws` uses for its negotiation capability. When it is false, only the `internal/direct` implementation SHALL be constructed and no OTel SDK code path SHALL be reachable, because the relay can only revoke and therefore can never make the instrumented path reachable. When it is true, both implementations SHALL be constructed and the per-operation relay verdict SHALL select between them. No wrapper SHALL be pinned to one implementation because a `WithTracingEnabled` option was supplied.

For a single public operation, the tracing boolean used to select the implementation SHALL also be the tracing input to document-propagation resolution for that operation — the propagation path SHALL NOT independently re-resolve module tracing via a second resolver read that could cross a TTL boundary mid-operation. Fail-safe composition remains: when that tracing value is false, propagation SHALL be false.

The unexported `collectionImpl` methods `Find`, `Aggregate`, and `Watch` SHALL return only the raw driver cursor/change-stream plus error; the facade SHALL construct dual direct/traced wrappers. Those methods SHALL NOT return a throwaway `shared.CursorImpl` / `shared.ChangeStreamImpl` that the facade discards. `FindOne` continues to return `shared.SingleResultImpl` for the live-span exception below.

`SingleResult` is the documented exception: its implementation SHALL be fixed by whichever path executed the originating `FindOne`. `internal/traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`, so no instrumented implementation can be constructed for a `FindOne` that ran through the passthrough path — there is no span to hold. Selecting per call would also be incoherent: a revocation between `FindOne` and `Decode` would leave an already-started span that the passthrough path would never end.

#### Scenario: First tier off constructs only the passthrough implementation
- **WHEN** `gate1` is disabled at the time a `Collection` is constructed
- **THEN** only an `internal/direct` implementation is constructed and no OTel SDK code path can execute for that collection's lifetime

#### Scenario: Module switch off also constructs only the passthrough implementation
- **WHEN** `gate1` is enabled and `OTEL_MONGO_TRACING_ENABLED` is unset or falsy at the time a `Collection` is constructed
- **THEN** only an `internal/direct` implementation is constructed, no OTel SDK code path can execute, and no relay evaluation is ever performed for that collection — because no relay value could reach the instrumented path

#### Scenario: Revocation selects the implementation per operation
- **WHEN** `gate1` and `OTEL_MONGO_TRACING_ENABLED` are enabled, a `Collection` has been constructed, and the relay revokes `otel-mongo-tracing` between two operations
- **THEN** the first operation runs through the instrumented implementation and the second through the passthrough implementation, without the `Collection` being reconstructed

#### Scenario: Long-lived change stream follows the revocation
- **WHEN** a `ChangeStream` opened while tracing was enabled outlives a relay revocation of `otel-mongo-tracing`
- **THEN** iterations after the resolver's TTL expires run through the `internal/direct` implementation and emit no spans

#### Scenario: Option does not pin the implementation
- **WHEN** a `Client` is constructed with `WithTracingEnabled(true)` while `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset, and the relay later revokes `otel-mongo-tracing`
- **THEN** its Collections switch to `internal/direct` and stop emitting spans

#### Scenario: SingleResult keeps the implementation its FindOne ran through
- **WHEN** `FindOne` executes through the instrumented path and the relay revokes `otel-mongo-tracing` before the caller calls `Decode`
- **THEN** `Decode` still runs through `internal/traced.SingleResult` and ends the span that `FindOne` started, because leaving it unended would leak an open span

#### Scenario: SingleResult from a passthrough FindOne never becomes instrumented
- **WHEN** `FindOne` executes through the passthrough path and the relay verdict changes before the caller calls `Decode`
- **THEN** `Decode` runs through `internal/direct.SingleResult` and emits no span, because no `FindOne` span exists to end

#### Scenario: CI enforcement of the direct package boundary
- **WHEN** any file under `otel-mongo/otelmongo/internal/direct/` or `otel-mongo/v2/internal/direct/` imports any `go.opentelemetry.io/otel` package (the CI grep pattern matches the bare `go.opentelemetry.io/otel` prefix, not just `sdk`/`exporters` subpaths)
- **THEN** the CI "Verify direct/ has no OTel SDK imports" step SHALL fail the build
