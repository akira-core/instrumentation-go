## Context

Three instrumentation packages — `otel-mongo` (v1 + v2), `otel-nats`, `otel-gorilla-ws` — each ship their own feature-flag plumbing. `otel-mongo` is the most evolved: three env vars (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, `OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`), `sync.Once`+`atomic.Bool` caching, a `resolveDocumentPropagation` resolver that respects both env and functional-option overrides, and a strategy-split layout (`internal/{direct,traced,shared}`) where the disabled path is compiler-enforced — `internal/direct` packages have zero `go.opentelemetry.io/otel/sdk/*` imports. `otel-nats` and `otel-gorilla-ws` each have only two env vars, no caching, no propagation env separation, and a single runtime `tracingEnabled bool` branch on every public method.

`envEnabledByDefault` is duplicated verbatim in four files (`otel-mongo/otelmongo/env_flags.go`, `otel-mongo/v2/env_flags.go`, `otel-nats/otelnats/env_flags.go`, `otel-gorilla-ws/env_flags.go`). The default-off rule (`os.LookupEnv` returns `false` ⇒ flag disabled) is the same everywhere; the per-module flag composition differs. Each module also has its own version of `mongoTracingEnabled` / `natsTracingEnabled` / `wsTracingEnabled` with identical structure but different env-var names.

`otel-mongo` keeps the three-tier gate because the propagation flag controls **on-disk schema** — `_oteltrace` adds ~100–120 bytes per document and changes what dbwatcher / change-stream consumers see. Decoupling tracing (wrapper spans) from propagation (document field) is a real product requirement, not just plumbing. `otel-nats` and `otel-gorilla-ws` do not have this distinction: NATS-header propagation and WebSocket JSON envelope propagation share a single kill switch with tracing — no consumer wants spans-on / wire-propagation-off for those transports.

Stakeholders: maintainers of all four modules, downstream consumers (the `otel-traces-test` services), and PR #15 reviewers tracking layout consistency (`DIRECTORY_LAYOUT_PLAN.html`). Pre-1.0 modules — minor bump covers layout refactor.

## Goals / Non-Goals

**Goals:**

- Single source of truth for env-var parsing and default-off semantics, reused by all four modules.
- Same **pattern** across all three packages — strategy split via `internal/{direct,traced}/` — so every public method dispatches through an interface and runtime `if tracingEnabled` branches inside public methods disappear.
- Strict disabled-mode invariant enforced by package boundary: when any required flag is off, no `go.opentelemetry.io/otel/sdk/*` or `otel/exporters/*` code can run. Compiler enforces this — not reviewers.
- Process-level caching of resolved flags so hot paths (publish, subscribe loop, change-stream iteration, `ReadMessage`) pay one `os.LookupEnv` per process, not per call.
- Source-compatible public Go API for all three packages. Functional options (`WithTracerProvider`, `WithTracePropagationEnabled`, etc.) keep their current semantics.
- Test-reset hook in the shared helper so `t.Setenv` continues to work without `t.Parallel()`.
- Per-package env-var **surface** stays unchanged: `otel-mongo` keeps three-tier (tracing decoupled from on-disk propagation), `otel-nats` and `otel-gorilla-ws` keep two-tier.

**Non-Goals:**

- Re-designing the underlying wrapper APIs. Method signatures, return types, struct field exports stay.
- Adding a propagation env var to `otel-nats` or `otel-gorilla-ws`. Their tracing flag already covers wire propagation since there is no on-disk distinction.
- Promoting `internal/flags` to a public package. Stays in `internal/` per module so we can change it freely without breaking SemVer.
- Cross-module Go workspace consolidation. Each module keeps its own `go.mod` and release cadence.
- Adding new functional-option overrides on `otel-nats` / `otel-gorilla-ws` (can be added later if asked).
- Runtime flag toggling. Cached value is frozen for the process lifetime — same contract `otel-mongo` already documents.
- Changing how trace context flows on the wire (NATS headers, MongoDB `_oteltrace` field, WebSocket JSON envelope). Propagation mechanics unchanged — only the gate that enables them is unified.

## Decisions

### D1: Duplicated `internal/flags/` package per module, drift-check in CI

Each of the four modules gets its own `internal/flags/` directory with the same generic helper logic (parameterised by per-module env var names supplied at call sites). A new CI step diffs `internal/flags/flags.go` across all four module copies and fails the build on drift.

