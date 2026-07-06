## Context

Four independent Go modules (`otel-mongo/`, `otel-mongo/v2/`, `otel-nats/`, `otel-gorilla-ws/`), each with its own `go.mod`, plus `examples/` and `tests/integration/` sub-modules per top-level module (11 `go.mod` files total). All currently pin `go.opentelemetry.io/otel*` to v1.39.0. Each wrapped upstream client library has newer releases:

| Module | Dependency | Current | Latest (Go proxy) |
|---|---|---|---|
| all 4 | `go.opentelemetry.io/otel`, `.../sdk`, `.../trace`, `.../metric`, `.../exporters/otlp/otlptrace/otlptracegrpc`, `.../otlptracehttp` | v1.39.0 | v1.44.0 |
| otel-mongo | `go.mongodb.org/mongo-driver` | v1.17.2 | v1.17.9 |
| otel-mongo/v2 | `go.mongodb.org/mongo-driver/v2` | v2.6.0 | v2.7.0 |
| otel-nats | `github.com/nats-io/nats.go` | v1.38.0 | v1.52.0 |
| otel-nats | `github.com/nats-io/nats-server/v2` (test-only, embedded server) | v2.11.0-preview.2 | v2.14.3 |
| otel-mongo, otel-mongo/v2, all `tests/integration/` | `github.com/testcontainers/testcontainers-go`(+`/modules/mongodb`) | v0.34.0 | v0.43.0 |

`gorilla/websocket` (v1.5.3), `stretchr/testify` (v1.11.1), and `go.opentelemetry.io/auto/sdk` (v1.2.1) are already at the latest available version — no bump needed, though their transitive `go.sum` entries may shift as a side effect of other bumps.

**Blocking prerequisite discovered during research**: `go.opentelemetry.io/otel`'s own `go.mod` bumped its `go` directive from `1.24.0` to `1.25.0` starting at v1.42.0 (confirmed by fetching `go.mod` for tags v1.39.0, v1.41.0, v1.42.0, v1.44.0 directly from the upstream repo — v1.39.0/v1.41.0 both say `go 1.24.0`, v1.42.0/v1.44.0 both say `go 1.25.0`). All 11 `go.mod` files in this repo currently pin `go 1.24.0`, and `.github/workflows/ci.yml` pins `go-version: "1.24"` in both the `test-and-lint` and `integration-test` jobs. **The otel bump to v1.44.0 cannot land until the Go toolchain requirement is raised to 1.25.0 across every module and CI is updated to install Go ≥1.25** — otherwise `go build`/`go mod tidy` will fail with a `go.mod` version-requirement error the moment `go.opentelemetry.io/otel` resolves to v1.42.0+.

## Goals / Non-Goals

**Goals:**
- Bring every direct dependency to its latest stable released version across all 11 `go.mod` files.
- Bump `instrumentationVersion` from `0.5.0` → `0.6.0` in all four top-level modules.
- Keep `go build`, `go test -race`, and `golangci-lint run` green (0 issues) in every module after the bump, per this repo's mandatory-verification rule in `CLAUDE.md`.
- Preserve the existing public API surface of all four wrapper packages — this is a dependency-currency change, not a feature or breaking-API change to callers.

**Non-Goals:**
- No new OTel instrumentation features, spans, or attributes.
- No changes to the strategy-split / cached-gate disabled-mode-invariant architecture.
- No `nats-server` upgrade beyond what's needed to move off the `-preview` build (it is test-only; its API is not part of the shipped wrapper).

## Decisions

0. **Bump the Go toolchain requirement to `go 1.25.0` in all 11 `go.mod` files and to `go-version: "1.25"` in both CI jobs, as the first task, before any otel bump.** This is the minimal version satisfying otel v1.44.0's own requirement (its `go.mod` says `1.25.0`, not higher). The local dev toolchain (`go1.26.4`) already satisfies this floor, so no local tooling change is needed.
   - *Alternative considered*: jump straight to the latest Go release (matching the locally-installed `go1.26.4`) instead of the minimum `1.25.0`. Rejected for this change — the user's ask was to upgrade the OTel SDK and wrapper packages, and the Go-version bump is a forced side effect of that, not an independently-requested upgrade; taking the minimal floor keeps the diff scoped to what's actually required and avoids opening a second, unrelated compatibility surface (newer Go toolchain features/vet changes) in the same change.

1. **Bump all `go.opentelemetry.io/otel*` packages together, in lockstep, across all 4 modules.** These packages are versioned as a matched set upstream; mixing versions (e.g. `otel` v1.44.0 with `otel/trace` v1.39.0) risks incompatible internal APIs. Each module's `go.mod` gets all six otel-family requires bumped to v1.44.0 in the same change.
   - *Alternative considered*: bump module-by-module over separate PRs. Rejected — CLAUDE.md's "mandatory after any `.go` change" verification loop makes one combined pass more efficient than four, and the otel-family packages are lockstep-versioned anyway so partial bumps buy nothing.

2. **Use `go get <module>@latest` + `go mod tidy` per module, run inside each module directory**, rather than hand-editing `go.mod` version strings. This lets Go's module resolver compute correct transitive/indirect version bumps (e.g. `go.opentelemetry.io/otel/exporters/otlp/otlptrace` indirect, `golang.org/x/*` transitive deps) instead of guessing them by hand.

3. **`nats.go` v1.38.0 → v1.52.0 (14 minor versions) confirmed safe/mechanical** — reviewed all 19 upstream release changelogs (v1.39.0 through v1.52.0) directly; no breaking changes to `nats.Connect`, `MsgHandler`/`func(*nats.Msg)`, `Conn.Publish`/`PublishMsg`/`Subscribe`, `Header`, `jetstream.Consumer.Messages()`, or `jetstream.Msg`. One behavioral addition to smoke-test: **v1.48.0** added publish-subject validation (rejects subjects containing protocol-breaking characters); run the existing publish-path tests after the bump to confirm no test fixture relies on a subject that would now be rejected.

