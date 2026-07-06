## Why

Feature-flag handling drifts across the three instrumentation packages. `otel-mongo` enforces a three-tier gate (global + module tracing + module propagation), caches the resolved flag with `sync.Once`/`atomic.Bool`, and uses a strategy-split layout (`internal/direct` vs `internal/traced`) that makes the disabled path compiler-enforced. `otel-nats` and `otel-gorilla-ws` each define a two-tier gate (global + module tracing), duplicate the same `envEnabledByDefault` helper, read env on every call, and rely on a single runtime `tracingEnabled bool` cached on the wrapper that branches inside every public method. The differences make the disabled-mode contract hard to reason about and let regressions slip in when only one package is updated.

The user-facing requirement is simple: when feature flags are off, every wrapper must behave exactly like the unwrapped upstream client — no extra spans, no extra goroutines, no OTel SDK code paths, no helper allocations. Achieve this by adopting the same design pattern (strategy split via `internal/{direct,traced}`) across all three packages so the disabled path is compiler-enforced, runtime `if tracingEnabled` branches disappear from public methods, and env-flag plumbing is deduplicated.

## What Changes

- Introduce a shared, internal feature-flag helper package consumed by `otel-mongo` (v1+v2), `otel-nats`, and `otel-gorilla-ws`. Deduplicates `envEnabledByDefault`, gate resolution, and `sync.Once`/`atomic.Bool` caching. Per-module env var names are passed in by the caller.
- Keep the gate **surface** different per package — only `otel-mongo` keeps the three-tier gate (global + module tracing + module propagation). `otel-nats` and `otel-gorilla-ws` keep their existing two-tier gate (global + module tracing). The propagation flag is a Mongo-specific concern (`_oteltrace` document field affects on-disk schema and storage) — NATS header propagation and WebSocket JSON envelope propagation share a single kill switch with tracing.
- Standardise the **pattern** across all three packages: replace cached-gate runtime `if tracingEnabled` branches inside every public method with the strategy-split layout already proven in `otel-mongo` Collection/Cursor/SingleResult/ChangeStream. The facade type holds an `impl` interface; the constructor picks `internal/direct.X` (zero OTel SDK imports, compiler-enforced isolation) or `internal/traced.X` (instrumented) **once** and dispatches via interface method calls thereafter. Caller-visible API surface unchanged.
- Guarantee the disabled-mode invariant uniformly: when any required flag is off, **no** OTel SDK code runs (no `tracer.Start` on a real tracer, no propagator inject/extract, no attribute slice build, no deliver-span goroutine, no `_oteltrace` field injection, no JSON envelope encode for `otel-gorilla-ws`). Wrappers delegate straight to the upstream call with the original signature semantics preserved.
- Cache the resolved flag once per process across all three packages so hot paths (publish, subscribe loop, change-stream iteration, `ReadMessage`) pay zero `os.LookupEnv` overhead. Provide a `resetForTest` hook in the shared helper to keep `t.Setenv`-style tests working.
- Update READMEs (`otel-mongo`, `otel-nats`, `otel-gorilla-ws`) and `CLAUDE.md` to document the unified pattern, the disabled-mode contract, and the env-var surface (3-tier for mongo, 2-tier for nats/ws).

## Capabilities

### New Capabilities

- `instrumentation-feature-flags`: Cross-module contract for OpenTelemetry feature-flag resolution and the disabled-mode invariant. Defines env-var naming convention, gate-resolution semantics, process-level caching rules, test-reset hook, and the rule that disabled mode must run zero OTel SDK code paths via strategy-split package boundary.
- `otel-mongo-flag-wiring`: Wiring of the shared feature-flag helper into `otel-mongo` v1 and v2 — replaces the in-package `env_flags.go` while preserving the existing three-tier gate (global + tracing + propagation), strategy-split impl selection (`internal/direct` vs `internal/traced`), and `_oteltrace` propagation behaviour.
- `otel-nats-flag-wiring`: Wiring of the shared helper into `otel-nats`. Keeps the existing two-tier gate (global + `OTEL_NATS_TRACING_ENABLED`). Migrates `otelnats.Conn` and `oteljetstream.Consumer` / `MessageBatch` from cached-gate `tracingEnabled bool` to strategy-split `internal/direct` vs `internal/traced` packages so all runtime `if tracingEnabled` checks in public methods disappear.
- `otel-gorilla-ws-flag-wiring`: Wiring of the shared helper into `otel-gorilla-ws`. Keeps the existing two-tier gate (global + `OTEL_GORILLA_WS_TRACING_ENABLED`). Migrates `Conn` to strategy-split impls while preserving the existing `Sec-WebSocket-Protocol` subprotocol negotiation runtime override (impl selection happens after `Dial` / `Upgrade` completes).
- `module-directory-layout`: Cross-module convention for repository layout. Each module follows the same directory shape, aligned with Go community standards (`golang-standards/project-layout`, idiomatic Go module conventions): facade files at module root, helpers grouped by concern, `internal/` for compiler-enforced privacy, `examples/` for runnable demos, `tests/integration/` for testcontainers-based tests. Categories are named and grouped consistently so new contributors can navigate any of the four modules without re-learning the layout.

### Modified Capabilities

None — no existing capability specs in `openspec/specs/`.

## Impact

- **Code**: New `internal/flags/` package shared by all four modules (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`). Each module deletes its local `env_flags.go` (and `envEnabledByDefault` duplication). `otel-nats` and `otel-gorilla-ws` gain `internal/direct/` and `internal/traced/` packages mirroring the `otel-mongo` layout.
- **API**: Public Go API of all three packages is source-compatible. Functional options (`WithTracePropagationEnabled`, etc.) keep current semantics. No new env vars — gate surface per package is unchanged.
- **Env vars**: Unchanged. `otel-mongo` still uses `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_MONGO_TRACING_ENABLED` + `OTEL_MONGO_PROPAGATION_ENABLED`. `otel-nats` still uses `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_NATS_TRACING_ENABLED`. `otel-gorilla-ws` still uses `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_GORILLA_WS_TRACING_ENABLED`.
- **Dependencies**: No new third-party deps. `internal/flags/` uses only `os`, `strings`, `sync`, `sync/atomic`.
- **CI**: Existing four-module matrix runs unchanged. Add a drift-check step that fails the build if any module re-introduces a local copy of `envEnabledByDefault` or if the four `internal/flags/flags.go` copies drift.
- **Docs**: `README.md` and `README.zh-TW.md` for `otel-mongo`, `otel-nats`, `otel-gorilla-ws`; root `CLAUDE.md`; `pkg/instrumentation-go/CLAUDE.md`.
- **Versioning**: Minor bump on each of the four modules (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) — pre-1.0, layout refactor only, no public API or env-var surface change. Release tags issued separately per module.
- **Consumers**: `otel-traces-test` services (`api`, `worker`, `dbwatcher`) — no env wiring changes needed; existing `docker-compose.yml` and Helm values continue to work.
