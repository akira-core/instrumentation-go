# Versioning

This is a multi-module repository: `otel-mongo`, `otel-mongo/v2`, `otel-nats`, and `otel-gorilla-ws` each have their own `go.mod` and version independently. `examples/` and `tests/integration/` sub-modules follow their parent module's version informally (they are not separately tagged) and are expected to build against the parent's `HEAD`.

## Tag format

```
<module>/v<x.y.z>
```

Examples: `otel-nats/v0.7.0`, `otel-mongo/v0.7.0`. The module segment matches the directory path relative to the repo root — with one exception, below.

Each tag must point at a commit where that module's version constant equals the tag's version. A CI workflow enforces this on every push of a tag matching one of the four module patterns (`otel-mongo/v[0-9]*`, `otel-mongo/v2/v[0-9]*`, `otel-nats/v[0-9]*`, `otel-gorilla-ws/v[0-9]*`) — see [CI enforcement](#ci-enforcement) below.

### Exception: `otel-mongo/v2` is tagged `otel-mongo/v2.x.y`

`otel-mongo/v2`'s module path ends in the `/v2` **major-version suffix**. Go semantic import versioning strips that suffix from the tag prefix and requires the version's major to match it, so the module's Go-resolvable tag shape is:

```
otel-mongo/v2.x.y        (e.g. otel-mongo/v2.7.0)
```

`v2.MINOR.PATCH` tracks the sibling modules' `0.MINOR.PATCH` line — a coordinated release tagged `0.8.0` elsewhere is `otel-mongo/v2.8.0` here. The version constant in `otel-mongo/v2/version.go` carries the same `2.x.y` value (and is what `otel.scope.version` reports on the module's spans).

**History:** every tag of the old `otel-mongo/v2/v0.x.y` shape (`0.1.5` through `0.7.0`) was **never resolvable** by `go get`/`go list -m` — a `/v2` module path rejects major-0 versions outright. Those tags are retained as history only; `otel-mongo/v2.7.0` carries the identical content to `otel-mongo/v2/v0.7.0`. Pre-`v2.7.0` content is reachable via commit pseudo-versions (`go get …/otel-mongo/v2@<commit>` → `v2.0.0-<timestamp>-<hash>`). The release guard now rejects the deprecated shape outright (see below). Ecosystem precedent for the shape: `go.mongodb.org/mongo-driver/v2` and `github.com/googleapis/gax-go/v2` both tag plain `v2.x.y`; a hypothetical future `otel-mongo/v3` wrapper would follow the same scheme (`otel-mongo/v3.x.y`).

## Pre-1.0 (`0.x`) policy

All four modules are pre-1.0. Within the `0.x` line:

- **Breaking change → at least a minor bump** (`0.6.x` → `0.7.0`). A breaking change is anything that changes the meaning of already-emitted telemetry (attribute key/value semantics, span kind, span duration semantics) or changes an exported Go API signature/behavior in a way existing callers must react to.
- **Additive feature or bug fix → a patch bump is sufficient** (`0.7.0` → `0.7.1`), unless the fix itself changes existing behavior in a way covered by the breaking-change definition above, in which case it still needs a minor bump even though it's "just a fix."
- Modules bump independently. A release that touches multiple modules (common, since most changes to `internal/flags` or a shared pattern land everywhere at once) does not require the same version number across modules, but in practice this repo has kept the four numbers aligned through `0.7.0` for a simpler release story — that's a convention, not a rule.

## Where release notes live

1. **Module-level `CHANGELOG.md`** (`otel-nats/CHANGELOG.md`, `otel-mongo/CHANGELOG.md`, `otel-mongo/v2/CHANGELOG.md`, `otel-gorilla-ws/CHANGELOG.md`) — inside the module directory, so it ships in the Go module zip served by the module proxy. This is the canonical, per-module record; add an entry before tagging a release.
2. **GitHub Releases**, one per tag, summarizing that module's `CHANGELOG.md` entry for the version being released.
3. **Root-level `RELEASE-NOTES-<version>.md`** (e.g. `RELEASE-NOTES-0.6.0.md`) for releases that touch all four modules together and warrant a single cross-module summary — optional, used for major coordinated releases, not required for every tag.

Each module's `CHANGELOG.md` starts at `0.6.0` rather than being reconstructed further back. **Root cause**: the `0.5.x` line was tagged from a side branch (the `legacy_go` line) that never carried a `CHANGELOG.md`; when `0.6.0` was cut from `main`, there was no file to carry forward. Earlier history is still recoverable from git tags and diffs, just not narrated in the CHANGELOG.

## Version constant locations

The CI guard (below) and any manual version bump need to know exactly where each module's reported version string lives:

| Module | File | Symbol |
|---|---|---|
| `otel-nats` | `otel-nats/otelnats/conn.go` | `instrumentationVersion` const |
| `otel-mongo` (v1) | `otel-mongo/otelmongo/version.go` | `instrumentationVersion` const |
| `otel-mongo/v2` | `otel-mongo/v2/version.go` | `instrumentationVersion` const |
| `otel-gorilla-ws` | `otel-gorilla-ws/version.go` | `Version()` return literal |

This constant is what every wrapper package reports as its `TracerProvider.Tracer(..., trace.WithInstrumentationVersion(Version()))` instrumentation-scope version — it appears on every span the module emits, real or noop. Bump it in the same commit as the rest of the release's code changes, before tagging.

## CI enforcement

`.github/workflows/release-guard.yml` triggers on any pushed tag matching one of four explicit patterns — `otel-mongo/v[0-9]*`, `otel-mongo/v2/v[0-9]*`, `otel-nats/v[0-9]*`, `otel-gorilla-ws/v[0-9]*` (a single `otel-*/v*` glob would miss tags containing a second `/`, since GitHub Actions tag globs do not cross `/`). It parses the module and version out of the tag name, extracts the corresponding version constant using the table above, and fails the workflow if they don't match. This exists because a hand-maintained constant with no automated check has already shipped wrong once (`otel-nats` `0.5.0` reported `0.4.1` on every span) — the guard makes that class of mistake fail loudly at tag-push time instead of shipping silently.

Two `otel-mongo` routing details:

- `otel-mongo/v2.*` tags (the [v2 exception](#exception-otel-mongov2-is-tagged-otel-mongov2xy) shape) validate against `otel-mongo/v2/version.go`; other `otel-mongo/v*` tags validate against the v1 module's constant.
- The deprecated `otel-mongo/v2/v*` shape **fails immediately** with an error pointing at `otel-mongo/v2.x.y`. Its trigger pattern is kept precisely so the mistake fails loudly instead of pushing with no CI signal.

The guard checks the version constant against the tag; it does not check that a `CHANGELOG.md` entry exists for the version. That remains a review-checklist item.