Rationale: Go's `internal/` rules forbid cross-module sharing without promoting to a public package. The existing `internal/shared/` pattern in `otel-mongo` v1 vs v2 already accepts duplication for the same reason (documented in `CLAUDE.md` and `DIRECTORY_LAYOUT_PLAN.html` §6). Adding a fifth module-shared package (e.g. `pkg/internal-shared`) would pollute SemVer surface and force coordinated bumps. Drift-check CI is the established mitigation.

Alternatives considered:
- *Public `pkg/flags` package* — rejected: forces SemVer commitments on internal plumbing, adds an import for downstream users who never call it.
- *Single Go workspace consolidating modules* — rejected: out of scope; would change release-tag layout and break consumer `replace` directives.
- *`go:generate` + template* — rejected: extra tooling for ~80 LOC of helpers; drift-check catches the same regressions with less machinery.

### D2: Per-package gate surface stays unchanged — Mongo 3-tier, NATS/WS 2-tier

`otel-mongo` keeps `global AND module-tracing AND (option || module-propagation-env)`. `otel-nats` and `otel-gorilla-ws` keep `global AND module-tracing`. The shared `internal/flags/` helper is generic over the number of tiers — callers supply the env-var names; the helper handles parsing, AND-composition, and caching.

Rationale: The on-disk schema implication of `_oteltrace` makes Mongo's propagation a real product knob. NATS headers and WebSocket envelope frames have no equivalent disk footprint — gating wire propagation independently of spans buys nothing and adds an env knob nobody asked for. Uniformity in **pattern** (strategy split) gives the safety; uniformity in **surface** (env-var count) is not the goal — the user requirement is "disabled flag → behaves like original pkg", and that holds for both 2-tier and 3-tier shapes.

Alternatives considered:
- *Force 3-tier on nats/ws* — rejected: adds env vars no consumer asks for, breaks current defaults, increases test matrix.
- *Force 2-tier on mongo* — rejected: would remove a real product knob (on-disk `_oteltrace` toggle) that consumers already use.

### D3: Strategy-split (`internal/{direct,traced}/`) for `otel-nats` and `otel-gorilla-ws`

`otelnats.Conn`, `oteljetstream.Consumer`, `oteljetstream.MessageBatch`, and `otelgorillaws.Conn` migrate from cached-gate runtime branching to the strategy-split layout already used by `otel-mongo` Collection/Cursor/SingleResult/ChangeStream.

Pattern (mirrors existing `otel-mongo`):

```
otel-nats/otelnats/
├── conn.go                 # facade; holds impl connImpl
├── tracing.go env_flags.go # facade helpers; thin wrappers over internal/flags
└── internal/
    ├── shared/  impls.go   # connImpl, subImpl interfaces; tracing helpers
    ├── direct/  conn.go    # zero otel/sdk imports; pure delegation
    └── traced/  conn.go    # full instrumentation, span creation, header inject

otel-gorilla-ws/
├── conn.go                 # facade; holds impl connImpl
├── tracing.go env_flags.go
└── internal/
    ├── shared/  impls.go
    ├── direct/  conn.go    # passthrough ReadMessage/WriteMessage
    └── traced/  conn.go    # JSON envelope + span creation
```

Each facade method becomes `return c.impl.<Method>(args...)`. Constructor picks impl once based on `flags.NATSTracingEnabled()` / `flags.WSTracingEnabled()`. `internal/direct/*.go` imports zero `go.opentelemetry.io/otel/sdk/*`, zero `otel/exporters/*`, zero attribute slices — compile-time guarantee.

Compile-time assertions in the facade enforce interface parity:

```go
var _ connImpl = (*direct.Conn)(nil)
var _ connImpl = (*traced.Conn)(nil)
```

Add new method ⇒ build fails until both impls satisfy.

Rationale: Reviewer-enforced gates regress (see `otel-mongo` commits `dbfda2b`, `7ce92ba` — propagation flag bugs slipped through review). Compiler-enforced gates don't. The `otel-mongo` Collection migration already paid the layout cost and the design now provides a template — copy the structure, swap the upstream types.

`otel-gorilla-ws` has a wrinkle: `Conn.Dial` performs `Sec-WebSocket-Protocol` negotiation at runtime — even with flags on, the peer may not support the OTel subprotocol, in which case the connection runs in passthrough mode. Handle by deferring impl selection until after negotiation: `flags.WSTracingEnabled() && negotiatedOTelSubprotocol` ⇒ `internal/traced.Conn`, else `internal/direct.Conn`. The `tracingEnabled` runtime field disappears; both branches return through the same `connImpl` interface.

