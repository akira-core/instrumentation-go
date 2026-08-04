## ADDED Requirements

### Requirement: The application owns the default provider; the library reads through one domain
No package in this repository SHALL call `openfeature.SetProvider`, `openfeature.SetEvaluationContext`, `openfeature.AddHooks`, or `openfeature.Shutdown`. `otel-flags` SHALL obtain a client via `openfeature.NewClient(FlagDomain)` and read from it, mirroring the existing rule that packages never initialize a `TracerProvider` and instead fall back to `otel.GetTracerProvider()`.

`FlagDomain` SHALL be the single process-scoped value `otel-instrumentation-go`, shared by every module. Per-module domains SHALL NOT be used: `InProcess.Init` is not idempotent, so registering one provider instance under N domains starts N pollers of which N−1 can never be stopped, and N separate instances would poll the relay N times over identical configuration.

The only OpenFeature global state the library MAY mutate is a **named** provider bound to `FlagDomain`, and only under *Provider auto-install from the environment* below. The default provider, the global evaluation context, hooks and shutdown SHALL remain the application's, so nothing the library does can change how the application's own feature flags resolve.

#### Scenario: Library never touches the default provider
- **WHEN** any module in this repository is built
- **THEN** no source file outside `_test.go` files references `SetProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown` from `github.com/open-feature/go-sdk/openfeature`, and the only `SetNamedProvider` call site binds `FlagDomain`

#### Scenario: No provider installed and no endpoint configured
- **WHEN** an application imports an instrumentation module, never installs an OpenFeature provider, and does not set `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`
- **THEN** `relayPossible` is false, no OpenFeature client is created, and every switch resolves from its option, its environment variable and its hardcoded default alone

#### Scenario: An application's own flags are unaffected
- **WHEN** an application resolves its own business flags through the default provider while the library has auto-installed a named provider on `FlagDomain`
- **THEN** the application's flags resolve through its own provider, unchanged

### Requirement: The relay is authoritative in both directions
Each switch SHALL be resolved as `client.Boolean(ctx, key, local, evalCtx)`, where `local` is the value the option, the environment variable or the hardcoded default supplied at construction, and `evalCtx` is the resolver's evaluation context — empty except as *Evaluation context* below allows.

That single call SHALL be the whole precedence ladder. A relay value SHALL override `local` in **either** direction: a relay `false` disables a switch the deployment enabled, and a relay `true` enables a switch the deployment left off. Because `Client.Boolean` returns the supplied default on every path where the relay has no usable answer — no provider installed, provider not ready, key absent from the relay configuration, evaluation error, type mismatch, relay unreachable — every such path SHALL resolve to `local`, so the deployment's own answer stands. The library SHALL NOT inspect or branch on the evaluation error, and SHALL NOT use `BooleanValueDetails` to distinguish relay silence from relay failure, because both fall through to the same value.

The library SHALL NOT pass a literal `true` or `false` as the evaluation default. Doing so is what made the relay revoke-only, and is superseded.

#### Scenario: Relay disables a module the environment enabled
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is `true` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** Mongo wrapper spans are disabled

#### Scenario: Relay enables a module the environment left off
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is unset or falsy, `relayPossible` is true, and the relay resolves `otel-mongo-tracing` to `true`
- **THEN** Mongo wrapper spans are emitted

#### Scenario: Relay overrides a constructor option
- **WHEN** a wrapper is constructed with `WithTracingEnabled(false)` and the relay resolves that module's tracing key to `true`
- **THEN** that wrapper emits spans, because the relay is the top rung

#### Scenario: Flag absent from the relay leaves the local value in charge
- **WHEN** a provider is installed but its configuration contains no flag with the module's key
- **THEN** the evaluation returns `local`, and behaviour matches a process with no relay at all

#### Scenario: Provider evaluation error leaves the local value in charge
- **WHEN** the installed provider returns an error, is not yet ready, or is unreachable
- **THEN** the evaluation returns `local`, and no error is surfaced to the caller

#### Scenario: Relay outage does not change a running process
- **WHEN** a process is tracing under a relay value that has already been fetched and the relay becomes unreachable
- **THEN** the in-process evaluator serves its last fetched configuration and the process continues at that state

### Requirement: The master switch is a veto with no option spelling
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and the relay key `otel-instrumentation-go-tracing` SHALL be the only two spellings of the process-wide master switch. Its hardcoded default SHALL be `true`.

The master switch SHALL be composed by conjunction above every per-module switch:

```
tracing = master && moduleTracing
```

