## ADDED Requirements

### Requirement: Shared internal `flags` package

Each of the four modules (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) SHALL ship an `internal/flags/` package containing a generic `Gate` type and the `EnvEnabled` helper. The file contents of `internal/flags/flags.go` MUST be byte-identical across all four modules (excluding the package declaration line and any module-specific constants kept in separate files).

#### Scenario: Helper used in place of local `envEnabledByDefault`

- **WHEN** any module needs to read a tracing or propagation env var
- **THEN** the module SHALL call `flags.EnvEnabled(name)` instead of declaring its own `envEnabledByDefault` function
- **AND** the four legacy `env_flags.go` files SHALL no longer contain a private `envEnabledByDefault` helper

#### Scenario: Drift across modules is rejected by CI

- **WHEN** a developer edits `internal/flags/flags.go` in one module but forgets the other three
- **THEN** the CI drift-check step SHALL fail the build with a diff showing which file differs
- **AND** the build SHALL NOT be marked green until all four copies are identical

### Requirement: All feature flags default to disabled

Every feature flag in every module SHALL default to **disabled** when its env var is unset. A consumer SHALL receive instrumentation behaviour ONLY by explicitly setting the relevant env var to a truthy value. Importing the module SHALL NOT switch on any tracing or propagation behaviour by itself.

#### Scenario: Fresh process with no env vars

- **WHEN** a binary linking any of the four instrumentation modules starts with none of the `OTEL_INSTRUMENTATION_GO_*` / `OTEL_MONGO_*` / `OTEL_NATS_*` / `OTEL_GORILLA_WS_*` variables set
- **THEN** every gate SHALL resolve to `false`
- **AND** every wrapper SHALL be constructed with its `internal/direct.X` impl

#### Scenario: Global master switch off short-circuits module flags

- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset OR set to a falsy value
- **THEN** every module's tracing gate SHALL resolve to `false` regardless of the module-specific env var value
- **AND** every module's propagation gate (if any) SHALL also resolve to `false`

### Requirement: Default-off env semantics

`flags.EnvEnabled(name)` SHALL return `true` if and only if the environment variable is set AND its value is not one of `0`, `false`, `no`, `off` (case-insensitive, whitespace-trimmed).

#### Scenario: Variable absent

- **WHEN** `os.LookupEnv(name)` returns `ok=false`
- **THEN** `flags.EnvEnabled(name)` SHALL return `false`

#### Scenario: Variable set to falsy value

- **WHEN** the variable equals one of `"0"`, `"false"`, `"FALSE"`, `"no"`, `"NO"`, `"off"`, `"OFF"`, or `" 0 "` (with surrounding whitespace)
- **THEN** `flags.EnvEnabled(name)` SHALL return `false`

#### Scenario: Variable set to any other value

- **WHEN** the variable is set to `"1"`, `"true"`, `"yes"`, `"on"`, `"enabled"`, or any non-falsy string
- **THEN** `flags.EnvEnabled(name)` SHALL return `true`

### Requirement: `Gate` type with cached resolution

`internal/flags/` SHALL expose a `Gate` type that caches the result of a resolver function across the process lifetime using `sync.Once` and `atomic.Bool`. The `Gate.Enabled()` method SHALL call the resolver at most once per process.

#### Scenario: First call evaluates resolver

- **WHEN** `gate.Enabled()` is called for the first time after `NewGate(fn)`
- **THEN** the gate SHALL invoke `fn` exactly once
- **AND** SHALL store the result in `atomic.Bool`
- **AND** SHALL return the result

#### Scenario: Subsequent calls return cached value

- **WHEN** `gate.Enabled()` is called any number of additional times
- **THEN** the resolver `fn` SHALL NOT be invoked again
- **AND** all calls SHALL return the value stored on the first call

#### Scenario: Env change after first call is ignored

- **WHEN** a process sets `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true`, calls `gate.Enabled()` (returns `true`), then sets the variable to `false`
- **THEN** subsequent `gate.Enabled()` calls SHALL still return `true`

### Requirement: Test-reset hook

`Gate` SHALL expose a `ResetForTest()` method that clears the cached value and the `sync.Once`, allowing tests using `t.Setenv` to re-resolve the gate. Production code paths SHALL NOT call `ResetForTest()`.

#### Scenario: Test resets after `t.Setenv`

- **WHEN** a test sets `t.Setenv("OTEL_MONGO_TRACING_ENABLED", "true")` and calls `gate.ResetForTest()`
- **THEN** the next call to `gate.Enabled()` SHALL re-invoke the resolver and observe the new env value

#### Scenario: Parallel test isolation

- **WHEN** a test using `gate.ResetForTest()` is marked `t.Parallel()`
- **THEN** the test framework SHALL be allowed to fail or produce a data-race report — `ResetForTest` is documented as not parallel-safe
- **AND** the package documentation SHALL warn against `t.Parallel()` with env-toggle tests

### Requirement: Disabled-mode invariant — zero OTel SDK span produced