Alternatives considered:
- *Keep cached-gate for nats/ws* — rejected: regression risk demonstrated by past `otel-mongo` bugs; user explicitly asked to "use design pattern to avoid many if condition to check the feature flag".
- *Promote `internal/shared` to facade level* — rejected: bigger refactor, no extra safety; the `direct`/`traced` split is what the compiler enforces.

### D4: `sync.Once` + `atomic.Bool` caching with `resetForTest` hook

The shared `internal/flags/` helper exposes:

```go
// Gate caches the result of fn() on first call.
type Gate struct {
    once sync.Once
    flag atomic.Bool
    fn   func() bool
}
func NewGate(fn func() bool) *Gate
func (g *Gate) Enabled() bool
func (g *Gate) ResetForTest()  // build tag: testing-only via internal package isolation
```

Each module composes its own gate at package init:

```go
// otel-nats/otelnats/env_flags.go
var natsGate = flags.NewGate(func() bool {
    return flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED") &&
           flags.EnvEnabled("OTEL_NATS_TRACING_ENABLED")
})
```

Hot paths call `natsGate.Enabled()` (one atomic load after the first resolve). Tests call `natsGate.ResetForTest()` after `t.Setenv`.

Rationale: Matches the existing `otel-mongo` `cachedPropagationEnabled` behaviour, generalises to the other modules' tracing flag too, and removes per-call `os.LookupEnv` overhead from hot paths. The cached value being frozen for process lifetime is already documented and accepted in `otel-mongo`. With the strategy-split layout (D3), `Gate.Enabled()` is called only **once** per wrapper — in the constructor — so even the atomic load disappears from hot paths.

Alternatives considered:
- *No caching* — rejected: hot-path `os.LookupEnv` measurable (~80ns extra per `change-stream Next` in `otel-mongo` pre-cache).
- *`sync.Map`-keyed by env-var name* — rejected: overkill for two flags per module.

### D5: Caller-visible API stays source-compatible; functional options preserved

`WithTracePropagationEnabled` on `otel-mongo` keeps current semantics: only overrides the propagation default when both tracing gates are on. No new functional options on `otel-nats` / `otel-gorilla-ws` in this change. Consumers who need per-connection override use env var only for now.

Rationale: Source compatibility minimises churn for downstream consumers (`otel-traces-test` services). Functional-option API surface is additive — withhold new options until a concrete use case asks for them.

## Risks / Trade-offs

- *Duplicated `internal/flags/` drift across four modules* → CI drift-check diffs the file contents and fails on mismatch. Drift-check follows the existing pattern planned for `internal/shared/`.
- *Subprotocol negotiation in `otel-gorilla-ws` complicates strategy split* → impl selection happens after `Dial` / `Upgrade` completes, not in the constructor. `newConn` becomes a two-step: parse negotiated subprotocol, then return `direct.NewConn` or `traced.NewConn`. Existing scenarios A–E in `conn.go` map cleanly onto the two impls.
- *Migration touches all public method bodies in `otelnats.Conn`, `oteljetstream.Consumer`, `oteljetstream.MessageBatch`, `otelgorillaws.Conn`* → diff is mechanical (move body to `traced.X`, write passthrough in `direct.X`) but large. Mitigate with module-at-a-time landing — `otel-mongo` first as smoke test (only flags package wiring), `otel-nats` second, `otel-gorilla-ws` third. Each module ships independently.
- *`sync.Once` cache frozen for process lifetime* → documented behaviour, matches `otel-mongo` today. Tests use `ResetForTest`. Production consumers don't toggle flags mid-process.
- *Asymmetric env-var surface (3-tier for mongo, 2-tier for nats/ws)* → could confuse new consumers. Mitigate with a single env-var table in `pkg/instrumentation-go/CLAUDE.md` and per-module README updates.
- *Four module bumps land in the same window* → release engineering writes four tags. Existing release script in `pkg/instrumentation-go` already supports multi-tag pushes — see commit `a599d87`.

### D6: Directory layout aligned with Go community convention

All four modules adopt the same canonical tree, aligned with `golang-standards/project-layout` and trending Go community practice:

```
<module>/
├── go.mod / go.sum / doc.go / version.go / README*.md
├── <facade>.go (collection.go / conn.go / etc.)
├── tracing.go / env_flags.go / options.go
├── internal/
│   ├── flags/    # shared gate helpers (byte-identical across modules)
│   ├── shared/   # interfaces + helpers used by both impls
│   ├── direct/   # disabled-mode impls — zero otel/sdk imports
│   └── traced/   # enabled-mode impls — full instrumentation
├── examples/<demo>/  # runnable demos, plural per Go convention, each its own module
└── tests/integration/  # testcontainers-based tests, separate module
```

Key conventions:

