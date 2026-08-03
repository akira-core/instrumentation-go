## ADDED Requirements

### Requirement: The application owns the default provider; the library reads through one domain
No package in this repository SHALL call `openfeature.SetProvider`, `openfeature.SetEvaluationContext`, `openfeature.AddHooks`, or `openfeature.Shutdown`. Each module SHALL obtain a client via `openfeature.NewClient(FlagDomain)` and read from it, mirroring the existing rule that packages never initialize a `TracerProvider` and instead fall back to `otel.GetTracerProvider()`.

`FlagDomain` SHALL be the single process-scoped value `otel-instrumentation-go`, shared by all four modules. Per-module domains SHALL NOT be used: `InProcess.Init` is not idempotent, so registering one provider instance under N domains starts N pollers of which N−1 can never be stopped, and N separate instances would poll the relay N times over identical configuration.

The only OpenFeature global state a module MAY mutate is a **named** provider bound to `FlagDomain`, and only under *Provider auto-install from the environment* below. The default provider, the global evaluation context, hooks and shutdown SHALL remain the application's, so nothing the library does can change how the application's own feature flags resolve.

#### Scenario: Library never touches the default provider
- **WHEN** any module in this repository is built
- **THEN** no source file outside `_test.go` files references `SetProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown` from `github.com/open-feature/go-sdk/openfeature`, and the only `SetNamedProvider` call site binds `FlagDomain`

#### Scenario: No provider installed and no endpoint configured
- **WHEN** an application imports an instrumentation module, never installs an OpenFeature provider, and does not set `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`
- **THEN** every relay verdict resolves to "allowed" and the module's behavior is decided entirely by its environment variables, identical to the release preceding this change except where the truthiness allow-list applies

#### Scenario: An application's own flags are unaffected
- **WHEN** an application resolves its own business flags through the default provider while a module has auto-installed a named provider on `FlagDomain`
- **THEN** the application's flags resolve through its own provider, unchanged

### Requirement: Provider readiness is the application's call
Because an unresolvable flag means "do not interfere", a provider that has not yet fetched its configuration cannot revoke anything. The auto-install path SHALL register non-blocking, so that the first instrumented operation of a process never waits on a relay round trip. The window this leaves — from install until the provider's first successful fetch, during which every flag resolves to `true` — SHALL be documented together with the way to close it, and SHALL NOT be stated as a requirement on the application.

An application that cannot accept the window SHALL install its own provider with `openfeature.SetProviderAndWait` before constructing any wrapper, which both closes the window and makes the auto-install path stand down.

The library SHALL NOT attempt to detect or compensate for a not-ready provider: doing so would require branching on evaluation errors, which the resolver deliberately does not do, and would give a not-ready provider a different meaning from an absent one.

#### Scenario: Non-blocking install leaves a window where a revocation is not in effect
- **WHEN** the relay is revoking a module whose environment variable is truthy, and an operation runs after the auto-install but before the provider's first successful fetch
- **THEN** the flag resolves to `true`, the module is enabled for that window, and the revocation takes effect once the provider becomes ready

#### Scenario: Blocking install closes the window
- **WHEN** an application calls `openfeature.SetProviderAndWait` before constructing any wrapper under the same conditions
- **THEN** the first operation already observes the revocation, and no auto-install occurs

#### Scenario: Documentation states the window and its cost
- **WHEN** `feature-flags.md` is read
- **THEN** it states that a process restarting under an active revocation is instrumented until the provider's first fetch, that for `otel-mongo` that window can write permanent `_oteltrace` fields, and that installing a provider with `SetProviderAndWait` closes it

### Requirement: A relay outage must not reach the application
A relay that becomes unreachable after the provider has fetched successfully SHALL NOT affect the application: the in-process evaluator serves its last successfully fetched configuration, so an active revocation survives the outage and no evaluation performs network I/O.

Two provider settings are required for that guarantee to hold, and SHALL be documented as requirements rather than suggestions, with their failure modes stated:

- **`DataCollectorDisabled: true`.** The provider's data collector is enabled by default, appends one event per evaluation to a bounded in-memory buffer, and does not clear that buffer when a flush fails. Once the buffer is full, every subsequent append flushes synchronously on the evaluating goroutine while holding the buffer's mutex, so a relay outage stalls every instrumented operation behind a failing request.
- **An install failure SHALL NOT abort startup.** When the relay is unreachable at boot the provider's first fetch fails; the auto-install path SHALL log and carry on, and an application installing its own provider SHALL be documented as doing the same, so the relay is not a hard dependency. The documentation SHALL state the unavoidable cost: a process that starts during a relay outage cannot learn about an active revocation and comes up at the state its environment declares.

On the auto-install path both settings SHALL be enforced in code — `DataCollectorDisabled: true` and `EvaluationType: INPROCESS` are hardcoded and SHALL NOT be exposed as environment variables — so the zero-code path cannot be misconfigured into either failure. For an application that installs its own provider the library SHALL NOT attempt to enforce either setting, and both SHALL remain documented as requirements with their failure modes stated.

#### Scenario: Relay goes down while a revocation is active
- **WHEN** a module has been revoked at the relay, the provider has fetched that configuration, and the relay then becomes unreachable
- **THEN** the module stays revoked, and no evaluation performs network I/O or blocks

#### Scenario: Relay is down at startup
- **WHEN** the relay is unreachable when the provider is installed
- **THEN** the failure is logged, startup continues, and the process runs at the state its environment declares with no relay control until the provider fetches successfully

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
2. `openfeature.ProviderMetadata().Name` equals `"NoopProvider"`, i.e. the application has installed no provider of its own.

Registration SHALL use `SetNamedProvider` on `FlagDomain` and SHALL be non-blocking. The provider SHALL be configured as follows, and no setting other than the three variables SHALL be exposed:

| Setting | Source |
|---|---|
| `Endpoint` | `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` |
| `APIKey` | `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` |
| `FlagChangePollingInterval` | `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL`, parsed with `time.ParseDuration`, default `60s` |
| `DataCollectorDisabled` | hardcoded `true` |
| `EvaluationType` | hardcoded in-process |

`OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` SHALL accept only Go duration strings; a bare integer SHALL fail to parse rather than be read as milliseconds. A parse failure SHALL warn through `slog.Default()`, fall back to `60s`, and **still install** — a malformed optional tuning value SHALL NOT remove the kill switch. An unset or empty endpoint SHALL install nothing and SHALL touch no OpenFeature state.

The default polling interval SHALL be `60s` and SHALL be set explicitly, because the provider's own default is 120 s and applies whenever the interval is non-positive.

`OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` SHALL NOT appear in any log, warning or error message.

No shutdown SHALL be exposed for an auto-installed provider; its polling goroutine lives for the process lifetime. An application requiring lifecycle control SHALL install its own provider.

#### Scenario: Endpoint set and no provider installed
- **WHEN** `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is set and the application has installed no provider, and the first instrumented operation runs
- **THEN** a GO Feature Flag provider is registered on `FlagDomain` and subsequent relay revocations reach the module

#### Scenario: Application-installed provider wins
- **WHEN** the application installs its own provider before the first instrumented operation and the endpoint variable is also set
- **THEN** no auto-install occurs and every verdict resolves through the application's provider

#### Scenario: Endpoint unset
- **WHEN** `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is unset or empty
- **THEN** no provider is constructed, no OpenFeature global state is written, and every verdict resolves to "allowed"

#### Scenario: Malformed polling interval keeps the kill switch
- **WHEN** `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` is set to `60` or any value `time.ParseDuration` rejects
- **THEN** a warning names the variable and its value, the interval falls back to `60s`, and the provider is still installed

#### Scenario: Two modules install concurrently
- **WHEN** two modules evaluate for the first time concurrently and both observe `NoopProvider`
- **THEN** both may register, the later registration replaces the earlier, the SDK shuts the earlier one down, and exactly one provider and one poller remain

### Requirement: The relay is a revoke-only kill switch
Each dynamic flag SHALL be resolved as `client.Boolean(ctx, key, true, evalCtx)` — where `evalCtx` is the resolver's evaluation context, empty except as *Evaluation context* below allows — and SHALL be combined with the module's environment variable by conjunction, not by supplying that variable as the evaluation default:

