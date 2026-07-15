# Proposal: mongo-v2-go-resolvable-tags

## Why

`otel-mongo/v2`'s module path ends in the `/v2` major-version suffix, and Go's semantic import versioning requires such a module's versions to be `v2.x.y` with the tag prefix being the module subdirectory **minus** the suffix — i.e. `otel-mongo/v2.x.y`. Every tag this repo has ever pushed for the module (`otel-mongo/v2/v0.1.5` … `otel-mongo/v2/v0.7.0`) uses the wrong shape and has **never been resolvable** by `go get`/`go list -m` (error: "module path includes a major version suffix, so major version must match"). Verified against ecosystem precedent: `go.mongodb.org/mongo-driver/v2` (root layout) tags `v2.x.y`; `github.com/googleapis/gax-go/v2` (physical `v2/` subdirectory, same layout as ours) also tags `v2.x.y`. Consumers today can only pin by commit pseudo-version (`v2.0.0-<ts>-<hash>`).

## What Changes

- **New tag line for `otel-mongo/v2`**: `otel-mongo/v2.x.y`, where `v2.MINOR.PATCH` tracks the sibling modules' `0.MINOR.PATCH` (first tag: `otel-mongo/v2.7.0`, same content as the unresolvable `otel-mongo/v2/v0.7.0`).
- **Version constant** `otel-mongo/v2/version.go` bumps `0.7.0` → `2.7.0` (and `version_test.go` expectation): the instrumentation-scope version reported on every span now matches the Go module version line.
- **`release-guard.yml` re-routes by tag shape**: `otel-mongo/v2.*` → v2 module (constant in `otel-mongo/v2/version.go`); `otel-mongo/v[01].*` stays the v1 module; the deprecated `otel-mongo/v2/v*` shape now **fails fast** with an explanatory error instead of being validated, so the unresolvable shape can never be pushed again.
- **`VERSIONING.md`** documents the v2 exception: why the shape differs, the `v2.MINOR.PATCH ↔ 0.MINOR.PATCH` mapping, the deprecated-shape history (tags kept for the record, never resolvable), and the commit-pin escape hatch for pre-2.7.0 content.
- **`otel-mongo/v2/CHANGELOG.md`** gains a `2.7.0` entry (re-versioning of the 0.7.0 content under the Go-resolvable line; notes the scope-version change).
- Old tags and the `otel-mongo/v2 v0.7.0` GitHub Release are kept (history); the release note gains a deprecation pointer to `v2.7.0`.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `release-versioning`: the "Written versioning policy" requirement gains the v2 tag-shape exception and mapping; the "Release-tag version guard in CI" requirement's pattern table and routing change (v2 shape validated, deprecated shape hard-fails).

## Impact

- `.github/workflows/release-guard.yml` — case routing.
- `otel-mongo/v2/version.go`, `otel-mongo/v2/version_test.go` — constant `2.7.0`.
- `otel-mongo/v2/CHANGELOG.md`, `VERSIONING.md`, `CLAUDE.md` — docs.
- Emitted telemetry: `otel.scope.version` on v2 spans becomes `2.7.0` (was `0.7.0`) — metadata only, noted in CHANGELOG.
- No Go API change. v1 module untouched.