When any module's required gates resolve to disabled, **zero** OTLP / OTel SDK spans SHALL be produced by the module. No code path in the disabled impl SHALL import, instantiate, or call any symbol from `go.opentelemetry.io/otel/sdk/*`, `go.opentelemetry.io/otel/exporters/*`, or any `[]attribute.KeyValue` builder. Trace context inject/extract via `propagation.TextMapPropagator` SHALL NOT execute. No `BatchSpanProcessor`, `SimpleSpanProcessor`, or exporter goroutine SHALL be started by the disabled module.

#### Scenario: No spans reach the exporter

- **WHEN** any module's gates are off and the application exercises any public wrapper method N times
- **THEN** the configured OTLP exporter SHALL receive zero spans attributable to the disabled module
- **AND** a Tempo / collector query for the module's service name SHALL return no traces produced by the disabled module

#### Scenario: Disabled mode runs unwrapped behaviour

- **WHEN** all of `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, `OTEL_<MODULE>_TRACING_ENABLED` (and `OTEL_MONGO_PROPAGATION_ENABLED` for `otel-mongo`) are unset or falsy
- **THEN** every public method of every wrapper in the affected module SHALL produce the same observable side effects as calling the upstream client directly
- **AND** no `internal/traced/*` package symbol SHALL be referenced by the disabled impl

#### Scenario: Compiler-enforced isolation

- **WHEN** a developer accidentally adds `import "go.opentelemetry.io/otel/sdk/trace"` to a file under `internal/direct/`
- **THEN** the package boundary review SHALL flag the violation
- **AND** a `go vet -vettool` or `golangci-lint` custom check SHALL be encouraged in follow-up; for this change, the absence of SDK imports is enforced by code review of the `internal/direct/` tree

### Requirement: Tracer is noop when disabled and `tracer.Start` is avoided

When gates are disabled, any tracer reference held by the disabled impl SHALL be a `noop.Tracer` returned by `noop.NewTracerProvider().Tracer(...)`. Additionally, the disabled impl SHALL avoid calling `tracer.Start` at all where the strategy-split layout permits — span starts SHALL live exclusively in `internal/traced/*` code paths.

#### Scenario: Disabled impl holds a noop tracer

- **WHEN** any wrapper is constructed with gates off
- **THEN** any internal `tracer trace.Tracer` field on the impl (if present at all) SHALL be the noop tracer from `go.opentelemetry.io/otel/trace/noop`
- **AND** any `TracerProvider` substituted by the wrapper for the user-supplied one SHALL be `noop.NewTracerProvider()`

#### Scenario: `tracer.Start` never appears in `internal/direct/`

- **WHEN** a maintainer greps `internal/direct/` for `tracer.Start` or `Tracer().Start(`
- **THEN** zero matches SHALL be returned
- **AND** the corresponding code path in the traced impl SHALL be the only place span starts live

### Requirement: Strategy-split pattern adopted by all three packages

Every wrapper that today branches on a runtime `tracingEnabled bool` inside public methods (`otelnats.Conn`, `oteljetstream.Consumer`, `oteljetstream.MessageBatch`, `otelgorillaws.Conn`, plus `otel-mongo` Client/Database/Collection/Cursor/SingleResult/ChangeStream) SHALL hold an `impl <X>Impl` interface field. The constructor SHALL pick `internal/direct.X` or `internal/traced.X` exactly once based on `gate.Enabled()` (plus any runtime negotiation, e.g. WebSocket subprotocol). All public methods SHALL dispatch through `c.impl.<Method>(args...)` with no further `if tracingEnabled` branches.

#### Scenario: Public method has no `tracingEnabled` branch

- **WHEN** a maintainer reads any public method body of `otelnats.Conn`, `oteljetstream.Consumer`, `oteljetstream.MessageBatch`, or `otelgorillaws.Conn` after this change lands
- **THEN** the body SHALL NOT contain `if c.tracingEnabled` or any equivalent runtime gate
- **AND** the body SHALL contain exactly one statement of the form `return c.impl.<Method>(args...)` (modulo argument adaptation)

#### Scenario: Compile-time interface assertion

- **WHEN** the facade package builds
- **THEN** assertions of the form `var _ <X>Impl = (*direct.X)(nil)` and `var _ <X>Impl = (*traced.X)(nil)` SHALL be present in the facade
- **AND** adding a method to the interface without implementing it in both impls SHALL fail compilation

### Requirement: No new public functional options on `otel-nats` / `otel-gorilla-ws`

This change SHALL NOT add new exported `With*` options to `otel-nats` or `otel-gorilla-ws`. Existing options (`WithTracerProvider`, `WithPropagators`, etc.) keep current semantics.

#### Scenario: Public API surface unchanged

- **WHEN** `go doc` is run against `otel-nats/otelnats`, `otel-nats/oteljetstream`, or `otel-gorilla-ws` before and after this change
- **THEN** the set of exported identifiers SHALL be identical (modulo doc-comment edits)
