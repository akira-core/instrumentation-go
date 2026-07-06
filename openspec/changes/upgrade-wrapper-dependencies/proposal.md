## Why

All four modules pin `go.opentelemetry.io/otel*` to v1.39.0 and each wrapped upstream client library (mongo-driver, mongo-driver/v2, nats.go, gorilla/websocket) has newer patch/minor releases available. Staying current keeps the modules on supported, patched upstream releases and validates the wrapper API surface still compiles and passes tests against the latest OTel SDK and client libraries before the next tagged release.

## What Changes

- **BREAKING (prerequisite)**: bump the `go` directive from `1.24.0` → `1.25.0` in all 11 `go.mod` files, and `go-version` from `"1.24"` → `"1.25"` in both `.github/workflows/ci.yml` jobs. Confirmed by inspecting upstream `go.mod` files: `go.opentelemetry.io/otel` itself requires `go 1.25.0` starting at v1.42.0, so this repo cannot resolve the target otel version without raising its own floor first.
- Bump `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/trace`, `go.opentelemetry.io/otel/metric`, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`, and `.../otlptracehttp` from v1.39.0 → v1.44.0 across all four modules (`otel-mongo/`, `otel-mongo/v2/`, `otel-nats/`, `otel-gorilla-ws/`) and their `examples/` and `tests/integration/` submodules.
- Bump `go.mongodb.org/mongo-driver` (v1 API) from v1.17.2 → v1.17.9 in `otel-mongo/`.
- Bump `go.mongodb.org/mongo-driver/v2` from v2.6.0 → v2.7.0 in `otel-mongo/v2/`.
- Bump `github.com/nats-io/nats.go` from v1.38.0 → v1.52.0 in `otel-nats/`.
- Bump `github.com/nats-io/nats-server/v2` (test-only embedded server dependency) from v2.11.0-preview.2 → v2.14.3 (moves off a preview build onto a stable release) in `otel-nats/`.
- Bump `github.com/testcontainers/testcontainers-go` and `.../modules/mongodb` from v0.34.0 → latest stable in modules/submodules that reference them (`otel-mongo/`, `otel-mongo/v2/`, and the `tests/integration/` submodules for all four modules).
- `github.com/gorilla/websocket` (v1.5.3), `github.com/stretchr/testify` (v1.11.1), and `go.opentelemetry.io/auto/sdk` (v1.2.1) are already at latest — no change needed, but `go.sum`/indirect deps will shift as a side effect of the other bumps.
- Bump `instrumentationVersion` from `0.5.0` → `0.6.0` in all four modules' version constants (`otel-nats/otelnats/conn.go`, `otel-mongo/otelmongo/version.go`, `otel-mongo/v2/version.go`, `otel-gorilla-ws/version.go`'s `Version()` return).
- `nats.go` v1.38.0 → v1.52.0 is a large jump (14 minor versions) but was verified against every upstream changelog in the range: no breaking API changes affecting `Connect`, `MsgHandler`, `Subscribe`, header access, or the JetStream `Consumer.Messages()` iterator. The only user-visible addition is stricter publish-subject validation landed in v1.48.0 (rejects protocol-breaking characters), worth a smoke-test of publish paths.

## Capabilities

### New Capabilities
- `instrumentation-scope-metadata`: baselines the existing, previously-undocumented behavior that every wrapper package (`otelmongo`, `otelmongo/v2`, `otelnats`, `otelgorillaws`) reports its module version to the configured `TracerProvider` via `trace.WithInstrumentationVersion(Version())` on every tracer it creates. This change bumps that reported version from `0.5.0` to `0.6.0` in all four modules, so the requirement is baselined now to make the version-bump user-observable and testable.

### Modified Capabilities
(none — no other documented capability requirement in `openspec/specs/` changes)

## Impact

- **Affected code**: `go.mod`/`go.sum` in all four top-level modules plus their `examples/` and `tests/integration/` submodules (11 `go.mod` files total, `go` directive bumped in every one); version constant files in each of the four top-level modules; `.github/workflows/ci.yml` (`go-version` in both jobs).
- **Affected dependencies**: `go.opentelemetry.io/otel*` (all 4 modules), `go.mongodb.org/mongo-driver` (otel-mongo v1), `go.mongodb.org/mongo-driver/v2` (otel-mongo v2), `github.com/nats-io/nats.go` + `github.com/nats-io/nats-server/v2` (otel-nats), `github.com/testcontainers/testcontainers-go` + `modules/mongodb` (otel-mongo v1/v2 + all `tests/integration/` submodules).
- **Not affected**: `github.com/gorilla/websocket`, `github.com/stretchr/testify`, `go.opentelemetry.io/auto/sdk` (already latest); the wrapper API surface itself does not change for callers.
- **CI**: `.github/workflows/ci.yml` matrix (`test-and-lint`, `integration-test`) must pass for all four modules on Go 1.25 after the bump; the `otel-mongo`/`otel-mongo/v2` "no OTel SDK imports in internal/direct/" grep check must still pass.
- **Reported scope version**: spans emitted by all four modules will carry `otel.scope.version` (or exporter-equivalent instrumentation-scope version field) `0.6.0` instead of `0.5.0` — downstream trace consumers/dashboards keyed on that field see the new value.