```
enabled := flags.EnvEnabled(moduleEnvVar) && resolver.Allowed(keyIndex)
```

A relay value of `false` SHALL disable a module whose environment variable enables it. No relay value SHALL be able to enable a module whose environment variable leaves it disabled. Because `Client.Boolean` returns the supplied default on every failure path, every failure — no provider installed, provider not ready, flag absent from the relay configuration, evaluation error, type mismatch, relay unreachable — SHALL resolve to `true`, meaning the relay does not interfere and the environment alone decides. The library SHALL NOT inspect or branch on the evaluation error.

When the module's environment variable resolves to disabled, the module SHALL short-circuit and SHALL NOT consult the resolver at all.

#### Scenario: Relay revokes a module the environment enabled
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is `true` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** Mongo wrapper spans are disabled

#### Scenario: Relay cannot enable a module the environment left off
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is unset or falsy and the relay resolves `otel-mongo-tracing` to `true`
- **THEN** Mongo wrapper spans remain disabled and no `Client.Boolean` call is made for that flag

#### Scenario: Flag absent from the relay leaves the environment in charge
- **WHEN** a provider is installed but its configuration contains no flag with the module's key
- **THEN** the evaluation returns the default `true`, the conjunction reduces to the environment variable, and behavior matches the release preceding this change

#### Scenario: Provider evaluation error leaves the environment in charge
- **WHEN** the installed provider returns an error, is not yet ready, or is unreachable
- **THEN** the evaluation returns `true`, the environment variable decides, and no error is surfaced to the caller

#### Scenario: Relay outage does not change a running process
- **WHEN** a process is tracing under a truthy module environment variable and the relay becomes unreachable
- **THEN** tracing continues at the state the deployment declared, rather than falling back to disabled

### Requirement: The first-tier switch is expressible in exactly one place
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and the per-connection option `WithTracingEnabled(v bool)` SHALL be two spellings of the same tier. A constructor SHALL return an error when the environment variable is **present** (by `os.LookupEnv`, regardless of its value) and the option is also supplied. The check SHALL be on presence, not on value: supplying both with the same value SHALL also be an error.

When neither is supplied the tier resolves to disabled. When exactly one is supplied it decides. When the tier resolves to disabled the module SHALL construct only its passthrough implementation, SHALL perform no OpenFeature evaluation, and no relay value SHALL be able to enable tracing or propagation for that connection.

`otel-mongo` SHALL apply the same presence-based mutual exclusion between `OTEL_MONGO_PROPAGATION_ENABLED` and `WithTracePropagationEnabled`.

#### Scenario: Environment variable and option together are rejected
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set to any value and a constructor is also passed `WithTracingEnabled(true)`
- **THEN** construction returns an error identifying both the option value and the environment variable value, and no connection or client is created

#### Scenario: Same value in both places is still rejected
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is `true` and a constructor is passed `WithTracingEnabled(true)`
- **THEN** construction returns the same error, because the rule is "set exactly one", not "make them agree"

#### Scenario: Option alone supplies the tier
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset and a constructor is passed `WithTracingEnabled(true)`
- **THEN** construction succeeds, the instrumented implementation is built, and the module tier and relay verdict decide per operation

#### Scenario: Tier off suppresses all evaluation
- **WHEN** neither the environment variable nor the option enables the tier, and the relay resolves every module flag to `true`
- **THEN** no module creates spans or propagates trace context, and no call to `Client.Boolean` is made

#### Scenario: Mongo propagation exclusion
- **WHEN** `OTEL_MONGO_PROPAGATION_ENABLED` is set to any value and `ConnectWithOptions` is also passed `WithTracePropagationEnabled(false)`
- **THEN** construction returns an error naming both settings

### Requirement: Environment truthiness is an explicit allow-list
`flags.EnvEnabled(name)` SHALL return `true` only when the variable is set and its value, lowercased and whitespace-trimmed, is one of `1`, `true`, `yes`, `on`. An unset variable SHALL return `false`. Every other set value — including the empty string, `0`, `false`, `no`, `off`, `enabled`, `2` — SHALL return `false`.