4. **Move `nats-server/v2` off the `-preview` tag to a stable v2.14.3** since it is a test-only embedded server (confirmed via grep — not imported by any non-test file in `otelnats`/`oteljetstream`). Low risk: only the embedded-server bootstrap in `*_test.go` files is affected, not the wrapper's public API.

5. **Bump `instrumentationVersion` string constants last**, after all dependency bumps are verified green, in one small commit-sized task per module (`otel-nats/otelnats/conn.go`, `otel-mongo/otelmongo/version.go`, `otel-mongo/v2/version.go`, `otel-gorilla-ws/version.go`). This is a pure string literal change with no dependency interaction, so it's low-risk and easy to verify independently (`Version()` return value / `instrumentationVersion` const).

6. **Bump `examples/` and `tests/integration/` sub-modules' otel-family requires too**, since they independently `require` `go.opentelemetry.io/otel*` in their own `go.mod` (per CLAUDE.md, these are separate Go modules). Skipping them would leave the repo in a mixed-version state that `go build ./...` run from the top-level module won't catch (sub-modules build independently).

## Risks / Trade-offs

- **[Risk]** Bumping `go.opentelemetry.io/otel` past v1.41.0 without first raising the `go` directive fails `go mod tidy`/`go build` with a Go-version-requirement error, in all 11 modules simultaneously → **Mitigation**: Decision 0 sequences the Go-toolchain bump (all `go.mod` files + both CI `go-version` pins) as the very first task, verified independently (`go version` ≥ 1.25 report, `go build ./...` still green with old deps) before any otel `go get` runs.
- **[Risk]** `nats.go` v1.48.0's new publish-subject validation rejects a subject used in an existing test fixture → **Mitigation**: confirmed via upstream changelog review that this is the only user-visible behavior addition in the full v1.39.0–v1.52.0 range; run `go test -race ./...` in `otel-nats/` immediately after the bump and inspect any `Publish`-path failures for a subject-validation cause specifically.
- **[Risk]** `mongo-driver`/`mongo-driver/v2` patch/minor bumps deprecate or change a BSON/cursor/options API used by `internal/direct` or `internal/traced` impls → **Mitigation**: confirmed via upstream release notes that v1.17.2→v1.17.9 and v2.6.0→v2.7.0 contain only bugfixes and non-breaking behavior refinements (e.g. v2's `Collection.Clone` now correctly propagates `BSONOptions` — verify no test asserted the old non-propagating behavior; causally-consistent sessions now send `afterClusterTime` on writes). Run both v1 and v2 build+test+lint per CLAUDE.md's "v1 and v2 parity rule"; the strategy-split compile-time assertions (`var _ shared.CursorImpl = (*traced.Cursor)(nil)`, etc.) will fail loudly if an impl's method set no longer satisfies the shared interfaces.
- **[Risk]** CI's "no OTel SDK imports in `internal/direct/`" grep check breaks if a bumped indirect otel package gets pulled into `internal/direct/` transitively → **Mitigation**: this is a source-level import grep, not a `go.sum` check, so it's unaffected by version bumps as long as no new `import` statement is added to `internal/direct/*.go`; verify by re-running the grep locally before considering the module done.
- **[Trade-off]** Bumping `testcontainers-go` (v0.34.0 → v0.43.0) touches only test-only dependencies (`tests/integration/` sub-modules, plus the oddly-direct requires in `otel-mongo/go.mod` and `otel-mongo/v2/go.mod`, where `testcontainers-go` itself is `// indirect` but `.../modules/mongodb` is not) — accepted as in-scope since CLAUDE.md instructs running integration tests as part of verification, and stale testcontainers versions can fail against newer Docker/Podman daemons. Run `go mod tidy` and confirm the direct/indirect markers stay consistent.

## Migration Plan

1. For each of the 4 top-level modules (in isolation, one at a time): `cd <module>/ && go get -u ./... && go mod tidy`, then `go build ./... && go test -race ./... && golangci-lint run ./...` — all three must pass before moving to the next module (per CLAUDE.md's mandatory verification loop).
2. Repeat step 1 for each module's `examples/` and `tests/integration/` sub-modules (`go build`/`go test` only for `examples/`; full integration suite for `tests/integration/`, which requires Docker/Podman running).
3. Bump the four `instrumentationVersion` constants to `0.6.0` after all dependency bumps are green.
4. Re-run the full CI matrix locally where feasible (`test-and-lint` across all 4 modules, plus the `internal/direct/` grep check) before pushing.
5. **Rollback**: each module's dependency bump is an independent `go.mod`/`go.sum` change — if one module's upgrade surfaces an unresolvable break (e.g. a genuine nats.go v1.52.0 incompatibility), that module's `go.mod`/`go.sum` can be reverted independently without affecting the other three modules' already-verified bumps, since they share no `go.mod`.

## Open Questions

- Should `nats-server/v2`'s jump from a `-preview` tag to `v2.14.3` stable be pinned to an earlier stable (e.g. the first v2.11.x/v2.12.x stable release) instead of jumping straight to latest, to minimize the diff being validated in one step? Current plan: go straight to latest stable per the "upgrade to latest" instruction, since it's test-only and any embedded-server API break will surface immediately via `go test`.
- Should the Go toolchain directive go to the exact minimum `1.25.0` required by otel v1.44.0, or track the newest published Go patch release available at implementation time? Current plan: `1.25.0` exactly (see Decision 0) to keep the diff minimal and scoped to what the otel bump actually requires.