A per-connection option SHALL NOT supply the master switch: `WithTracingEnabled` supplies the per-module tier for the connection it is passed to. A per-connection value cannot express a process-wide switch, and keeping the option off the master is what guarantees that one environment variable, or one relay flag, stops everything.

Because the default is `true`, setting either spelling to `true` SHALL have no observable effect. Documentation SHALL describe the master switch as "set to `false` to stop everything" and SHALL NOT present it as an enable.

The master switch SHALL be resolved per operation, like every other relay-backed switch, so a relay veto reaches connections that already exist.

#### Scenario: Master default does not enable anything
- **WHEN** no environment variable is set, no option is passed and no relay flag exists
- **THEN** the master resolves to `true`, every module tier resolves to its default of `false`, and no module traces

#### Scenario: Master veto from the environment stops every module
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is falsy while module variables are truthy and options enable modules
- **THEN** no module in the process emits spans or propagates trace context

#### Scenario: Master veto from the relay stops every module
- **WHEN** the relay resolves `otel-instrumentation-go-tracing` to `false`
- **THEN** every module in every process served by that relay stops emitting on its next operation, including connections constructed with `WithTracingEnabled(true)`

#### Scenario: Setting the master relay key to true is inert
- **WHEN** the relay resolves `otel-instrumentation-go-tracing` to `true` and every module tier resolves to `false`
- **THEN** no module traces, because the master only ever removes permission