`flags.EnvSet(name)` SHALL report presence only, via `os.LookupEnv`, so callers can distinguish "unset" from "set to a value that reads as disabled". `EnvSet` SHALL be used for the mutual-exclusion check and SHALL NOT be used to decide whether a switch is enabled.

**A set-but-unrecognised value SHALL warn.** When the variable is present and its value is in neither the truthy nor the falsy list, `EnvEnabled` SHALL emit one `slog.Warn` naming the variable, the observed value and the accepted values, then return `false`. Three cases SHALL stay silent, so a correct deployment logs nothing: unset, a value in the truthy list, and a value in the falsy list.

The warning SHALL NOT be deduplicated. A process constructing N wrappers emits it N times; suppressing that would require mutable state in the file the byte-identical rule covers, to quieten a message that only appears when a deployment is already misconfigured.

#### Scenario: Allow-list values enable
- **WHEN** a switch is set to `1`, `true`, `yes`, `on`, `TRUE`, `On`, or `  yes  `
- **THEN** `EnvEnabled` reports enabled

#### Scenario: Empty string does not enable
- **WHEN** a switch is exported with an empty value (`export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=`)
- **THEN** `EnvEnabled` reports disabled, and `EnvSet` reports present

#### Scenario: Unrecognised values do not enable, and say so
- **WHEN** a switch is set to `enabled`, `2`, `y`, or `t`
- **THEN** `EnvEnabled` reports disabled and emits a warning naming the variable, the value, and the accepted values

#### Scenario: Correct configuration is silent
- **WHEN** a switch is unset, or set to a value in the truthy or the falsy list
- **THEN** no warning is emitted

#### Scenario: Presence is distinguishable from falsity
- **WHEN** a switch is set to `false`
- **THEN** `EnvSet` reports present and `EnvEnabled` reports disabled, so the mutual-exclusion check fires while the switch itself stays off

### Requirement: The relay verdict is resolved per operation
`internal/flags` SHALL expose a `Resolver` holding the module's OpenFeature client and its flag keys, whose `Allowed(i int)` evaluates the key at index `i` on **every** call. It SHALL NOT cache verdicts, and SHALL therefore contain no snapshot, no TTL, no clock and no refresh. An out-of-range index SHALL return `false` rather than panic, so a mis-wired module degrades to the disabled path instead of taking the process down.

The OpenFeature client SHALL be created lazily on first use rather than in `NewResolver`, so a process whose switches are off never initializes any part of the OpenFeature SDK. The environment-driven provider install SHALL hang on that same `sync.Once`, which gives it the same property for free: `Allowed` is reached only when a wrapper was built on the instrumented path, so switches-off processes never construct a provider either.

`NewResolver` SHALL NOT take a domain parameter. The domain is process-scoped, and holding it as a constant in the shared file removes a string that would otherwise have to agree across five places with nothing checking it.

A revocation SHALL therefore take effect on the next operation, bounded only by the provider's own polling interval. A module needing more than one verdict for one operation SHALL make consecutive `Allowed` calls; the microsecond window between them is accepted and is not grounds for adding a cache.

Caching remains a permitted future optimisation: it SHALL be addable inside `Resolver` without changing `Allowed`'s signature or any call site. An implementation that adds it SHALL restate the TTL, timestamp-placement, multi-flag-consistency and snapshot-immutability decisions that the uncached form makes unnecessary.

#### Scenario: Every operation observes the current relay value
- **WHEN** a relay flag is revoked while a wrapper is in use
- **THEN** the very next operation observes the revocation, with no waiting period beyond the provider's own polling

#### Scenario: No client is created while the switches are off
- **WHEN** a process runs with `gate1` or the module environment variable disabled
- **THEN** `openfeature.NewClient` is never called for that module and no evaluation is performed

#### Scenario: Out-of-range index degrades to disabled
- **WHEN** `Allowed` is called with an index outside the resolver's key list
- **THEN** it returns `false` and does not panic

