## ADDED Requirements

### Requirement: Application owns the OpenFeature provider
No package in this repository SHALL call `openfeature.SetProvider`, `openfeature.SetNamedProvider`, `openfeature.SetEvaluationContext`, `openfeature.AddHooks`, or `openfeature.Shutdown`. Each module SHALL obtain a client via `openfeature.NewClient(domain)` and read from it only, mirroring the existing rule that packages never initialize a `TracerProvider` and instead fall back to `otel.GetTracerProvider()`. The domain SHALL be the module name (`otel-mongo` for both v1 and v2, `otel-nats`, `otel-gorilla-ws`), so an application MAY install a module-scoped provider with `SetNamedProvider`.

#### Scenario: Library never mutates OpenFeature global state
- **WHEN** any module in this repository is built
- **THEN** no source file outside `_test.go` files references `SetProvider`, `SetNamedProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown` from `github.com/open-feature/go-sdk/openfeature`

#### Scenario: No provider installed by the application
- **WHEN** an application imports an instrumentation module but never installs an OpenFeature provider
- **THEN** every relay verdict resolves to "allowed" and the module's behavior is decided entirely by its environment variables, identical to the release preceding this change except where the truthiness allow-list applies

#### Scenario: Application installs a module-scoped provider
- **WHEN** an application calls `openfeature.SetNamedProvider("otel-mongo", p)` in addition to a default provider
- **THEN** `otel-mongo`'s flags resolve through `p` while the other modules resolve through the default provider

### Requirement: The provider must be ready before the first traced operation
Because an unresolvable flag means "do not interfere", a provider that has not yet fetched its configuration cannot revoke anything. Applications SHALL install the provider with `openfeature.SetProviderAndWait`, or otherwise block until it reports ready, before serving traffic. The documentation SHALL state this as a requirement and SHALL give the reason, since it is a consequence of the kill-switch model that an application cannot derive on its own.

The library SHALL NOT attempt to detect or compensate for a not-ready provider: doing so would require branching on evaluation errors, which the resolver deliberately does not do, and would give a not-ready provider a different meaning from an absent one.

#### Scenario: Non-blocking install leaves a window where a revocation is not in effect
- **WHEN** an application calls `openfeature.SetProvider` (not `SetProviderAndWait`) while the relay is revoking a module whose environment variable is truthy, and an operation runs before the provider's first successful fetch
- **THEN** the flag resolves to `true`, the module is enabled for that window, and the revocation only takes effect once the provider becomes ready

#### Scenario: Blocking install closes the window
- **WHEN** an application calls `openfeature.SetProviderAndWait` before serving traffic under the same conditions
- **THEN** the first operation already observes the revocation

#### Scenario: Restart under an active revocation
- **WHEN** a process whose module is revoked at the relay restarts, and the application blocks on provider readiness during startup
- **THEN** no operation is instrumented after the restart, matching the state the operator established before it

### Requirement: The relay is a revoke-only kill switch
Each dynamic flag SHALL be resolved as `client.Boolean(ctx, key, true, openfeature.EvaluationContext{})` and SHALL be combined with the module's environment variable by conjunction, not by supplying that variable as the evaluation default:

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

#### Scenario: Allow-list values enable
- **WHEN** a switch is set to `1`, `true`, `yes`, `on`, `TRUE`, `On`, or `  yes  `
- **THEN** `EnvEnabled` reports enabled

#### Scenario: Empty string does not enable
- **WHEN** a switch is exported with an empty value (`export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=`)
- **THEN** `EnvEnabled` reports disabled, and `EnvSet` reports present

#### Scenario: Unrecognised values do not enable
- **WHEN** a switch is set to `enabled`, `2`, `y`, or `t`
- **THEN** `EnvEnabled` reports disabled

#### Scenario: Presence is distinguishable from falsity
- **WHEN** a switch is set to `false`
- **THEN** `EnvSet` reports present and `EnvEnabled` reports disabled, so the mutual-exclusion check fires while the switch itself stays off

