## ADDED Requirements

### Requirement: Application owns the OpenFeature provider
No package in this repository SHALL call `openfeature.SetProvider`, `openfeature.SetNamedProvider`, `openfeature.SetEvaluationContext`, `openfeature.AddHooks`, or `openfeature.Shutdown`. Each module SHALL obtain a client via `openfeature.NewClient(domain)` and read from it only, mirroring the existing rule that packages never initialize a `TracerProvider` and instead fall back to `otel.GetTracerProvider()`. The domain SHALL be the module name (`otel-mongo` for both v1 and v2, `otel-nats`, `otel-gorilla-ws`), so an application MAY install a module-scoped provider with `SetNamedProvider`.

#### Scenario: Library never mutates OpenFeature global state
- **WHEN** any module in this repository is built
- **THEN** no source file outside `_test.go` files references `SetProvider`, `SetNamedProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown` from `github.com/open-feature/go-sdk/openfeature`

#### Scenario: No provider installed by the application
- **WHEN** an application imports an instrumentation module but never installs an OpenFeature provider
- **THEN** every flag resolves to its environment-variable default and span on/off behavior matches that environment-only resolution, except where a design-documented env-only rule change applies (otel-gorilla-ws negotiation gated on the global switch alone; see `websocket-tracing` and design D9/R4)

#### Scenario: Application installs a module-scoped provider
- **WHEN** an application calls `openfeature.SetNamedProvider("otel-mongo", p)` in addition to a default provider
- **THEN** `otel-mongo`'s flags resolve through `p` while the other modules resolve through the default provider

### Requirement: Environment variable is the OpenFeature default value
Each dynamic flag SHALL be resolved as `client.Boolean(ctx, spec.Key, flags.EnvEnabled(spec.EnvVar), openfeature.EvaluationContext{})`. Because `Client.Boolean` returns the supplied default on every failure path, the environment variable SHALL govern whenever the relay has no usable opinion — no provider installed, provider not ready, flag absent from the relay configuration, evaluation error, or type mismatch. The library SHALL NOT inspect or branch on the evaluation error.

#### Scenario: Relay value overrides a falsy environment variable
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is unset and the relay resolves `otel-mongo-tracing` to `true`
- **THEN** Mongo wrapper spans are enabled

#### Scenario: Relay value overrides a truthy environment variable
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is `true` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** Mongo wrapper spans are disabled

#### Scenario: Flag absent from the relay falls back to the environment
- **WHEN** a provider is installed but its configuration contains no flag with the module's key
- **THEN** the environment variable's value decides, exactly as it did before this change

#### Scenario: Provider evaluation error falls back to the environment
- **WHEN** the installed provider returns an error or is not yet ready for a flag key
- **THEN** the environment variable's value decides and no error is surfaced to the caller

### Requirement: Global switch is an environment-only kill switch
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` SHALL have no OpenFeature flag key. When it resolves to disabled and no `WithTracingEnabled` option is present, the module SHALL NOT perform any OpenFeature evaluation, and no relay value SHALL be able to enable tracing or propagation for that process.

#### Scenario: Global switch off suppresses all evaluation
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy, no `WithTracingEnabled` option is passed, and the relay resolves every module flag to `true`
- **THEN** no module creates spans or propagates trace context, and no call to `Client.Boolean` is made

#### Scenario: Kill switch survives an unreachable relay
- **WHEN** the relay proxy is unreachable and `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is falsy
- **THEN** tracing remains disabled without depending on any relay interaction

### Requirement: Per-module snapshot with a one-second TTL
`internal/flags` SHALL expose a `Resolver` that holds an immutable snapshot of every flag value for one module in an `atomic.Pointer`, together with the instant the snapshot was taken. `Resolver.Enabled(i int)` SHALL load the pointer, compare that instant against the resolver's clock, and return the cached value when the snapshot is younger than the TTL. When the snapshot is absent or expired, the calling goroutine SHALL evaluate every `Spec` of that module and store a new snapshot. Refreshes SHALL NOT be serialized — concurrent refreshes are permitted and the last store wins. The snapshot's `at` timestamp SHALL be recorded at the **start** of the refresh (before any `client.Boolean` call), not after evaluation completes, so a slower refresh that observed older relay values cannot stamp a newer completion time over a fresher snapshot and keep stale values marked fresh for a full TTL. The TTL SHALL be fixed at one second and SHALL NOT be configurable through any exported API or environment variable. Specs within one refresh MAY be evaluated sequentially; parallel fan-out is not required.

#### Scenario: Repeated reads within the TTL do not re-evaluate
- **WHEN** `Enabled` is called many times within one second of a snapshot being taken
- **THEN** the provider is evaluated no more than once for that window and every call returns the cached value

