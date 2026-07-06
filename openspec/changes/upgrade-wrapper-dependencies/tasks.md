## 1. Go toolchain floor (blocking prerequisite)

- [ ] 1.1 Bump the `go` directive from `1.24.0` to `1.25.0` in all 11 `go.mod` files: `otel-mongo/go.mod`, `otel-mongo/examples/go.mod`, `otel-mongo/tests/integration/go.mod`, `otel-mongo/v2/go.mod`, `otel-mongo/v2/tests/integration/go.mod`, `otel-nats/go.mod`, `otel-nats/examples/go.mod`, `otel-nats/tests/integration/go.mod`, `otel-gorilla-ws/go.mod`, `otel-gorilla-ws/examples/go.mod`, `otel-gorilla-ws/tests/integration/go.mod`.
- [ ] 1.2 Bump `go-version: "1.24"` to `go-version: "1.25"` in both jobs (`test-and-lint`, `integration-test`) in `.github/workflows/ci.yml`.
- [ ] 1.3 Sanity check: run `go build ./...` in each of the 4 top-level modules (still on old dependency versions) to confirm the Go 1.25 toolchain bump alone doesn't break anything before touching dependency versions.

## 2. otel-mongo (v1) dependency bump

- [ ] 2.1 In `otel-mongo/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/metric@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0 go.mongodb.org/mongo-driver@v1.17.9 github.com/testcontainers/testcontainers-go/modules/mongodb@latest`, then `go mod tidy`.
- [ ] 2.2 Verify `testcontainers-go`'s direct/indirect marker in `go.mod` is still consistent after tidy (design.md notes it's oddly listed as a direct require in this module already).
- [ ] 2.3 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-mongo/` — all three must pass with 0 issues.
- [ ] 2.4 Re-run `grep -rE '"go\.opentelemetry\.io/otel' internal/direct` in `otel-mongo/` and confirm zero matches (disabled-mode invariant, mirrors the CI check).
- [ ] 2.5 Bump the same otel-family + testcontainers deps in `otel-mongo/examples/go.mod` (`go get` + `go mod tidy`), then `go build ./...` (examples module isn't in the CI lint/test matrix, but must still compile).
- [ ] 2.6 Bump the same otel-family + testcontainers deps in `otel-mongo/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running).

## 3. otel-mongo/v2 dependency bump

- [ ] 3.1 In `otel-mongo/v2/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/metric@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0 go.mongodb.org/mongo-driver/v2@v2.7.0 github.com/testcontainers/testcontainers-go/modules/mongodb@latest`, then `go mod tidy`.
- [ ] 3.2 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-mongo/v2/` — all three must pass with 0 issues.
- [ ] 3.3 Re-run the `internal/direct/` no-OTel-SDK-imports grep in `otel-mongo/v2/` and confirm zero matches.
- [ ] 3.4 Check `Collection.Clone`'s `BSONOptions` propagation fix (GODRIVER-3862, landed between v2.6.0 and v2.7.0) doesn't break any existing test that asserted the old non-propagating behavior.
- [ ] 3.5 Bump the same otel-family + testcontainers deps in `otel-mongo/v2/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running; note `otel-mongo/v2` has no separate `examples/` submodule — `otel-mongo/examples/` already covers v2 usage).

## 4. otel-nats dependency bump

- [ ] 4.1 In `otel-nats/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/metric@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0 github.com/nats-io/nats.go@v1.52.0 github.com/nats-io/nats-server/v2@v2.14.3`, then `go mod tidy`.
- [ ] 4.2 Run `go build ./... && go test -v -race ./...` in `otel-nats/` immediately; inspect any failure in publish-path tests for the new v1.48.0 publish-subject validation (rejects protocol-breaking characters) as the likely cause.
- [ ] 4.3 Run `golangci-lint run ./...` in `otel-nats/` — 0 issues.
- [ ] 4.4 Bump the same deps in `otel-nats/examples/go.mod` (`go get` + `go mod tidy`), then `go build ./...`.
- [ ] 4.5 Bump the same deps in `otel-nats/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running; exercises the embedded `nats-server/v2` v2.14.3 stable build).

## 5. otel-gorilla-ws dependency bump

- [ ] 5.1 In `otel-gorilla-ws/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0`, then `go mod tidy` (this module has no `otel/metric` or OTLP exporter direct requires — confirm `go mod tidy` doesn't add them as unwanted new direct deps).
- [ ] 5.2 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-gorilla-ws/` — all three must pass with 0 issues.
- [ ] 5.3 Bump the same deps in `otel-gorilla-ws/examples/go.mod` (`go get` + `go mod tidy`), then `go build ./...`.
- [ ] 5.4 Bump the same deps in `otel-gorilla-ws/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running).

## 6. Version bump to 0.6.0

- [ ] 6.1 Bump `instrumentationVersion` from `"0.5.0"` to `"0.6.0"` in `otel-mongo/otelmongo/version.go`.
- [ ] 6.2 Bump `instrumentationVersion` from `"0.5.0"` to `"0.6.0"` in `otel-mongo/v2/version.go`.
- [ ] 6.3 Bump `instrumentationVersion` from `"0.5.0"` to `"0.6.0"` in `otel-nats/otelnats/conn.go`.
- [ ] 6.4 Bump the literal returned by `Version()` from `"0.5.0"` to `"0.6.0"` in `otel-gorilla-ws/version.go`.
- [ ] 6.5 Re-run `go test -v -race ./...` in all four top-level modules to confirm any test asserting the literal version string (e.g. `Version()` unit tests) is updated to expect `0.6.0`, not just left passing by accident.

## 7. Final verification

- [ ] 7.1 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` one more time in all four top-level modules (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) as a final green pass after the version bump.
- [ ] 7.2 Run the `internal/direct/` no-OTel-SDK-imports grep one final time in `otel-mongo/` and `otel-mongo/v2/`.
- [ ] 7.3 Confirm `git diff` touches exactly the 11 `go.mod`/`go.sum` pairs, `.github/workflows/ci.yml`, and the 4 version-constant files (plus any nats.go v1.52.0 adaptation, if the v1.48.0 subject-validation check in 4.2 surfaced one) — no unrelated files changed.
