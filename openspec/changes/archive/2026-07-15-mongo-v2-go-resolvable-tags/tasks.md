# Tasks: mongo-v2-go-resolvable-tags

## 1. Version constant (TDD)

- [x] 1.1 `otel-mongo/v2/version_test.go` — change expectation to `"2.7.0"`; run, watch it fail against the current `0.7.0` constant (RED).
- [x] 1.2 `otel-mongo/v2/version.go` — bump `instrumentationVersion` to `"2.7.0"`; test passes (GREEN).

## 2. Release guard

- [x] 2.1 `.github/workflows/release-guard.yml` — re-route the `case`: `otel-mongo/v2/v*` → hard fail with pointer to `otel-mongo/v2.x.y`; `otel-mongo/v2.*` → MODULE=`otel-mongo/v2`, VERSION=`${TAG#otel-mongo/v}`, FILE=`otel-mongo/v2/version.go`; plain `otel-mongo/v*` unchanged (v1).
- [x] 2.2 Simulate the guard script locally for all shapes: `otel-mongo/v2.7.0` (pass), `otel-mongo/v2/v0.8.0` (fail with deprecation error), `otel-mongo/v0.7.0` (pass, v1), `otel-nats/v0.7.0` (pass), unknown shape (fail).

## 3. Verification gate

- [x] 3.1 `cd otel-mongo/v2 && go build ./... && go test -race ./... && golangci-lint run ./...` — all green.

## 4. Documentation

- [x] 4.1 `VERSIONING.md` — add the `otel-mongo/v2` exception section: Go SIV rule, `otel-mongo/v2.x.y` shape, `v2.MINOR.PATCH ↔ 0.MINOR.PATCH` mapping, historical-tags note (never resolvable, kept for history), commit pseudo-version escape hatch; update the CI-enforcement section for the new routing and deprecated-shape hard fail.
- [x] 4.2 `otel-mongo/v2/CHANGELOG.md` — add `## [2.7.0] - 2026-07-15` pointer entry (same content as 0.7.0, re-versioned under the Go-resolvable v2 line; `otel.scope.version` now reports `2.7.0`).
- [x] 4.3 `CLAUDE.md` — update the Versioning + CI sections where the `otel-mongo/v2/v[0-9]*` shape is described.

## 5. Release (post-merge)

- [x] 5.1 PR to `main`, merge; tag `otel-mongo/v2.7.0` on the merge commit and push; release-guard passes (live verification of the new routing).
- [x] 5.2 GitHub Release `otel-mongo/v2 v2.7.0`; verify `GOPROXY=direct go list -m github.com/akira-core/instrumentation-go/otel-mongo/v2@v2.7.0` resolves.
- [x] 5.3 Edit the existing `otel-mongo/v2 v0.7.0` GitHub Release body: banner pointing at `v2.7.0` as the Go-resolvable tag for the same content.