#### Scenario: Two consecutive verdicts may tear
- **WHEN** an operation reads a module's tracing and propagation verdicts as two consecutive `Allowed` calls and the relay changes both between them
- **THEN** the operation MAY observe one old and one new verdict; the window is microseconds wide and is accepted, and for the tracing/propagation pair a disabled tracing verdict short-circuits propagation so the combination fails safe

### Requirement: Module-specific flag identity lives outside the shared file
`internal/flags` SHALL NOT contain module-scoped OpenFeature flag keys or module-scoped environment variable names. `Resolver` SHALL accept the OpenFeature flag keys through `WithFlagKeys(keys ...string)`, supplied by each module's own non-shared `env_flags.go`, which also owns the paired environment variable and performs the conjunction itself. The resolver SHALL NOT hold or read any **module-scoped** environment variable name — a field pairing a key with its environment variable would have no reader, since the environment variable is no longer the evaluation default.

The shared file MAY define **process-scoped** names, which are properties of the binary rather than of any module. These are exactly:

| Name | Exposed as |
|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `EnvGlobalTracing`, with `GlobalTracingPossible() bool` and `GlobalTracingSet() bool` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | `EnvFlagsEndpoint` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | `EnvFlagsAPIKey` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | `EnvFlagsPollInterval` |
| `OTEL_SERVICE_NAME` | `EnvServiceName` — the OTel-specified targeting attribute source |
| — | `FlagDomain`, the single OpenFeature domain, exported because module-package tests install their provider on it |

The byte-identical vendoring rule SHALL continue to apply to the whole of `internal/flags`, including the `Resolver` code and the provider install.

#### Scenario: Shared file names no module
- **WHEN** `internal/flags/flags.go` is inspected in any of the four modules
- **THEN** it contains no occurrence of `otel-mongo`, `otel-nats`, `otel-gorilla-ws`, or any module-scoped `OTEL_MONGO_*` / `OTEL_NATS_*` / `OTEL_GORILLA_WS_*` name