### Requirement: Implementation selection keys on whether a relay can exist
`otel-flags` SHALL expose `RelayPossible() bool`, reporting `true` when `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is set to a non-empty value **or** `openfeature.NamedProviderMetadata(FlagDomain).Name` is not `"NoopProvider"`.

Each wrapper SHALL resolve it once, at construction, and SHALL allocate its instrumented implementation when:

```
useTracedImpl = relayPossible || (masterLocal && moduleTracingLocal)
```

When `relayPossible` is false the relay is structurally incapable of returning anything other than the value passed to it, so the wrapper SHALL resolve every switch from `env > option > default` alone, SHALL NOT create an OpenFeature client, and SHALL NOT perform any evaluation for the rest of its life. Every configuration that took the zero-cost passthrough path in the preceding release SHALL still take it, including the registration of `shared.NewCommandMonitor`, which SHALL be conditioned on the same expression.

When `relayPossible` is true both implementations SHALL be allocated and the per-operation resolution SHALL select between them, because the relay may enable a module the environment left off.

`RelayPossible` SHALL be resolved per construction and SHALL NOT be memoized process-wide. A process-wide memo would freeze the answer at whichever wrapper was built first, which in a test binary makes every subsequent relay test unreachable.

Everything derived from a wrapper — `oteljetstream.New(conn)`, `Client.Database()`, `Database.Collection()` — SHALL inherit its implementations and its gate state rather than re-resolving.

#### Scenario: No endpoint and no provider keeps the zero-cost path
- **WHEN** no endpoint variable is set, no provider is installed, and every switch resolves locally to disabled
- **THEN** only the passthrough implementation is allocated, `openfeature.NewClient` is never called, no `Client.Boolean` call is made, and no command monitor is registered

#### Scenario: Endpoint set allocates the instrumented implementation regardless of the environment
- **WHEN** the endpoint variable is set and the module's environment variable is unset
- **THEN** both implementations are allocated, so a later relay `true` can take effect without reconstruction

#### Scenario: A pre-installed provider makes the relay possible without the endpoint variable
- **WHEN** an application installs its own provider on `FlagDomain` and sets no endpoint variable, then constructs a wrapper
- **THEN** `relayPossible` is true for that wrapper and relay values reach it

#### Scenario: A wrapper built before the provider stays static
- **WHEN** a wrapper is constructed while no provider is installed and no endpoint variable is set, and the application installs a provider afterwards
- **THEN** that wrapper never observes a relay value, and the ordering requirement is documented

#### Scenario: Derived wrappers inherit the decision
- **WHEN** a `Conn` or `Client` was constructed on the passthrough-only path and a JetStream wrapper, `Database` or `Collection` is derived from it
- **THEN** the derived wrapper is also passthrough-only and does not re-resolve `relayPossible`

### Requirement: Provider readiness is the application's call
The auto-install path SHALL register non-blocking, so that the first instrumented operation of a process never waits on a relay round trip.

The window this leaves — from install until the provider's first successful fetch — SHALL resolve every switch to its locally supplied value. A process therefore starts at exactly the state its environment and options declare, and the relay's opinion arrives one fetch later. The window SHALL NOT be able to enable a switch the deployment left off, and for `otel-mongo` SHALL NOT be able to write a `_oteltrace` field the deployment did not configure.

An application that requires the relay's answer before its first operation SHALL install its own provider with `openfeature.SetProviderAndWait` before constructing any wrapper, which both closes the window and makes the auto-install path stand down. This SHALL be documented as the application's option, not as a requirement on it.

The library SHALL NOT attempt to detect or compensate for a not-ready provider: doing so would require branching on evaluation errors, which the resolver deliberately does not do, and would give a not-ready provider a different meaning from an absent one.

#### Scenario: The window delays an enable, not a disable
- **WHEN** the relay enables a module whose environment variable is unset, and an operation runs after the auto-install but before the provider's first successful fetch
- **THEN** the switch resolves to its local value of disabled, and the relay's enable takes effect once the provider becomes ready

#### Scenario: The window cannot introduce document writes
- **WHEN** a process starts with `OTEL_MONGO_PROPAGATION_ENABLED` unset and the endpoint variable set
- **THEN** no `_oteltrace` field is written during the window, because the evaluation default is the local value of `false`

#### Scenario: Blocking install closes the window
- **WHEN** an application calls `openfeature.SetProviderAndWait` before constructing any wrapper
- **THEN** the first operation already observes the relay's value, and no auto-install occurs

### Requirement: A relay outage must not reach the application
A relay that becomes unreachable after the provider has fetched successfully SHALL NOT affect the application: the in-process evaluator serves its last successfully fetched configuration, so the current state survives the outage and no evaluation performs network I/O.

Two provider settings are required for that guarantee to hold, and SHALL be documented as requirements rather than suggestions, with their failure modes stated:

- **`DataCollectorDisabled: true`.** The provider's data collector is enabled by default, appends one event per evaluation to a bounded in-memory buffer, and does not clear that buffer when a flush fails. Once the buffer is full, every subsequent append flushes synchronously on the evaluating goroutine while holding the buffer's mutex, so a relay outage stalls every instrumented operation behind a failing request.
- **An install failure SHALL NOT abort startup.** When the relay is unreachable at boot the provider's first fetch fails; the auto-install path SHALL log and carry on, and an application installing its own provider SHALL be documented as doing the same. The documentation SHALL state the unavoidable cost: a process that starts during a relay outage comes up at the state its environment and options declare, with no relay control until the provider fetches successfully.

On the auto-install path both settings SHALL be enforced in code — `DataCollectorDisabled: true` and `EvaluationType: INPROCESS` are hardcoded and SHALL NOT be exposed as environment variables. For an application that installs its own provider the library SHALL NOT attempt to enforce either setting, and both SHALL remain documented as requirements with their failure modes stated.

#### Scenario: Relay goes down while a flag value is in force
- **WHEN** a module's state has been set at the relay, the provider has fetched that configuration, and the relay then becomes unreachable
- **THEN** the module stays at that state, and no evaluation performs network I/O or blocks

#### Scenario: Relay is down at startup
- **WHEN** the relay is unreachable when the provider is installed
- **THEN** the failure is logged, startup continues, and the process runs at the state its environment and options declare

#### Scenario: The auto-installed provider cannot be misconfigured into a stall
- **WHEN** a provider is constructed by the auto-install path
- **THEN** `DataCollectorDisabled` is `true` and `EvaluationType` is in-process, and no environment variable can change either

#### Scenario: Data collector is disabled in every documented snippet
- **WHEN** a wiring snippet for an application-installed provider is read in this repository
- **THEN** it sets `DataCollectorDisabled: true` and explains why

### Requirement: Provider auto-install from the environment
An application SHALL be able to obtain relay control by setting environment variables alone, writing no Go code and adding no import.

`Resolver` SHALL, inside the same `sync.Once` that lazily creates the OpenFeature client, construct and register a provider when **both** of the following hold:

1. `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is set to a non-empty value; and
2. `openfeature.NamedProviderMetadata(FlagDomain).Name` equals `"NoopProvider"` — which is true only when the application has bound neither a default provider nor a named one to this domain, since that call falls back to the default provider's metadata.

Registration SHALL use `SetNamedProvider` on `FlagDomain` and SHALL be non-blocking. The provider SHALL be configured as follows, and no setting other than the three variables SHALL be exposed:

| Setting | Source |
|---|---|
| `Endpoint` | `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` |
| `APIKey` | `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` |
| `FlagChangePollingInterval` | `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL`, parsed with `time.ParseDuration`, default `60s` |
| `DataCollectorDisabled` | hardcoded `true` |
| `EvaluationType` | hardcoded in-process |

`OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` SHALL accept only Go duration strings; a bare integer SHALL fail to parse rather than be read as milliseconds. A parse failure SHALL warn through `slog.Default()`, fall back to `60s`, and **still install** — a malformed optional tuning value SHALL NOT remove the control plane. This is a deliberate asymmetry with the construction error an invalid `_ENABLED` value produces: the interval has a safe fallback and a switch does not.

An unset or empty endpoint SHALL install nothing and SHALL touch no OpenFeature state.

The default polling interval SHALL be `60s` and SHALL be set explicitly, because the provider's own default is 120 s and applies whenever the interval is non-positive.

`OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` SHALL NOT appear in any log, warning or error message.

Exactly one install SHALL occur per process. Because the shared flag logic lives in one module (see `shared-feature-flags` § *Single shared flags module*), one `sync.Once` governs it for every instrumentation module in the binary; concurrent first evaluations from different modules SHALL NOT produce two registrations.

No shutdown SHALL be exposed for an auto-installed provider; its polling goroutine lives for the process lifetime. An application requiring lifecycle control SHALL install its own provider.

#### Scenario: Endpoint set and no provider installed
- **WHEN** `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is set, the application has installed no provider, and the first instrumented operation runs
- **THEN** a GO Feature Flag provider is registered on `FlagDomain` and subsequent relay changes reach every module

#### Scenario: Application-installed provider wins
- **WHEN** the application installs its own provider before the first instrumented operation and the endpoint variable is also set
- **THEN** no auto-install occurs and every value resolves through the application's provider

#### Scenario: Endpoint unset
- **WHEN** `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is unset or empty and no provider is installed
- **THEN** no provider is constructed, no OpenFeature global state is written, and `RelayPossible()` reports false

#### Scenario: Malformed polling interval keeps the control plane
- **WHEN** `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` is set to `60` or any value `time.ParseDuration` rejects
- **THEN** a warning names the variable and its value, the interval falls back to `60s`, and the provider is still installed

#### Scenario: Four modules produce one install
- **WHEN** wrappers from all four instrumentation modules perform their first evaluation concurrently with the endpoint variable set
- **THEN** exactly one provider is registered, and no provider is registered and then replaced

### Requirement: Every relay-backed switch is resolved per operation
`otel-flags` SHALL expose a `Resolver` holding the OpenFeature client and a list of flag keys, whose `Value(i int, local bool) bool` evaluates the key at index `i` on **every** call, passing `local` as the evaluation default. It SHALL NOT cache values, and SHALL therefore contain no snapshot, no TTL, no clock and no refresh. An out-of-range index SHALL return `false` rather than panic, so a mis-wired module degrades to the disabled path instead of taking the process down.

The OpenFeature client SHALL be created lazily on first use rather than in `NewResolver`, so a process that never reaches a relay-backed path never initializes any part of the OpenFeature SDK. The environment-driven provider install SHALL hang on that same `sync.Once`.

`NewResolver` SHALL NOT take a domain parameter. The domain is process-scoped, and holding it as a constant in the shared module removes a string that would otherwise have to agree across five places with nothing checking it.

The master switch SHALL be resolved through the same mechanism, so an instrumented operation performs **two** evaluations for `otel-nats` and `otel-gorilla-ws`, two for an `otel-mongo` read, and **three** for an `otel-mongo` write. Each evaluation costs approximately 2.0 µs, 336 B and 7 allocations against an in-memory provider. This cost SHALL be recorded rather than assumed.

A relay change SHALL therefore take effect on the next operation, bounded only by the provider's own polling interval. A module needing more than one value for one operation SHALL make consecutive `Value` calls; the microsecond window between them is accepted and is not grounds for adding a cache.

Caching remains a permitted future optimisation: it SHALL be addable inside `Resolver` without changing `Value`'s signature or any call site. An implementation that adds it SHALL restate the TTL, timestamp-placement, multi-flag-consistency and snapshot-immutability decisions that the uncached form makes unnecessary.

#### Scenario: Every operation observes the current relay value
- **WHEN** a relay flag changes while a wrapper is in use
- **THEN** the very next operation observes the change, with no waiting period beyond the provider's own polling

#### Scenario: No client is created when no relay can exist
- **WHEN** `relayPossible` is false for a wrapper
- **THEN** `openfeature.NewClient` is never called for it and no evaluation is performed

