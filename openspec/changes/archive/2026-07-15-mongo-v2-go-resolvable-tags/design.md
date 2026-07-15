# Design: mongo-v2-go-resolvable-tags

## Context

Go's rule for module tags in a multi-module repo: tag = `<module subdirectory without the major-version suffix>/<vMAJOR.MINOR.PATCH>`, and a path ending in `/vN` (N≥2) must have version major N. For `github.com/akira-core/instrumentation-go/otel-mongo/v2` (physical directory `otel-mongo/v2/`), the only resolvable tag shape is `otel-mongo/v2.x.y`. All historical `otel-mongo/v2/v0.x.y` tags are dead to the toolchain — confirmed empirically (`go list -m …/v2@v0.6.0` and `@v0.7.0` both fail; commit pseudo-version `v2.0.0-<ts>-<hash>` works) and by ecosystem precedent (mongo-driver/v2 root layout and gax-go/v2 subdirectory layout both tag plain `v2.x.y`).

## Goals / Non-Goals

**Goals:**
- `go get github.com/akira-core/instrumentation-go/otel-mongo/v2@v2.7.0` resolves.
- The unresolvable tag shape can never be pushed again without CI failing loudly.
- The policy (shape, mapping, history) is written down where downstreams look.

**Non-Goals:**
- No module-path rename (rejected: breaking import change for zero benefit — the `/v2` path is the idiomatic wrapper for mongo-driver v2).
- No deletion of historical tags or the old GitHub Release (immutable history; they never resolved, so nothing breaks by keeping them).
- No backfill of resolvable tags for pre-0.7.0 content (commit pseudo-versions cover it; noted in VERSIONING.md).

## Decisions

### D1. Version mapping: `v2.MINOR.PATCH` tracks the sibling `0.MINOR.PATCH`

First tag `otel-mongo/v2.7.0` ≡ content of `otel-mongo/v2/v0.7.0`. Future coordinated releases keep minor/patch aligned (0.8.0 across modules → `otel-mongo/v2.8.0`).
- **Why**: preserves the repo's "aligned numbers for a simple release story" convention with zero mental overhead; semver-legal since the v2 line has no prior tags.
- **Alternative rejected — restart at `v2.0.0`**: cleaner semver optics, but the number would forever lag the siblings and invite "which 0.x is v2.0.3?" mapping tables.

### D2. Constant becomes `2.7.0` (not kept at `0.7.0`)

The release-guard compares tag version to the constant verbatim; a `v2.7.0` tag therefore requires the constant to say `2.7.0`. Side effect: `otel.scope.version` on emitted v2 spans reports `2.7.0` — correct (it now equals the actual module version) and documented in the CHANGELOG.
- **Alternative rejected — guard-side mapping (strip the major)**: keeps the constant at `0.7.0` but makes the guard validate a number that no longer exists anywhere a consumer can see; the constant's whole purpose is to equal the shipped version.

### D3. Guard routing: match on the version's major, deprecated shape hard-fails

`case` order in `release-guard.yml` (first match wins):
1. `otel-mongo/v2/v*` → **fail** with an error explaining the shape is not Go-resolvable and pointing at `otel-mongo/v2.x.y`. Pattern stays in the workflow trigger precisely so the push fails visibly instead of silently going unguarded.
2. `otel-mongo/v2.*` → MODULE=`otel-mongo/v2`, VERSION=`${TAG#otel-mongo/v}` (yields `2.x.y`), constant file `otel-mongo/v2/version.go`.
3. `otel-mongo/v*` → v1 module, unchanged.
- **Why hard-fail instead of dropping the trigger pattern**: dropping it would let the bad shape push cleanly with no CI signal — the guard exists to make tagging mistakes loud.
- A hypothetical future `otel-mongo/v3` wrapper would follow the same scheme (`otel-mongo/v3.x.y`); noted in VERSIONING.md, not pre-built in the guard.

### D4. Old artifacts kept, annotated

`otel-mongo/v2/v0.*` tags and the `otel-mongo/v2 v0.7.0` GitHub Release stay; the release body gains a banner pointing to `v2.7.0`. Nothing ever resolved against them, so removal buys nothing and costs history.

## Risks / Trade-offs

- **[Risk] Local guard-script testing can't exercise the real tag-push path** → Mitigation: simulate the guard's `case` logic locally against all shapes before merging; the real `otel-mongo/v2.7.0` push after merge is the live verification (same pattern as 0.7.0's guard rollout, where otel-nats went first).
- **[Risk] Downstream dashboards keyed on `otel.scope.version == "0.7.0"` for v2 spans** → Documented in CHANGELOG; scope-version pinning is rare and the old value was wrong-by-construction anyway.
- **[Trade-off] v2's CHANGELOG now has both `0.7.0` and `2.7.0` entries for the same content** → Accepted; the `2.7.0` entry is a pointer entry, and the dual record is exactly what a confused reader needs.

## Migration Plan

Single PR to `main` (constant + test + guard + docs + this change's artifacts), then tag `otel-mongo/v2.7.0` on the merge commit → guard validates the new routing live → GitHub Release → `go list -m` verification. Rollback: revert the PR; the new tag would then fail the guard on any re-push (constant back at 0.7.0), which is the guard working as intended.

## Open Questions

None.