#### Scenario: Read after the TTL re-evaluates
- **WHEN** `Enabled` is called more than one second after the snapshot was taken and the provider's value has changed
- **THEN** the new value is returned and a fresh snapshot is stored

#### Scenario: All flags of a module share one snapshot instant
- **WHEN** `otel-mongo`'s tracing and propagation flags are both changed on the relay between two snapshots
- **THEN** a reader observes either both old values or both new values, never one of each

#### Scenario: First read populates the snapshot
- **WHEN** `Enabled` is called for the first time on a freshly constructed `Resolver`
- **THEN** every `Spec` is evaluated once and the resulting snapshot is stored before the value is returned

#### Scenario: Snapshot timestamp is taken before evaluation
- **WHEN** two concurrent refreshes run and the later-finishing refresh began earlier and read older provider values
- **THEN** its stored snapshot's `at` reflects its start time (not completion), so it cannot appear strictly fresher than a refresh that started later solely by finishing later

### Requirement: Module-specific flag identity lives outside the shared file
`internal/flags` SHALL NOT contain module-scoped OpenFeature flag keys or module-scoped environment variable names. `Resolver` SHALL accept those as `Spec` values (`Key` for the OpenFeature flag key, `EnvVar` for the fallback environment variable), supplied by each module's own non-shared `env_flags.go`. The shared file MAY define the single process-wide kill-switch name `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` as `EnvGlobalTracing` and MAY export `GlobalTracingPossible() bool` as `EnvEnabled(EnvGlobalTracing)`. The byte-identical vendoring rule SHALL continue to apply to the whole of `internal/flags`, including the new `Resolver` code.

#### Scenario: Shared file names no module
- **WHEN** `internal/flags/flags.go` is inspected in any of the four modules
- **THEN** it contains no occurrence of `otel-mongo`, `otel-nats`, `otel-gorilla-ws`, or any module-scoped `OTEL_MONGO_*` / `OTEL_NATS_*` / `OTEL_GORILLA_WS_*` name

#### Scenario: Shared file may name only the global kill switch
- **WHEN** `internal/flags/flags.go` is inspected
- **THEN** the only `OTEL_*` environment variable name it may contain is `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, exposed as `EnvGlobalTracing` / `GlobalTracingPossible`

#### Scenario: Resolver code stays byte-identical
- **WHEN** the refresh, fallback, or TTL logic in one module's `internal/flags/flags.go` is modified
- **THEN** the same modification is applied to the other three copies so the file contents excluding the `package` declaration remain byte-identical

### Requirement: Fixed flag key vocabulary
The OpenFeature flag keys SHALL be exactly these, and SHALL NOT be overridable at runtime:

| Flag key | Fallback environment variable | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

#### Scenario: v1 and v2 Mongo share flag keys
- **WHEN** an application links both `otel-mongo` and `otel-mongo/v2` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** both modules disable their wrapper spans

#### Scenario: No key exists for the global switch
- **WHEN** the relay configuration defines a flag named after `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`
- **THEN** no module evaluates it and it has no effect

### Requirement: Evaluation context is supplied by the application
Modules SHALL pass an empty `openfeature.EvaluationContext{}` to every evaluation and SHALL NOT derive targeting attributes from `OTEL_SERVICE_NAME`, the OTel resource, the hostname, or any other source. Applications that require targeting SHALL install a global evaluation context with `openfeature.SetEvaluationContext`, which the SDK merges into every evaluation.

#### Scenario: Application-set targeting reaches the relay
- **WHEN** an application calls `openfeature.SetEvaluationContext` with a `service.name` attribute and the relay targets that attribute
- **THEN** the module resolves the targeted value without the module contributing any attribute of its own

#### Scenario: Request-scoped targeting is not supported
- **WHEN** the relay defines targeting rules keyed on an attribute that varies per request
- **THEN** the resolved value reflects only the process-wide evaluation context, because a single snapshot serves every caller for the TTL window

### Requirement: Test hooks for clock injection and in-memory providers
`NewResolver` SHALL accept a `WithClock(func() time.Time)` option so tests can advance time deterministically across the TTL boundary. Tests SHALL drive flag values through `memprovider.NewInMemoryProvider` installed with `openfeature.SetProviderAndWait`. Because both the OpenFeature provider and the process environment are global, tests that install a provider or call `t.Setenv` SHALL NOT call `t.Parallel`.

#### Scenario: Fake clock exercises the TTL boundary
- **WHEN** a test advances the injected clock by 900 ms after a snapshot and reads a changed provider value
- **THEN** the stale cached value is returned; advancing to 1100 ms and reading again returns the new value

#### Scenario: In-memory provider drives flag values
- **WHEN** a test installs an `InMemoryProvider` with a flag set to `false` while the corresponding environment variable is truthy
- **THEN** the resolver returns `false`
