# shared-feature-flags Specification

## Purpose
Shared feature-flag primitives used by all instrumentation modules: environment
reads, OpenFeature-backed `Resolver` snapshots, and per-connection overrides.

## Requirements
### Requirement: Byte-identical vendoring across modules
Every module (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) SHALL vendor its own copy of the `internal/flags` package (`flags.go` + `flags_test.go`), and the file contents excluding the `package` declaration line SHALL remain byte-identical across all four copies. This rule SHALL cover the `Resolver` snapshot and refresh logic added for dynamic flag resolution, which is the shared logic most at risk of silent divergence.

#### Scenario: A module's flags.go is modified
- **WHEN** a change modifies the body of `flags.go` in one module's `internal/flags/` copy
- **THEN** the same change SHALL be applied to the other three modules' `internal/flags/` copies to preserve byte-identical content

#### Scenario: Resolver logic is modified
- **WHEN** the TTL comparison, snapshot construction, or environment-default fallback in one copy's `Resolver` is changed
- **THEN** the identical change SHALL be applied to the other three copies

### Requirement: Composed per-module gates
Each module SHALL construct exactly one package-level `flags.Resolver` at package initialization, supplying one `flags.Spec` per dynamic flag it owns, and SHALL read it through `Resolver.Enabled(i)` at each decision point rather than caching the result on a wrapper struct. `otel-nats` and `otel-gorilla-ws` SHALL each own a single tracing `Spec`; `otel-mongo` (v1 and v2) SHALL own a tracing `Spec` and a propagation `Spec` so both resolve from one snapshot store. The global switch `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` SHALL be read via `flags.GlobalTracingPossible()` (or equivalent `EnvEnabled` of that name) and ANDed ahead of the resolver read, never expressed as a `Spec`.

#### Scenario: otel-nats / otel-gorilla-ws compose one tracing spec
- **WHEN** `otelnats` or `otel-gorilla-ws` resolves its effective tracing state for a connection with no `WithTracingEnabled` option
- **THEN** it returns `flags.GlobalTracingPossible() && resolver.Enabled(tracingIndex)` (equivalently `EnvEnabled` of the global kill-switch name)

#### Scenario: otel-mongo composes tracing and propagation in one resolver
- **WHEN** `otel-mongo` (v1 or v2) evaluates both its tracing and its propagation decision for the same operation
- **THEN** the propagation decision reuses the operation's already-resolved tracing boolean (or both values from one snapshot load), so a single operation does not combine tracing from snapshot T0 with a post-TTL refresh for the same tracing index

#### Scenario: Values are read per decision, not cached per construction
- **WHEN** a relay flag changes after a `Client` or `Conn` has been constructed without `WithTracingEnabled`
- **THEN** the next operation on that wrapper observes the new value within the resolver's TTL, without reconstruction

### Requirement: Per-connection override composes above the gates
Each wrapper module SHALL offer a construction-time functional option, `WithTracingEnabled(v bool)`, that overrides the flag resolution for that connection/client only. When the option is present its value SHALL be authoritative, SHALL be resolved once at construction, and SHALL suppress all OpenFeature evaluation for that connection — the connection is fully static and no relay change can affect it. When the option is absent, the global environment switch SHALL be ANDed with the module's dynamic resolver value, re-read per operation. The option's presence SHALL also decide which implementation a strategy-split wrapper is constructed with, so that `WithTracingEnabled(true)` still produces spans when the global environment switch is off.

Effective tracing SHALL follow this decision table (`Env` = `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`; `Flag` = the module's dynamic value, itself defaulting to the module environment variable):

| Env | `Flag` | `WithTracingEnabled` | Effective tracing |
|-----|--------|----------------------|-------------------|
| off (unset or falsy) | any | absent | off — no evaluation performed |
| off (unset or falsy) | any | `true` | on |
| off (unset or falsy) | any | `false` | off |
| on | on | absent | on |
| on | off | absent | off |
| on | any | `false` | off |
| on | any | `true` | on |

#### Scenario: Option absent defers to the global switch and the dynamic flag
- **WHEN** a wrapper is constructed without `WithTracingEnabled`
- **THEN** its tracing decision is `EnvEnabled(GLOBAL) && resolver.Enabled(tracing)`, re-read per operation

#### Scenario: Option true enables tracing despite env off
- **WHEN** a wrapper is constructed with `WithTracingEnabled(true)` while the global env switch is unset or explicitly falsy
- **THEN** the wrapper is constructed on the instrumented path, produces spans, and performs no OpenFeature evaluation

#### Scenario: Option false disables tracing despite env on and flag on
- **WHEN** the global env switch is truthy, the relay resolves the module flag to `true`, and the caller passes `WithTracingEnabled(false)`
- **THEN** tracing is disabled for that connection/client and remains disabled for its lifetime

#### Scenario: Option true with env already on stays on
- **WHEN** the global env switch is truthy and the caller also passes `WithTracingEnabled(true)`
- **THEN** tracing remains enabled for that connection/client and no relay change can disable it

#### Scenario: Downstream test controls gating without process-global state
- **WHEN** a downstream test suite constructs one traced and one untraced connection in the same process by passing the option
- **THEN** both behave per their option values with no environment manipulation, no OpenFeature provider, and no reset hooks required

#### Scenario: Option makes a connection immune to relay changes
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is in use and the relay flips the module flag to `false`
- **THEN** that connection continues to trace, while connections constructed without the option stop tracing within the TTL

## REMOVED Requirements

### Requirement: Cached gate resolution
**Reason**: `flags.Gate` / `NewGate` / `ResetForTest` were removed; process-lifetime
caching is incompatible with dynamic OpenFeature resolution. See the change
`openfeature-dynamic-flags` for migration notes.

### Requirement: Test-only cache reset
**Reason**: Replaced by `Resolver` + `WithClock` for TTL-boundary tests.