#### Scenario: Shared file may name only process-scoped switches
- **WHEN** `internal/flags/flags.go` is inspected
- **THEN** the only `OTEL_*` environment variable names it contains are `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, the three `OTEL_INSTRUMENTATION_GO_FLAGS_*` names and `OTEL_SERVICE_NAME`, and the only OpenFeature domain it contains is `FlagDomain`

#### Scenario: A drifted domain constant silently disables one module's kill switch
- **WHEN** one copy of `internal/flags/flags.go` carries a different `FlagDomain` value from the others
- **THEN** that module resolves through a domain no provider is bound to, falls back to the default provider, and stops receiving relay revocations — which no test detects, so the byte-identical rule covers this constant explicitly

#### Scenario: Resolver code stays byte-identical
- **WHEN** the evaluation call or the truthiness rules in one module's `internal/flags/flags.go` are modified
- **THEN** the same modification is applied to the other three copies so the file contents excluding the `package` declaration remain byte-identical

### Requirement: Fixed flag key vocabulary
The OpenFeature flag keys SHALL be exactly these, and SHALL NOT be overridable at runtime. The paired environment variable is a separate conjunctive tier, **not** the flag's evaluation default:

| Flag key | Paired environment variable | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

#### Scenario: v1 and v2 Mongo share flag keys
- **WHEN** an application links both `otel-mongo` and `otel-mongo/v2` and the relay resolves `otel-mongo-tracing` to `false`
- **THEN** both modules disable their wrapper spans

#### Scenario: No key exists for the first-tier switch
- **WHEN** the relay configuration defines a flag named after `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`
- **THEN** no module evaluates it and it has no effect

### Requirement: Evaluation context is the application's, except for one attribute on the auto-install path
Modules SHALL NOT call `openfeature.SetEvaluationContext`. Applications that require targeting and install their own provider SHALL install a global evaluation context themselves, which the SDK merges into every evaluation, and the resolver SHALL contribute no attribute of its own on that path.

When — and only when — the resolver auto-installed the provider, it SHALL populate its evaluation context with a single attribute derived from the OpenTelemetry-specified `OTEL_SERVICE_NAME`:

```go
if svc := os.Getenv("OTEL_SERVICE_NAME"); svc != "" {
    r.evalCtx = openfeature.NewTargetlessEvaluationContext(
        map[string]any{"service.name": svc})
}
```

This attribute SHALL be passed at the invocation site, never through `SetEvaluationContext`, so it composes with an application's global context rather than replacing it. Confining it to the auto-install path removes the one collision the SDK's merge order (*API → transaction → client → invocation*) would otherwise create, since invocation-level attributes win.

No other source SHALL be used: not `OTEL_RESOURCE_ATTRIBUTES`, not the OTel resource, not the hostname. An unset `OTEL_SERVICE_NAME` SHALL yield an empty evaluation context.

#### Scenario: Auto-installed provider supports per-service targeting
- **WHEN** the resolver auto-installed the provider, `OTEL_SERVICE_NAME` is `checkout-api`, and the relay targets `service.name eq "checkout-api"` with a disabled variation
- **THEN** that process resolves the targeted verdict while other services on the same flag resolve the default rule

#### Scenario: Application-installed provider is not augmented
- **WHEN** the application installed its own provider and set a global evaluation context
- **THEN** the resolver contributes no attribute, and a `service.name` the application set is not overridden

#### Scenario: Service name unset
- **WHEN** `OTEL_SERVICE_NAME` is unset on the auto-install path
- **THEN** the evaluation context is empty and a relay flag applies to every process in the fleet

#### Scenario: Request-scoped targeting is not supported
- **WHEN** the relay defines targeting rules keyed on an attribute that varies per request
- **THEN** the resolved verdict reflects only process-scoped attributes, because the resolver holds no per-request state

### Requirement: Supported provider evaluation mode
The GO Feature Flag provider's in-process evaluation mode, in which the provider polls the relay in the background and each `Boolean` call is a local lookup, SHALL be the only supported mode. On the auto-install path it SHALL be hardcoded, making remote evaluation unreachable. For an application-installed provider it SHALL be documented as the supported mode and remote evaluation as unsupported, because remote evaluation makes every evaluation an HTTP request and therefore places a synchronous network round trip on the path of every instrumented operation.

#### Scenario: In-process provider keeps evaluation local
- **WHEN** an application constructs the provider with only an `Endpoint` and installs it
- **THEN** every evaluation is a local lookup against the provider's polled configuration and issues no request on the operation path

#### Scenario: Documentation states the constraint
- **WHEN** the README and CLAUDE.md wiring snippets are read
- **THEN** `feature-flags.md` states that in-process evaluation is the supported mode and that remote evaluation is not supported

### Requirement: Tests drive verdicts through an in-memory provider
Tests SHALL drive relay verdicts through `memprovider.NewInMemoryProvider` installed with **`openfeature.SetNamedProviderAndWait(FlagDomain, …)`**, and SHALL observe a change on the next operation without sleeping, injecting a clock, or calling a reset hook. Because both the OpenFeature provider and the process environment are global, tests that install a provider or call `t.Setenv` SHALL NOT call `t.Parallel`.

Installing on the default provider SHALL NOT be used: a named provider on `FlagDomain` outranks the default for the resolver's clients, so once any earlier test in the same binary has triggered an auto-install, a default-provider installation is silently shadowed and the assertion reads that provider's values instead. `clientOnce` makes the install a once-per-process event that no test can undo, since `ResetForTest` is deleted.

The test environment SHALL leave `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` unset, so no test triggers an auto-install incidentally or reaches the network. Tests exercising the auto-install path SHALL set it deliberately and assert on the registration.

Tests SHALL cover the kill-switch asymmetry explicitly in both directions.

#### Scenario: A revocation is visible on the next call
- **WHEN** a test mutates the installed in-memory provider's flag from `true` to `false`
- **THEN** the next operation observes the revocation, with no sleep and no clock manipulation

#### Scenario: In-memory provider drives revocation
- **WHEN** a test installs an `InMemoryProvider` with a flag set to `false` while the paired environment variable is truthy
- **THEN** the module resolves to disabled

#### Scenario: In-memory provider cannot enable
- **WHEN** a test installs an `InMemoryProvider` with a flag set to `true` while the paired environment variable is unset or falsy
- **THEN** the module resolves to disabled and no evaluation is performed