- `examples/` plural (matches `kubernetes/examples`, `grpc-go/examples`, etc.). Current singular `example/` directories get renamed.
- `internal/` exclusively holds packages not importable downstream — Go compiler enforces this.
- Integration tests live under `tests/integration/` as a separate Go submodule so testcontainers does not leak into the module's dependency closure.
- Module root contains only files defining exported identifiers — purely-internal helpers move under `internal/`.
- Subpackage names are fixed (`flags`, `shared`, `direct`, `traced`) — no synonyms like `gate`, `common`, `disabled`, `instrumented`.
- Each module's README follows the same section order so cross-module scanning is uniform.
- `pkg/instrumentation-go/CLAUDE.md` carries the single source-of-truth "Module Layout" section; per-module READMEs reference it.

Rationale: A new contributor opens any of the four modules and finds the same categories in the same places. Reduces onboarding friction and review load. Aligns with widely-recognised Go community guides so the layout is immediately legible to anyone who has worked on a typical Go module.

The layout change SHALL ship as a separate commit (or commit series) from the feature-flag logic change so `git log --follow` and `git diff -M` cleanly show the rename. Reviewers should see two distinct kinds of diff: (a) the mechanical rename, (b) the logic change.

Alternatives considered:
- *Keep current `example/` (singular)* — rejected: out of step with widely-used Go projects (kubernetes, grpc-go, helm, cobra). Renaming is a one-time cost.
- *Flat module root with no `internal/shared`* — rejected: defeats the strategy-split pattern's compile-time guarantees.
- *Promote `internal/flags` to `pkg/flags`* — already rejected in D1.

## Migration Plan

1. Land `internal/flags/` package in `otel-mongo` first (lowest risk — only consumer is existing `env_flags.go` callers; replace internal API). Verify `go build`, `go test -race`, `golangci-lint` for both v1 and v2. The Collection/Cursor strategy split is already in place; this step only swaps the flag helper.
2. Land `internal/flags/` copy in `otel-nats`. Migrate `otelnats.Conn` and `oteljetstream.Consumer` / `MessageBatch` to `internal/{direct,traced}/` strategy split. Existing two-tier gate (`global + OTEL_NATS_TRACING_ENABLED`) wired through the shared helper. Run module tests + integration tests (testcontainers NATS).
3. Same for `otel-gorilla-ws`. Pay attention to subprotocol negotiation — impl selection deferred past `Dial` / `Upgrade`. Run module tests + the `otel-traces-test` WebSocket integration verify (`make verify-ws-trace`).
4. Add CI drift-check job: `diff` across the four `internal/flags/flags.go` copies. Wire into existing `.github/workflows/ci.yml` matrix.
5. Update READMEs (EN + zh-TW) for `otel-mongo` / `otel-nats` / `otel-gorilla-ws`, and root `CLAUDE.md` + `pkg/instrumentation-go/CLAUDE.md`. Document the unified strategy-split pattern and the per-package env-var surface.
6. Bump versions: `otel-mongo` → 0.4.0, `otel-mongo/v2` → 0.4.0, `otel-nats` → 0.4.0, `otel-gorilla-ws` → 0.4.0. Tag separately. Update consumer `go.mod` files in `otel-traces-test` services and re-run `make verify-trace`.
7. **Rollback strategy**: each module ships independently; if one regresses, revert that module's tag and pin the consumer back to the previous version. The `internal/flags/` package is purely internal — no consumer API surface impacted, so revert is local.
8. **Layout rename commits** ship before logic commits per module: `chore(<module>): rename example/ → examples/` and `chore(<module>): introduce internal/{flags,shared,direct,traced}` land first so `git diff -M` cleanly shows file moves; the subsequent logic commit only contains real code change.

## Open Questions

- *Should the global env var be cached separately or always re-read?* The cached approach loses one signal: an ops team disabling the global flag mid-run won't take effect. Acceptable for now (matches `otel-mongo` today), revisit if production ops asks for runtime kill switch.
- *Drift-check: byte-level diff or AST-level?* Byte-level is simpler but fragile (whitespace, import order). AST-level catches semantic drift but needs go/parser plumbing. Pragmatic choice: byte-level on the helper file, exclude module-specific call sites via separate file (`gate.go` shared, `env_flags.go` per-module). Decide during implementation.
- *`otel-mongo` `Client` and `Database` are still cached-gate.* Should they migrate to strategy-split in this change or follow up? User requirement is satisfied with current cached-gate behaviour, but for full pattern uniformity they should also migrate. Recommend: include in this change scope since the layout template is already in place — adding two more facades is incremental work.
