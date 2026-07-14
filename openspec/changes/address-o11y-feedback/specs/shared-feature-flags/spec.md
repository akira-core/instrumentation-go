# shared-feature-flags Delta Specification

## ADDED Requirements

### Requirement: Per-connection override composes above the gates
Each wrapper module SHALL offer a construction-time functional option, `WithTracingEnabled(v bool)`, that overrides the env-gate default for that connection/client only. The override SHALL compose **above** the `internal/flags` primitives at the wrapper layer: when the option is present its value is authoritative (overriding both the global and module env gates in either direction — including when the env vars are explicitly falsy, not merely unset); when absent, the existing gate resolution applies unchanged. The `internal/flags` package itself (`EnvEnabled`, `Gate`) SHALL NOT change for this feature, no new exported test-reset hooks SHALL be added, and the byte-identical vendoring rule continues to apply to the unchanged `flags` copies. Resolution SHALL happen once at construction, feeding the same cached per-wrapper `tracingEnabled` decision (or strategy-split impl selection) the modules already use, so the disabled-mode invariant is inherited without new per-method checks.

Effective tracing SHALL follow this decision table (`Env` = `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND the module switch; unset or falsy → off):

| Env | `WithTracingEnabled` | Effective tracing |
|-----|----------------------|-------------------|
| off (unset or falsy) | absent | off |
| off (unset or falsy) | `true` | on |
| off (unset or falsy) | `false` | off |
| on | absent | on |
| on | `false` | off |
| on | `true` | on |

#### Scenario: Option absent preserves gate behavior bit-for-bit
- **WHEN** a wrapper is constructed without `WithTracingEnabled`
- **THEN** its tracing decision comes from the existing gate resolution (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND the module switch), identical to behavior before this option existed

#### Scenario: Option true enables tracing despite env off
- **WHEN** a wrapper is constructed with `WithTracingEnabled(true)` while both env gates are unset or explicitly falsy
- **THEN** the option's value decides tracing for that connection/client; other connections in the same process still follow the env gates

#### Scenario: Option false disables tracing despite env on
- **WHEN** a wrapper is constructed with `WithTracingEnabled(false)` while both env gates are truthy
- **THEN** tracing is disabled for that connection/client

#### Scenario: Option true with env already on stays on
- **WHEN** both env gates are truthy and the caller also passes `WithTracingEnabled(true)`
- **THEN** tracing remains enabled for that connection/client

#### Scenario: Downstream test controls gating without process-global state
- **WHEN** a downstream test suite constructs one traced and one untraced connection in the same process by passing the option
- **THEN** both behave per their option values with no environment manipulation, no `TestMain` env setup, and no reset hooks required

#### Scenario: flags package remains untouched
- **WHEN** the option feature is implemented across the four modules
- **THEN** every module's `internal/flags/flags.go` body is unchanged (still byte-identical across copies), and the override logic lives entirely in each module's wrapper-layer construction code
