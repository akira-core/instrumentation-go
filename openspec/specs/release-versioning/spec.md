# release-versioning Specification

## Purpose
Per-module release and versioning policy: module-level CHANGELOGs shipped in the module zip, the written VERSIONING.md policy (independent module tags, pre-1.0 breaking-to-minor rule, version-constant locations), and the CI release-tag version guard.

## Requirements

### Requirement: Module-level CHANGELOG ships in the module zip
Each of the four modules (`otel-nats`, `otel-mongo`, `otel-mongo/v2`, `otel-gorilla-ws`) SHALL carry a `CHANGELOG.md` inside its module directory, so the file is included in the Go module zip served by the module proxy. The file SHALL follow the Keep a Changelog structure (one section per released version, newest first) and SHALL be backfilled from `0.6.0` onward; a pointer line SHALL note that pre-0.6.0 history was released from the legacy branch line and is not covered. Every release tag SHALL be preceded by a CHANGELOG entry for that version in the same module.

#### Scenario: Downstream inspects the module zip for release notes
- **WHEN** a consumer downloads `otel-nats` at any version from `0.7.0` onward via the Go module proxy
- **THEN** the module zip contains `CHANGELOG.md` with an entry for that version describing its changes, including any breaking-change migration notes

#### Scenario: Release without a CHANGELOG entry
- **WHEN** a release tag is being prepared for a module whose `CHANGELOG.md` has no entry for the new version
- **THEN** the release process SHALL NOT proceed until the entry is added (enforced by review checklist; the version-guard workflow covers the constant, not the CHANGELOG)

### Requirement: Written versioning policy
The repository SHALL contain a root-level `VERSIONING.md` documenting: the tag format `<module>/v<x.y.z>` and its `otel-mongo/v2` exception (below); that each module versions independently; the pre-1.0 policy that breaking changes require at least a **minor** bump while additive features and fixes may use a patch bump; where release notes live (module `CHANGELOG.md` plus GitHub Releases); and the source location of each module's version constant (`otel-nats/otelnats/conn.go`, `otel-mongo/otelmongo/version.go`, `otel-mongo/v2/version.go`, `otel-gorilla-ws/version.go`).

Because `otel-mongo/v2`'s module path ends in the `/v2` major-version suffix, Go semantic import versioning requires its versions to be `v2.x.y` and its tag prefix to be the module subdirectory **without** the suffix: the module SHALL be tagged `otel-mongo/v2.x.y`, with `v2.MINOR.PATCH` tracking the sibling modules' `0.MINOR.PATCH` line. `VERSIONING.md` SHALL document this exception, the mapping, that the historical `otel-mongo/v2/v0.x.y` tags (0.1.5–0.7.0) were never resolvable by Go tooling and are retained only as history, and that pre-`v2.7.0` content is reachable via commit pseudo-versions (`v2.0.0-<timestamp>-<hash>`).

#### Scenario: Downstream plans a pin upgrade across a breaking release
- **WHEN** a downstream consumer reads `VERSIONING.md` before moving a pin across `0.x` versions
- **THEN** the policy tells them a minor-version increase (e.g. `0.6.x` → `0.7.0`) may contain breaking changes documented in that module's `CHANGELOG.md`, while a patch increase does not

#### Scenario: Version constant location lookup
- **WHEN** a contributor needs to bump a module's reported instrumentation version
- **THEN** `VERSIONING.md` names the exact file holding that module's version constant

#### Scenario: Downstream pins otel-mongo/v2 by tag
- **WHEN** a consumer runs `go get github.com/akira-core/instrumentation-go/otel-mongo/v2@v2.7.0`
- **THEN** the module resolves via the `otel-mongo/v2.7.0` tag
- **AND** `VERSIONING.md` explains that `v2.MINOR.PATCH` corresponds to the sibling modules' `0.MINOR.PATCH`

### Requirement: Release-tag version guard in CI
A CI workflow SHALL trigger on pushed tags matching the four module tag patterns (`otel-mongo/v[0-9]*`, `otel-mongo/v2/v[0-9]*`, `otel-nats/v[0-9]*`, `otel-gorilla-ws/v[0-9]*` — a single `otel-*/v*` glob cannot match tags containing a second `/` because GitHub Actions tag globs do not cross `/`), parse the module path and semantic version from the tag, extract that module's version constant from its documented source location, and fail when the two values differ. The guard SHALL print both values in its failure output.

Routing SHALL distinguish the two `otel-mongo` modules by the tag's version major: `otel-mongo/v2.*` SHALL validate against `otel-mongo/v2/version.go`, while other `otel-mongo/v*` tags validate against the v1 module's `otel-mongo/otelmongo/version.go`. The deprecated shape `otel-mongo/v2/v*` SHALL fail immediately with an error explaining it is not resolvable by Go tooling and naming the correct `otel-mongo/v2.x.y` shape — the pattern stays in the trigger precisely so the mistake fails loudly rather than pushing unguarded.

#### Scenario: Tag matches the constant
- **WHEN** the tag `otel-nats/v0.7.0` is pushed and `otel-nats/otelnats/conn.go` declares `instrumentationVersion = "0.7.0"`
- **THEN** the guard workflow passes

#### Scenario: Tag does not match the constant
- **WHEN** a tag `otel-mongo/v0.7.0` is pushed while `otel-mongo/otelmongo/version.go` still declares `0.6.0`
- **THEN** the workflow fails, printing the tag version and the constant value, so the mislabelled release is caught before consumers pin it

#### Scenario: v2 tag routes to the v2 module's constant
- **WHEN** the tag `otel-mongo/v2.7.0` is pushed and `otel-mongo/v2/version.go` declares `instrumentationVersion = "2.7.0"`
- **THEN** the guard validates the tag against the v2 module's constant and passes

#### Scenario: Deprecated v2 tag shape fails fast
- **WHEN** a tag matching `otel-mongo/v2/v*` (e.g. `otel-mongo/v2/v0.8.0`) is pushed
- **THEN** the guard fails with an error stating the shape is not resolvable by Go tooling and pointing at the `otel-mongo/v2.x.y` shape

#### Scenario: Version constant moves without updating the guard
- **WHEN** a refactor relocates a module's version constant and the guard's location map is not updated
- **THEN** the next release tag for that module fails the guard (fail-closed), prompting the map update documented in `VERSIONING.md`
