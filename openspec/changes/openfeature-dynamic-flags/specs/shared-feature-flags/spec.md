## MODIFIED Requirements

### Requirement: Byte-identical vendoring across modules
Every module (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) SHALL vendor its own copy of the `internal/flags` package (`flags.go` + `flags_test.go`), and the file contents excluding the `package` declaration line SHALL remain byte-identical across all four copies. This rule SHALL cover the `Resolver` evaluation call and the truthiness rules added for dynamic flag resolution, which is the shared logic most at risk of silent divergence — and would cover any caching added to it later.

#### Scenario: A module's flags.go is modified
- **WHEN** a change modifies the body of `flags.go` in one module's `internal/flags/` copy
- **THEN** the same change SHALL be applied to the other three modules' `internal/flags/` copies to preserve byte-identical content

#### Scenario: Resolver logic is modified
- **WHEN** the evaluation call, the out-of-range behaviour, or the truthiness allow-list in one copy is changed
- **THEN** the identical change SHALL be applied to the other three copies

### Requirement: Composed per-module gates
Each module SHALL construct exactly one package-level `flags.Resolver` at package initialization, supplying one OpenFeature flag key per dynamic flag it owns via `flags.WithFlagKeys`, and SHALL read it at each decision point rather than caching the result on a wrapper struct. `otel-nats` and `otel-gorilla-ws` SHALL each own a single tracing key; `otel-mongo` (v1 and v2) SHALL own a tracing key and a propagation key.

Composition SHALL be a conjunction of three tiers, in this order, with short-circuit evaluation:

```
effective := gate1 && flags.EnvEnabled(moduleEnvVar) && resolver.Allowed(keyIndex)
```

where `gate1` is the first-tier switch defined below. The relay verdict SHALL be the last term, SHALL be resolved with an evaluation default of `true`, and SHALL therefore only ever subtract. Neither `gate1` nor the module environment variable SHALL be passed to the resolver in any form; the module ANDs them itself. When either of the first two terms is false the resolver SHALL NOT be consulted.

`gate1` SHALL be read via `flags.GlobalTracingPossible()` when no per-connection option is present, and its presence tested via `flags.GlobalTracingSet()` for the mutual-exclusion check.

#### Scenario: otel-nats / otel-gorilla-ws compose one tracing key
- **WHEN** `otelnats` or `otel-gorilla-ws` resolves its effective tracing state for a connection with no `WithTracingEnabled` option
- **THEN** it returns `flags.GlobalTracingPossible() && flags.EnvEnabled(moduleEnvVar) && resolver.Allowed(tracingIndex)`

#### Scenario: Module environment variable off skips the relay entirely
- **WHEN** `gate1` is enabled but the module's environment variable is unset or falsy
- **THEN** the effective state is disabled and no `Client.Boolean` call is made for that module

#### Scenario: otel-mongo resolves propagation from the operation's tracing decision
- **WHEN** `otel-mongo` (v1 or v2) evaluates both its tracing and its propagation decision for the same operation
- **THEN** the propagation decision reuses the operation's already-resolved tracing boolean rather than re-resolving it, and short-circuits to disabled when that boolean is false

#### Scenario: Values are read per decision, not cached per construction
- **WHEN** a relay flag is revoked after a `Client` or `Conn` has been constructed
- **THEN** the next operation on that wrapper observes the revocation, without reconstruction, and this holds whether or not the wrapper was constructed with `WithTracingEnabled`

### Requirement: Per-connection option and environment switch are mutually exclusive
Each wrapper module SHALL offer a construction-time functional option, `WithTracingEnabled(v bool)`, that supplies the first-tier switch (`gate1`) for that connection or client. The option and the environment variable `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` SHALL be two spellings of the same tier and SHALL NOT both be supplied: when the environment variable is **present** — tested by `os.LookupEnv`, irrespective of its value — and the option is also passed, construction SHALL return an error and SHALL NOT produce a connection or client. Presence, not disagreement, is the trigger.

When exactly one is supplied, it SHALL decide `gate1`. When neither is supplied, `gate1` SHALL be disabled. Together with the module environment variable — the other term that cannot change after construction — `gate1` SHALL also decide which implementation a strategy-split wrapper is allocated with.

The option SHALL NOT make a connection static. A connection constructed with `WithTracingEnabled(true)` SHALL still evaluate the module environment variable and the relay verdict on every operation, and SHALL stop tracing when the relay revokes.

Effective tracing SHALL follow this decision table:

| `gate1` | `OTEL_<MODULE>_TRACING_ENABLED` | relay verdict | Effective tracing |
|---|---|---|---|
| disabled | any | any | off — no evaluation performed, passthrough implementation only |
| enabled | unset or falsy | any | off — no evaluation performed |
| enabled | truthy | `false` | off |
| enabled | truthy | `true`, absent, or unresolvable | on |

and `gate1` SHALL be resolved as:

| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `WithTracingEnabled` | `gate1` |
|---|---|---|
| unset | absent | disabled |
| unset | `true` | enabled |
| unset | `false` | disabled |
| set, truthy | absent | enabled |
| set, falsy | absent | disabled |
| set, any value | present, any value | **construction error** |

#### Scenario: Both spellings supplied is a construction error
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set to any value and a wrapper is constructed with `WithTracingEnabled(v)` for either value of `v`
- **THEN** construction returns an error naming both settings and their observed values, and no wrapper is produced