#### Scenario: Out-of-range index degrades to disabled
- **WHEN** `Value` is called with an index outside the resolver's key list
- **THEN** it returns `false` and does not panic

#### Scenario: Consecutive values may tear
- **WHEN** an operation reads the master, tracing and propagation values as consecutive `Value` calls and the relay changes them between calls
- **THEN** the operation MAY observe a mixture of old and new values; the window is microseconds wide and is accepted, and because the composition is conjunctive a disabled master or tracing value short-circuits everything below it

### Requirement: Module-specific flag identity lives outside the shared module
`otel-flags` SHALL NOT contain module-scoped OpenFeature flag keys, module-scoped environment variable names, or module-scoped hardcoded defaults. `Resolver` SHALL accept the OpenFeature flag keys through `WithFlagKeys(keys ...string)`, supplied by each module's own `env_flags.go`, which also owns the paired environment variable and the tier's default and passes the resolved `local` value into `Value`. The resolver SHALL NOT hold or read any module-scoped environment variable name.

The shared module MAY define **process-scoped** names, which are properties of the binary rather than of any module. These are exactly:

| Name | Exposed as |
|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `EnvGlobalTracing`, with `MasterLocal() (bool, error)` and `MasterEnabled(local bool) bool` |
| `otel-instrumentation-go-tracing` | `FlagKeyGlobalTracing` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | `EnvFlagsEndpoint` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | `EnvFlagsAPIKey` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | `EnvFlagsPollInterval` |
| `OTEL_SERVICE_NAME` | `EnvServiceName` — the OTel-specified targeting attribute source |
| — | `FlagDomain`, the single OpenFeature domain, exported because module-package tests install their provider on it |
| — | `RelayPossible() bool`, `Lookup(name) (bool, bool, error)`, `ErrInvalidFlagValue` |

#### Scenario: Shared module names no module
- **WHEN** `otel-flags/flags.go` is inspected
- **THEN** it contains no occurrence of `otel-mongo`, `otel-nats`, `otel-gorilla-ws`, or any module-scoped `OTEL_MONGO_*` / `OTEL_NATS_*` / `OTEL_GORILLA_WS_*` name

#### Scenario: Shared module may name only process-scoped switches
- **WHEN** `otel-flags/flags.go` is inspected
- **THEN** the only `OTEL_*` environment variable names it contains are `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, the three `OTEL_INSTRUMENTATION_GO_FLAGS_*` names and `OTEL_SERVICE_NAME`, and the only OpenFeature keys it contains are `FlagKeyGlobalTracing` and `FlagDomain`

#### Scenario: Adding a module does not change the shared module
- **WHEN** a new instrumentation module with its own flag key and environment variable is added
- **THEN** `otel-flags` requires no change and no new release

### Requirement: Fixed flag key vocabulary
The OpenFeature flag keys SHALL be exactly these, SHALL NOT be overridable at runtime, and SHALL each pair with one environment variable and one hardcoded default:

| Flag key | Paired environment variable | Option | Default | Modules |
|---|---|---|---|---|
| `otel-instrumentation-go-tracing` | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | — | `true` | all |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `WithTracePropagationEnabled` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-gorilla-ws` |

The environment variable SHALL NOT be a separate conjunctive tier; it is the rung directly below the relay in one ladder — above the option, not below it — and its value reaches the relay as the evaluation default.

#### Scenario: v1 and v2 Mongo share flag keys
- **WHEN** an application links both `otel-mongo` and `otel-mongo/v2` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** both modules disable their wrapper spans

#### Scenario: The master key exists and only subtracts
- **WHEN** the relay configuration defines `otel-instrumentation-go-tracing`
- **THEN** the value `false` stops every module in the process, and the value `true` has no effect because it matches the default

### Requirement: Evaluation context is the application's, except for one attribute on the auto-install path
The library SHALL NOT call `openfeature.SetEvaluationContext`. Applications that require targeting and install their own provider SHALL install a global evaluation context themselves, which the SDK merges into every evaluation, and the resolver SHALL contribute no attribute of its own on that path.

When — and only when — the resolver auto-installed the provider, it SHALL populate its evaluation context with a single attribute derived from the OpenTelemetry-specified `OTEL_SERVICE_NAME`:

```go
if svc := os.Getenv("OTEL_SERVICE_NAME"); svc != "" {
    r.evalCtx = openfeature.NewTargetlessEvaluationContext(
        map[string]any{"service.name": svc})
}
```

