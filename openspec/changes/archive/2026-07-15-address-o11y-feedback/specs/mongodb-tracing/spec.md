# mongodb-tracing Delta Specification

## MODIFIED Requirements

### Requirement: Three-tier tracing feature-flag gating
The package SHALL gate all wrapper CLIENT spans and `_oteltrace` document propagation behind three environment variables: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global), `OTEL_MONGO_TRACING_ENABLED` (module tracing), and `OTEL_MONGO_PROPAGATION_ENABLED` (module propagation). An unset variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL be treated as disabled; any other set value SHALL be treated as enabled. The env-derived tracing result SHALL serve as the **default**: when the caller passes the `WithTracingEnabled(v bool)` `ClientOption` to `ConnectWithOptions`, that value SHALL be authoritative for the resulting `Client` — and everything constructed from it (Databases, Collections including their strategy-split direct/traced impl selection, Cursors, ChangeStreams, and deliver-span initialization) — overriding the global and module tracing gates in either direction per the shared `WithTracingEnabled` decision table in `shared-feature-flags`. `WithTracePropagationEnabled` continues to govern only the propagation default, and propagation SHALL still require the client's effective tracing state to be enabled: `WithTracePropagationEnabled(true)` cannot enable propagation on a client whose effective tracing is off, whether that state came from the env gates or from `WithTracingEnabled(false)`. When effective tracing is on: absent prop option → `OTEL_MONGO_PROPAGATION_ENABLED`; prop option present → that value. Clients constructed without `WithTracingEnabled` SHALL behave exactly as before. This applies identically to v1 and v2 (parity rule). The package-level `ContextFromDocument`/`ContextFromRawDocument` gate remains env-only and is unaffected by per-client options.

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
- **THEN** that client uses the noop tracer, its Collections select the direct (passthrough) impl, no deliver provider is initialized, and `_oteltrace` propagation is disabled for that client regardless of `WithTracePropagationEnabled`

#### Scenario: Package-level document extraction ignores per-client options
- **WHEN** a client writes `_oteltrace` because of `WithTracingEnabled(true)` + `WithTracePropagationEnabled(true)` while the underlying env vars are off, and `ContextFromDocument` is later called on such a document
- **THEN** `ContextFromDocument` still resolves its own env-only cached gate and returns `ok == false` — per-client options do not affect the package-level functions