#### Scenario: Option alone enables the first tier
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset and a wrapper is constructed with `WithTracingEnabled(true)`
- **THEN** the wrapper is constructed on the instrumented path, and the module environment variable and relay verdict decide per operation

#### Scenario: Option-carrying connection still obeys a revocation
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is tracing under a truthy module environment variable and the relay resolves the module flag to `false`
- **THEN** the next operation emits no spans, exactly as it would on a connection constructed from the environment variable

#### Scenario: Option-carrying connection cannot be enabled by the relay
- **WHEN** a connection is constructed with `WithTracingEnabled(true)` while the module environment variable is unset, and the relay resolves the module flag to `true`
- **THEN** that connection emits no spans and performs no evaluation

#### Scenario: Downstream test controls gating without process-global state
- **WHEN** a downstream test suite leaves `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` unset, sets the module environment variable truthy, installs no provider, and constructs one connection with `WithTracingEnabled(true)` and one with `WithTracingEnabled(false)`
- **THEN** the first traces and the second does not, with no reset hooks required

## ADDED Requirements

### Requirement: Environment truthiness allow-list and presence predicate
`flags.EnvEnabled(name)` SHALL treat a variable as enabled only when it is set and its trimmed, lowercased value is one of `1`, `true`, `yes`, `on`. Every other set value — including the empty string, `0`, `false`, `no`, `off`, and any unrecognised word such as `enabled` or `2` — and an unset variable SHALL be treated as disabled.

`flags.EnvSet(name)` SHALL report only whether the variable is present, via `os.LookupEnv`. It SHALL be used solely for the mutual-exclusion check and SHALL NOT be used to decide whether a switch is enabled.

A set value belonging to neither the truthy nor the falsy list SHALL additionally emit one `slog.Warn` before returning disabled, so the reversal below announces itself rather than presenting later as "spans disappeared after upgrading". Unset, truthy and explicitly falsy values SHALL stay silent. The full contract, including the decision not to deduplicate the warning, is in the `dynamic-feature-flags` capability.

#### Scenario: Allow-list membership decides
- **WHEN** a switch is set to `ON`, ` true `, `yes`, or `1`
- **THEN** `EnvEnabled` reports enabled and emits no warning

#### Scenario: Empty string no longer enables
- **WHEN** a switch is exported with an empty value
- **THEN** `EnvEnabled` reports disabled, reversing the behavior of the preceding release

#### Scenario: Unrecognised values no longer enable, and warn
- **WHEN** a switch is set to `enabled`, `2`, `y`, or `t`
- **THEN** `EnvEnabled` reports disabled, reversing the behavior of the preceding release, and emits a warning naming the variable, the value and the accepted values

### Requirement: Named configuration-conflict errors
Each wrapper module SHALL export a sentinel error value for the mutual-exclusion violation, wrapped by the error its constructors return, so callers can match it with `errors.Is`. `otel-mongo` (v1 and v2) SHALL export a second sentinel for the `WithTracePropagationEnabled` / `OTEL_MONGO_PROPAGATION_ENABLED` pair. Because `internal/flags` is not importable by consumers, the sentinels SHALL live in each module's own package; only the `EnvSet` predicate is shared.

The returned error SHALL name both observed values, since a conflict is only actionable once the reader knows which two settings disagree.

A constructor with more than one such check — only `otel-mongo` has one — SHALL evaluate every check before returning, and SHALL combine the failures with `errors.Join` in a fixed order (tracing first, propagation second), so a caller violating both rules learns both at once. It SHALL NOT return on the first failure.

#### Scenario: Caller matches the sentinel
- **WHEN** a constructor rejects a conflicting configuration
- **THEN** the returned error satisfies `errors.Is(err, otelXxx.ErrTracingConfigConflict)`

#### Scenario: Error names both values
- **WHEN** a constructor rejects a conflicting configuration
- **THEN** the error message contains both the option's value and the environment variable's observed value

#### Scenario: Two conflicts are reported together
- **WHEN** `ConnectWithOptions` is called with both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_MONGO_PROPAGATION_ENABLED` set and both `WithTracingEnabled` and `WithTracePropagationEnabled` passed
- **THEN** the single returned error satisfies `errors.Is` for both sentinels, and its message names all four values

## REMOVED Requirements

### Requirement: Cached gate resolution
**Reason**: `flags.Gate` cached a resolver's result for the entire process lifetime, which is incompatible with revoking a flag at runtime. Its three call sites (`natsGate`, `wsGate`, `propEnabledGate`) are replaced by `flags.Resolver`, which resolves on every call. `Gate` and `NewGate` are deleted rather than left as dead code.

**Migration**: None required for consumers — `internal/flags` is not importable outside this repository. Within the repository, replace `flags.NewGate(fn)` with `flags.NewResolver(flags.WithFlagKeys(...))` — no domain argument; the OpenFeature domain is a process-scoped constant in the shared file — and `gate.Enabled()` with the three-tier conjunction defined in *Composed per-module gates*. See the `dynamic-feature-flags` capability for the replacement contract.

### Requirement: Test-only cache reset
**Reason**: `Gate.ResetForTest` is removed with `Gate`. Nothing replaces it: because `Resolver` caches nothing, tests change a value on the installed in-memory provider and the next operation observes it. The provider is the control surface, so tests drive the real code path instead of bypassing it.

**Migration**: None required for consumers. Within the repository, replace `gate.ResetForTest()` in tests with a mutation of the installed `memprovider.NewInMemoryProvider`. The prohibition on `t.Parallel` for tests that touch process-global state still applies.
