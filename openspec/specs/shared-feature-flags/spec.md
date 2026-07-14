# shared-feature-flags Specification

## Purpose
TBD - created by archiving change document-otel-instrumentation. Update Purpose after archive.
## Requirements
### Requirement: Default-off environment variable reading
`flags.EnvEnabled(name string) bool` SHALL return `false` when the named environment variable is unset. When set, it SHALL return `false` for the case-insensitive, whitespace-trimmed values `0`, `false`, `no`, `off`, and `true` for any other set value.

#### Scenario: Unset variable
- **WHEN** the named environment variable has never been set
- **THEN** `EnvEnabled` returns `false`

#### Scenario: Falsy value
- **WHEN** the variable is set to `"False"` (mixed case) or `" off "` (with whitespace)
- **THEN** `EnvEnabled` returns `false`

#### Scenario: Truthy value
- **WHEN** the variable is set to any value other than `0`/`false`/`no`/`off` (e.g. `"1"`, `"yes"`, or an arbitrary non-empty string)
- **THEN** `EnvEnabled` returns `true`

#### Scenario: Empty-string value is truthy
- **WHEN** the variable is explicitly set to the empty string (e.g. via `t.Setenv(name, "")`)
- **THEN** `EnvEnabled` returns `true` — being *set* to empty is distinct from being *unset*, and only the four named falsy strings disable

### Requirement: Cached gate resolution
`flags.Gate` (constructed via `NewGate(fn func() bool)`) SHALL invoke its resolver function at most once per process lifetime, using `sync.Once` and `atomic.Bool`, and `Enabled()` SHALL return the cached boolean on every call after the first.

#### Scenario: First call evaluates the resolver
- **WHEN** `Enabled()` is called on a freshly constructed `Gate` for the first time
- **THEN** the resolver function `fn` is invoked exactly once and its result is cached

#### Scenario: Subsequent calls use the cache
- **WHEN** `Enabled()` is called again after the first call, even if the underlying environment variables have changed
- **THEN** the previously cached value is returned and `fn` is not re-invoked

### Requirement: Test-only cache reset
`Gate.ResetForTest()` SHALL clear the cached `sync.Once` and reset the cached boolean to `false`, so the next `Enabled()` call re-invokes the resolver. This method SHALL be documented as not parallel-safe and reserved for tests that toggle environment variables via `t.Setenv`.

#### Scenario: Test toggles an env var
- **WHEN** a test calls `t.Setenv` to change a flag's underlying env var and then calls `gate.ResetForTest()`
- **THEN** the next `gate.Enabled()` call re-evaluates the resolver against the new environment value

#### Scenario: Not called in production paths
- **WHEN** `ResetForTest` is invoked outside of a test context or concurrently with other goroutines calling `Enabled()`
- **THEN** behavior is undefined — the method is documented as test-only and not parallel-safe, and production code paths SHALL NOT call it

### Requirement: Byte-identical vendoring across modules
Every module (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) SHALL vendor its own copy of the `internal/flags` package (`flags.go` + `flags_test.go`), and the file contents excluding the `package` declaration line SHALL remain byte-identical across all four copies.

#### Scenario: A module's flags.go is modified
- **WHEN** a change modifies the body of `flags.go` in one module's `internal/flags/` copy
- **THEN** the same change SHALL be applied to the other three modules' `internal/flags/` copies to preserve byte-identical content

### Requirement: Composed per-module gates
`otel-nats` and `otel-gorilla-ws` SHALL each compose a package-level `Gate` (`natsGate`, `wsGate`) via `NewGate` with a resolver that logically ANDs the global master switch and the module-specific switch, and SHALL call that `Gate.Enabled()` once per wrapper construction rather than per method call. `otel-mongo` (v1 and v2) SHALL use a different composition: its primary tracing decision (`mongoTracingEnabled()`, ANDing the global and `OTEL_MONGO_TRACING_ENABLED` switches) is a plain, uncached function — **not** wrapped in a `Gate` — called directly at each `Client`/`Collection` construction site; only its propagation decision is `Gate`-wrapped (`propEnabledGate`, wrapping `mongoPropagationEnabled`), and that `Gate` is invoked per-call from the package-level `ContextFromDocument`/`ContextFromRawDocument` functions (e.g. once per change-stream document decode), not once per wrapper construction.

#### Scenario: otel-nats / otel-gorilla-ws — global and module flags combined via a Gate
- **WHEN** `otelnats` or `otel-gorilla-ws` constructs its tracing gate as `NewGate(func() bool { return EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED") && EnvEnabled("OTEL_<MODULE>_TRACING_ENABLED") })`
- **THEN** `Enabled()` returns `true` only when both environment variables resolve to truthy, and the resolver runs at most once for that `Gate`'s lifetime

#### Scenario: otel-mongo — tracing decision is not Gate-wrapped
- **WHEN** `otel-mongo` (v1 or v2) evaluates whether wrapper CLIENT spans are enabled
- **THEN** it calls the plain function `mongoTracingEnabled()` directly (re-evaluating `EnvEnabled` on every call site), rather than reading a cached `Gate.Enabled()` value

#### Scenario: otel-mongo — propagation Gate is read per document, not per construction
- **WHEN** `ContextFromDocument` or `ContextFromRawDocument` is called on a decoded document
- **THEN** it reads `propEnabledGate.Enabled()` at that call site (the resolver itself still runs only once, per `Gate` semantics, but the read happens on every document decode rather than once at `Client`/`Collection` construction)

### Requirement: Per-connection override composes above the gates
Each wrapper module SHALL offer a construction-time functional option, `WithTracingEnabled(v bool)`, that overrides the env-gate default for that connection/client only. The override SHALL compose **above** the `internal/flags` primitives at the wrapper layer: when the option is present its value is authoritative (overriding both the global and module env gates in either direction — including when the env vars are explicitly falsy, not merely unset); when absent, the existing gate resolution applies unchanged. The `internal/flags` package itself (`EnvEnabled`, `Gate`) SHALL NOT change for this feature. Resolution SHALL happen once at construction.

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
- **THEN** tracing is enabled for that connection/client

#### Scenario: Option false disables tracing despite env on
- **WHEN** a wrapper is constructed with `WithTracingEnabled(false)` while both env gates are truthy
- **THEN** tracing is disabled for that connection/client

#### Scenario: Option true with env already on stays on
- **WHEN** both env gates are truthy and the caller also passes `WithTracingEnabled(true)`
- **THEN** tracing remains enabled for that connection/client

#### Scenario: Downstream test controls gating without process-global state
- **WHEN** a downstream test suite constructs one traced and one untraced connection in the same process by passing the option
- **THEN** both behave per their option values with no environment manipulation required