This attribute SHALL be passed at the invocation site, never through `SetEvaluationContext`, so it composes with an application's global context rather than replacing it. Confining it to the auto-install path removes the one collision the SDK's merge order (*API → transaction → client → invocation*) would otherwise create, since invocation-level attributes win.

No other source SHALL be used: not `OTEL_RESOURCE_ATTRIBUTES`, not the OTel resource, not the hostname. An unset `OTEL_SERVICE_NAME` SHALL yield an empty evaluation context, and a relay flag SHALL then apply to every process in the fleet — which the documentation SHALL state as a caution, because a flag can now enable.

#### Scenario: Auto-installed provider supports per-service targeting
- **WHEN** the resolver auto-installed the provider, `OTEL_SERVICE_NAME` is `checkout-api`, and the relay targets `service.name eq "checkout-api"` with an enabled variation over a disabled default rule
- **THEN** that process traces while other services on the same flag do not

#### Scenario: Application-installed provider is not augmented
- **WHEN** the application installed its own provider and set a global evaluation context
- **THEN** the resolver contributes no attribute, and a `service.name` the application set is not overridden

#### Scenario: Service name unset
- **WHEN** `OTEL_SERVICE_NAME` is unset on the auto-install path
- **THEN** the evaluation context is empty and a relay flag applies to every process in the fleet

#### Scenario: Request-scoped targeting is not supported
- **WHEN** the relay defines targeting rules keyed on an attribute that varies per request
- **THEN** the resolved value reflects only process-scoped attributes, because the resolver holds no per-request state

### Requirement: Supported provider evaluation mode
The GO Feature Flag provider's in-process evaluation mode, in which the provider polls the relay in the background and each `Boolean` call is a local lookup, SHALL be the only supported mode. On the auto-install path it SHALL be hardcoded, making remote evaluation unreachable. For an application-installed provider it SHALL be documented as the supported mode and remote evaluation as unsupported, because remote evaluation makes every evaluation an HTTP request and therefore places a synchronous network round trip on the path of every instrumented operation — two or three of them per operation under this design.

#### Scenario: In-process provider keeps evaluation local
- **WHEN** an application constructs the provider with only an `Endpoint` and installs it
- **THEN** every evaluation is a local lookup against the provider's polled configuration and issues no request on the operation path

#### Scenario: Documentation states the constraint
- **WHEN** the README and CLAUDE.md wiring snippets are read
- **THEN** `feature-flags.md` states that in-process evaluation is the supported mode and that remote evaluation is not supported

### Requirement: Tests drive values through an in-memory provider
Tests SHALL drive relay values through `memprovider.NewInMemoryProvider` installed with **`openfeature.SetNamedProviderAndWait(FlagDomain, …)`**, and SHALL observe a change on the next operation without sleeping, injecting a clock, or calling a reset hook. Because both the OpenFeature provider and the process environment are global, tests that install a provider or call `t.Setenv` SHALL NOT call `t.Parallel`.

Installing on the default provider SHALL NOT be used: a named provider on `FlagDomain` outranks the default for the resolver's clients, so once any earlier test in the same binary has triggered an auto-install, a default-provider installation is silently shadowed. `clientOnce` makes the install a once-per-process event that no test can undo, since `ResetForTest` is deleted.

Tests SHALL install the provider **before** constructing the wrapper under test. A wrapper constructed while `relayPossible` is false resolves statically for its whole life and will never observe a flag change.

The test environment SHALL leave `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` unset except in tests that exercise the auto-install path deliberately, so no test reaches the network.

Tests SHALL cover both directions of the ladder for every switch.

#### Scenario: A relay change is visible on the next call
- **WHEN** a test mutates the installed in-memory provider's flag from `true` to `false`
- **THEN** the next operation observes the change, with no sleep and no clock manipulation

#### Scenario: In-memory provider drives an enable
- **WHEN** a test installs an `InMemoryProvider` with a module's flag set to `true` while the paired environment variable is unset, and constructs the wrapper afterwards
- **THEN** the module traces

#### Scenario: In-memory provider drives a disable
- **WHEN** a test installs an `InMemoryProvider` with a module's flag set to `false` while the paired environment variable is truthy
- **THEN** the module does not trace

#### Scenario: Provider installed after construction has no effect
- **WHEN** a test constructs a wrapper with no endpoint variable and no provider, then installs an in-memory provider
- **THEN** the wrapper continues to resolve statically, demonstrating the documented ordering requirement
