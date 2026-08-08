# shared-feature-flags Specification

## Purpose
Shared feature-flag primitives used by all instrumentation modules: environment
reads, the OpenFeature-backed `Resolver`, and per-connection overrides.

## Requirements
### Requirement: Composed per-module gates
Each module SHALL construct exactly one package-level `otelflags.Resolver` at package initialization and SHALL read it at each decision point rather than caching the result on a wrapper struct. The OpenFeature flag key SHALL be passed to `Resolver.Value` as a parameter, not held as a per-resolver list, so nothing positional can be mis-wired. `otel-nats` and `otel-gorilla-ws` SHALL each own a single tracing key; `otel-mongo` (v1 and v2) SHALL own a tracing key and a propagation key. The process-wide master switch SHALL be resolved through `otel-flags`' own resolver, not through any module's.

Each switch SHALL be resolved independently down a four-step precedence ladder, first source with an opinion winning:

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

The ladder SHALL be implemented as a single evaluation in which the env-or-option-or-default value, computed once at construction, is passed as the OpenFeature evaluation default:

```go
local := hardcodedDefault
if option != nil {
    local = *option
}
if v, set, err := otelflags.Lookup(moduleEnvVar); err != nil {
    return err                       // see "Environment values are a strict tri-state"
} else if set {
    local = v                        // the environment overrides the option
}
// per operation
effective := resolver.Value(flagKey, local)   // Boolean evaluation with local as the default
```

The `Lookup` error SHALL be returned even when an option was supplied. The option does not excuse an unreadable environment variable, because the variable outranks it and a caller cannot know from the option alone what the deployment meant.

Because the Boolean evaluation returns the supplied default on every path where the relay has no usable answer — no provider installed, provider not ready, key absent, evaluation error, type mismatch — "the relay is silent" and "the relay is unreachable" SHALL both fall through to `local`.

Three switches SHALL exist, with these defaults:

| Switch | Relay key | Option | Environment variable | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| per-module tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` |
| Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

They SHALL compose by conjunction with short-circuit evaluation:

```
tracing     = master && moduleTracing
propagation = tracing && mongoPropagation
```

The master switch SHALL accept no option. Its default of `true` means "express no objection": setting its relay key or environment variable to `true` SHALL have no effect, and setting either to `false` SHALL disable every module in the process regardless of any option or per-module relay value.

Modules SHALL NOT read any environment variable on a hot path. `local` for each switch, and `relayPossible`, SHALL be fixed at construction.

#### Scenario: Relay value wins over every local source
- **WHEN** a module's environment variable is falsy, no option is passed, and the relay resolves that module's key to `true`
- **THEN** the effective module tier is enabled, because the relay is the top rung of the ladder

#### Scenario: Environment variable wins over the option
- **WHEN** a module's environment variable is set truthy and the wrapper is constructed with `WithTracingEnabled(false)`, with no relay opinion
- **THEN** the effective module tier is enabled, because the environment variable outranks the option

#### Scenario: Option wins over the default
- **WHEN** a module's environment variable is unset and the wrapper is constructed with `WithTracingEnabled(true)`, with no relay opinion
- **THEN** the effective module tier is enabled, overriding the hardcoded default of `false`

#### Scenario: Two connections differ when the environment is silent
- **WHEN** a module's environment variable is unset and one wrapper is constructed with `WithTracingEnabled(true)` and another with `WithTracingEnabled(false)`
- **THEN** the first traces and the second does not

#### Scenario: Environment variable wins over the default
- **WHEN** a module's environment variable is set truthy, no option is passed, and no relay opinion exists
- **THEN** the effective module tier is enabled, overriding the hardcoded default of `false`

#### Scenario: Nothing configured resolves to off
- **WHEN** no environment variable is set, no option is passed, and no provider is installed
- **THEN** the master resolves to `true`, every module tier resolves to `false`, and no module traces

#### Scenario: Master veto overrides an enabling relay value
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set falsy and the relay resolves a module's tracing key to `true`
- **THEN** that module emits no spans, because the conjunction short-circuits on the master

#### Scenario: Master veto overrides an enabling option
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set falsy and a wrapper is constructed with `WithTracingEnabled(true)`
- **THEN** that wrapper emits no spans

#### Scenario: otel-mongo resolves propagation from the operation's tracing decision
- **WHEN** `otel-mongo` (v1 or v2) evaluates both its tracing and its propagation decision for the same operation
- **THEN** the propagation decision reuses the operation's already-resolved tracing boolean rather than re-resolving it, and short-circuits to disabled when that boolean is false

#### Scenario: Values are read per decision, not cached per construction
- **WHEN** a relay flag changes after a `Client` or `Conn` has been constructed
- **THEN** the next operation on that wrapper observes the change, without reconstruction, and this holds whether or not the wrapper was constructed with `WithTracingEnabled`

### Requirement: Per-connection override composes above the gates
Each wrapper module SHALL offer a construction-time functional option, `WithTracingEnabled(v bool)`, that supplies the **per-module tracing tier** for that connection or client. `otel-mongo` SHALL additionally offer `WithTracePropagationEnabled(v bool)` for its propagation tier.

The option SHALL sit **below** that tier's environment variable and above the hardcoded default. Supplying both an option and the paired environment variable SHALL NOT be an error; the **environment variable** SHALL win. The option therefore decides only when the deployment left that variable unset.

This ordering SHALL be preserved because it is the operator's per-module control: a deployment SHALL be able to disable one module without silencing the process and without a relay, even when the application's Go code hardcoded that module on. It matters most for `WithTracePropagationEnabled`, which would otherwise let application code append permanent `_oteltrace` fields to documents against an `OTEL_MONGO_PROPAGATION_ENABLED=false` the operator set.

The option SHALL NOT supply the master switch. A per-connection value cannot express a process-wide switch, and keeping the option off the master is what preserves a single environment variable that stops everything.

The option SHALL NOT make a connection static. A connection constructed with `WithTracingEnabled(true)` SHALL still evaluate the master switch and the relay verdict on every operation, and SHALL change behaviour when either changes.

Effective tracing SHALL follow this decision table:

| master | module tier (`relay > env > option > false`) | Effective tracing |
|---|---|---|
| `false` from relay or env | any | off |
| `true` (default, env, or relay) | `false` | off |
| `true` (default, env, or relay) | `true` | on |

and the module tier SHALL be resolved as:

| relay | `OTEL_<MODULE>_TRACING_ENABLED` | `WithTracingEnabled` | module tier |
|---|---|---|---|
| `true` or `false` | any | any | the relay's value |
| silent / unreachable / no provider | set, recognised | any | the variable's value |
| silent / unreachable / no provider | unset | present | the option's value |
| silent / unreachable / no provider | unset | absent | `false` |
| any | set, unrecognised or empty | any | **construction error** |

#### Scenario: Option and environment variable together are legal
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is set to any recognised value and a wrapper is constructed with `WithTracingEnabled(v)`
- **THEN** construction succeeds and the module tier takes the **variable's** value, whatever `v` is

#### Scenario: An operator disables a module the code hardcoded on
- **WHEN** an application constructs every Mongo client with `WithTracingEnabled(true)` and the deployment sets `OTEL_MONGO_TRACING_ENABLED=false`
- **THEN** no Mongo client traces, and the other three modules are unaffected

#### Scenario: An invalid variable fails even when an option is present
- **WHEN** `OTEL_MONGO_TRACING_ENABLED` is set to `enabled` and the wrapper is constructed with `WithTracingEnabled(true)`
- **THEN** construction returns an error wrapping `ErrInvalidFlagValue`, because the option does not excuse an unreadable variable that outranks it

#### Scenario: Option alone enables a module
- **WHEN** no environment variable is set and a wrapper is constructed with `WithTracingEnabled(true)`
- **THEN** the wrapper traces, because the master defaults to `true` and the option supplies the module tier

#### Scenario: Option-carrying connection still observes a relay disable
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is tracing and the relay resolves the module flag to `false`
- **THEN** the next operation emits no spans

#### Scenario: Option-carrying connection can be disabled by the master
- **WHEN** a connection is constructed with `WithTracingEnabled(true)` and the relay resolves `otel-instrumentation-go-tracing` to `false`
- **THEN** that connection emits no spans on its next operation

#### Scenario: Downstream test controls gating without process-global state
- **WHEN** a downstream test suite sets no environment variables, installs no provider, and constructs one connection with `WithTracingEnabled(true)` and one with `WithTracingEnabled(false)`
- **THEN** the first traces and the second does not, with no reset hooks required

### Requirement: Single shared flags module
The shared flag logic SHALL live in one published Go module, `github.com/akira-core/instrumentation-go/otel-flags`, which `otel-mongo`, `otel-mongo/v2`, `otel-nats` and `otel-gorilla-ws` SHALL each `require`. No module SHALL vendor a private copy of it.

The requirement this satisfies is a single OpenFeature provider per binary. Go's minimal version selection resolves one module path to one version per build, so one shared module yields one package instance, one `sync.Once` and therefore exactly one provider install — a guarantee that four independent `internal/` packages sharing no state cannot make.

`otel-flags` SHALL name only **process-scoped** things: the master switch's environment variable and flag key, the three `OTEL_INSTRUMENTATION_GO_FLAGS_*` provider variables, `OTEL_SERVICE_NAME`, and `FlagDomain`. Module flag keys, module environment variable names and module hardcoded defaults SHALL remain in each module's own `env_flags.go` and SHALL reach the shared module only through the `key` and `local` parameters of `Value`.

The four instrumentation modules' `go.mod` files SHALL require a published `otel-flags` version and SHALL NOT carry a `replace` directive, which consumers ignore. A repository-root `go.work` SHALL cover local development, and CI SHALL set `GOWORK=off` for each module's build, test and lint steps so every module is verified exactly as a consumer resolves it.

`otel-flags` SHALL carry a version constant, a `CHANGELOG.md`, a CI matrix entry, and a release-guard tag pattern, on the same terms as every other released module.

#### Scenario: One provider serves every module
- **WHEN** a binary constructs wrappers from all four instrumentation modules with the endpoint variable set and no application-installed provider
- **THEN** exactly one GO Feature Flag provider is registered on `FlagDomain`, and no provider is registered and then replaced

#### Scenario: Module vocabulary stays out of the shared module
- **WHEN** a new instrumentation module is added, or an existing module's flag key or environment variable is renamed
- **THEN** `otel-flags` requires no change

#### Scenario: Published modules resolve without the workspace
- **WHEN** a module's build, test or lint step runs in CI
- **THEN** it runs with `GOWORK=off` and resolves `otel-flags` from its `go.mod` requirement

### Requirement: Environment values are a strict tri-state
`otelflags.Lookup(name)` SHALL return `(value bool, set bool, err error)` with exactly three outcomes:

- **unset** — `set` is `false` and `err` is nil; this source has no opinion and resolution SHALL fall through to the next rung.
- **a recognised value** — the trimmed, lowercased value is one of `1`, `true`, `yes`, `on` (reported as `true`) or `0`, `false`, `no`, `off` (reported as `false`); `set` is `true` and `err` is nil.
- **anything else** — including the empty string, `enabled`, `2`, `y` and `t`; `err` is non-nil and wraps `otelflags.ErrInvalidFlagValue`.

A non-nil error SHALL be returned by the constructor that triggered the read, and no wrapper SHALL be produced. Guessing is prohibited: under a precedence ladder the safe direction is not uniform, because the master tier defaults to `true` and every other tier defaults to `false`, so a value read as `false` on the master would silently stop the whole process.

`flags.EnvEnabled` and `flags.EnvSet` SHALL NOT survive; `Lookup` replaces both.

#### Scenario: Recognised values decide
- **WHEN** a switch is set to `ON`, ` true `, `yes`, `1`, `off`, `FALSE`, `no` or `0`
- **THEN** `Lookup` reports `set` true, the corresponding value, and no error

#### Scenario: Unset falls through
- **WHEN** a switch is not present in the environment
- **THEN** `Lookup` reports `set` false and no error, and resolution continues to the option or the hardcoded default

#### Scenario: Empty string is a construction error
- **WHEN** a switch is exported with an empty value, for example from an unexpanded template variable
- **THEN** construction returns an error wrapping `ErrInvalidFlagValue` and no wrapper is produced

#### Scenario: Unrecognised values are a construction error
- **WHEN** a switch is set to `enabled`, `2`, `y` or `t`
- **THEN** construction returns an error wrapping `ErrInvalidFlagValue` and no wrapper is produced

### Requirement: Named invalid-value error
`otel-flags` SHALL export a single sentinel, `ErrInvalidFlagValue`, which every constructor's returned error SHALL wrap so callers can match it with `errors.Is`. Because `otel-flags` is a published module rather than an `internal/` package, one sentinel SHALL serve all four modules; per-module sentinels SHALL NOT be introduced.

The returned error SHALL name the variable and its observed value, so the fix requires no documentation lookup. It SHALL NOT include the value of `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY`.

A constructor that reads more than one switch SHALL evaluate every read before returning, and SHALL combine the failures with `errors.Join` in a fixed order (master, then module tracing, then propagation), so a caller with several bad values learns all of them at once. It SHALL NOT return on the first failure.

#### Scenario: Caller matches the sentinel
- **WHEN** a constructor rejects an unrecognised environment value
- **THEN** the returned error satisfies `errors.Is(err, otelflags.ErrInvalidFlagValue)`

#### Scenario: Error names the variable and the value
- **WHEN** a constructor rejects an unrecognised environment value
- **THEN** the error message contains the variable's name and the observed value

#### Scenario: Several bad values are reported together
- **WHEN** `ConnectWithOptions` is called with `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=maybe` and `OTEL_MONGO_PROPAGATION_ENABLED=`
- **THEN** the single returned error names both variables and both values