### Requirement: The relay verdict is resolved per operation
`internal/flags` SHALL expose a `Resolver` holding the module's OpenFeature client and its flag keys, whose `Allowed(i int)` evaluates the key at index `i` on **every** call. It SHALL NOT cache verdicts, and SHALL therefore contain no snapshot, no TTL, no clock and no refresh. An out-of-range index SHALL return `false` rather than panic, so a mis-wired module degrades to the disabled path instead of taking the process down.

The OpenFeature client SHALL be created lazily on first use rather than in `NewResolver`, so a process whose switches are off never initializes any part of the OpenFeature SDK.

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
`internal/flags` SHALL NOT contain module-scoped OpenFeature flag keys or module-scoped environment variable names. `Resolver` SHALL accept the OpenFeature flag keys through `WithFlagKeys(keys ...string)`, supplied by each module's own non-shared `env_flags.go`, which also owns the paired environment variable and performs the conjunction itself. The resolver SHALL NOT hold or read any environment variable name other than the process-wide one below — a field pairing a key with its environment variable would have no reader, since the environment variable is no longer the evaluation default. The shared file MAY define the single process-wide first-tier name `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` as `EnvGlobalTracing` and MAY export `GlobalTracingPossible() bool` and `GlobalTracingSet() bool`. The byte-identical vendoring rule SHALL continue to apply to the whole of `internal/flags`, including the `Resolver` code.

#### Scenario: Shared file names no module
- **WHEN** `internal/flags/flags.go` is inspected in any of the four modules
- **THEN** it contains no occurrence of `otel-mongo`, `otel-nats`, `otel-gorilla-ws`, or any module-scoped `OTEL_MONGO_*` / `OTEL_NATS_*` / `OTEL_GORILLA_WS_*` name

#### Scenario: Shared file may name only the first-tier switch
- **WHEN** `internal/flags/flags.go` is inspected
- **THEN** the only `OTEL_*` environment variable name it may contain is `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, exposed as `EnvGlobalTracing` / `GlobalTracingPossible` / `GlobalTracingSet`

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

### Requirement: Evaluation context is supplied by the application
Modules SHALL pass an empty `openfeature.EvaluationContext{}` to every evaluation and SHALL NOT derive targeting attributes from `OTEL_SERVICE_NAME`, the OTel resource, the hostname, or any other source. Applications that require targeting SHALL install a global evaluation context with `openfeature.SetEvaluationContext`, which the SDK merges into every evaluation.

#### Scenario: Application-set targeting reaches the relay
- **WHEN** an application calls `openfeature.SetEvaluationContext` with a `service.name` attribute and the relay targets that attribute
- **THEN** the module resolves the targeted verdict without the module contributing any attribute of its own

#### Scenario: Request-scoped targeting is not supported
- **WHEN** the relay defines targeting rules keyed on an attribute that varies per request
- **THEN** the resolved verdict reflects only the process-wide evaluation context, because the resolver contributes no attributes and holds no per-request state

### Requirement: Supported provider evaluation mode
The documented wiring SHALL use the GO Feature Flag provider's default in-process evaluation mode, in which the provider polls the relay in the background and each `Boolean` call is a local lookup. Remote evaluation mode SHALL be documented as unsupported, because it makes every evaluation an HTTP request and therefore places a synchronous network round trip on the path of every instrumented operation.

#### Scenario: In-process provider keeps evaluation local
- **WHEN** an application constructs the provider with only an `Endpoint` and installs it
- **THEN** every evaluation is a local lookup against the provider's polled configuration and issues no request on the operation path

#### Scenario: Documentation states the constraint
- **WHEN** the README and CLAUDE.md wiring snippets are read
- **THEN** `feature-flags.md` states that in-process evaluation is the supported mode and that remote evaluation is not supported

### Requirement: Tests drive verdicts through an in-memory provider
Tests SHALL drive relay verdicts through `memprovider.NewInMemoryProvider` installed with `openfeature.SetProviderAndWait`, and SHALL observe a change on the next operation without sleeping, injecting a clock, or calling a reset hook. Because both the OpenFeature provider and the process environment are global, tests that install a provider or call `t.Setenv` SHALL NOT call `t.Parallel`.

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
