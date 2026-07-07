## 1. Go toolchain floor (blocking prerequisite)

- [x] 1.1 Bump the `go` directive from `1.24.0` to `1.25.0` in all 11 `go.mod` files: `otel-mongo/go.mod`, `otel-mongo/examples/go.mod`, `otel-mongo/tests/integration/go.mod`, `otel-mongo/v2/go.mod`, `otel-mongo/v2/tests/integration/go.mod`, `otel-nats/go.mod`, `otel-nats/examples/go.mod`, `otel-nats/tests/integration/go.mod`, `otel-gorilla-ws/go.mod`, `otel-gorilla-ws/examples/go.mod`, `otel-gorilla-ws/tests/integration/go.mod`.
- [x] 1.2 Bump `go-version: "1.24"` to `go-version: "1.25"` in both jobs (`test-and-lint`, `integration-test`) in `.github/workflows/ci.yml`.
- [x] 1.3 Sanity check: run `go build ./...` in each of the 4 top-level modules (still on old dependency versions) to confirm the Go 1.25 toolchain bump alone doesn't break anything before touching dependency versions.

## 2. otel-mongo (v1) dependency bump

- [x] 2.1 In `otel-mongo/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/metric@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0 go.mongodb.org/mongo-driver@v1.17.9 github.com/testcontainers/testcontainers-go/modules/mongodb@latest`, then `go mod tidy`.
- [x] 2.2 Verify `testcontainers-go`'s direct/indirect marker in `go.mod` is still consistent after tidy (design.md notes it's oddly listed as a direct require in this module already).
- [x] 2.3 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-mongo/` — all three must pass with 0 issues.
- [x] 2.4 Re-run `grep -rE '"go\.opentelemetry\.io/otel' internal/direct` in `otel-mongo/` and confirm zero matches (disabled-mode invariant, mirrors the CI check).
- [x] 2.5 Bump the same otel-family + testcontainers deps in `otel-mongo/examples/go.mod` (`go get` + `go mod tidy`), then `go build ./...` (examples module isn't in the CI lint/test matrix, but must still compile).
- [x] 2.6 Bump the same otel-family + testcontainers deps in `otel-mongo/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running).

## 3. otel-mongo/v2 dependency bump

- [x] 3.1 In `otel-mongo/v2/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/metric@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0 go.mongodb.org/mongo-driver/v2@v2.7.0 github.com/testcontainers/testcontainers-go/modules/mongodb@latest`, then `go mod tidy`.
- [x] 3.2 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-mongo/v2/` — all three must pass with 0 issues.
- [x] 3.3 Re-run the `internal/direct/` no-OTel-SDK-imports grep in `otel-mongo/v2/` and confirm zero matches.
- [x] 3.4 Check `Collection.Clone`'s `BSONOptions` propagation fix (GODRIVER-3862, landed between v2.6.0 and v2.7.0) doesn't break any existing test that asserted the old non-propagating behavior.
- [x] 3.5 Bump the same otel-family + testcontainers deps in `otel-mongo/v2/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running; note `otel-mongo/v2` has no separate `examples/` submodule — `otel-mongo/examples/` already covers v2 usage).

## 4. otel-nats dependency bump

- [x] 4.1 In `otel-nats/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0 go.opentelemetry.io/otel/metric@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0 github.com/nats-io/nats.go@v1.52.0 github.com/nats-io/nats-server/v2@v2.14.3`, then `go mod tidy`.
- [x] 4.2 Run `go build ./... && go test -v -race ./...` in `otel-nats/` immediately; inspect any failure in publish-path tests for the new v1.48.0 publish-subject validation (rejects protocol-breaking characters) as the likely cause.
- [x] 4.3 Run `golangci-lint run ./...` in `otel-nats/` — 0 issues.
- [x] 4.4 Bump the same deps in `otel-nats/examples/go.mod` (`go get` + `go mod tidy`), then `go build ./...`.
- [x] 4.5 Bump the same deps in `otel-nats/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running; exercises the embedded `nats-server/v2` v2.14.3 stable build).

## 5. otel-gorilla-ws dependency bump

- [x] 5.1 In `otel-gorilla-ws/`: `go get go.opentelemetry.io/otel@v1.44.0 go.opentelemetry.io/otel/sdk@v1.44.0 go.opentelemetry.io/otel/trace@v1.44.0`, then `go mod tidy` (this module has no `otel/metric` or OTLP exporter direct requires — confirm `go mod tidy` doesn't add them as unwanted new direct deps).
- [x] 5.2 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-gorilla-ws/` — all three must pass with 0 issues.
- [x] 5.3 Bump the same deps in `otel-gorilla-ws/examples/go.mod` (`go get` + `go mod tidy`), then `go build ./...`.
- [x] 5.4 Bump the same deps in `otel-gorilla-ws/tests/integration/go.mod` (`go get` + `go mod tidy`), then `go test -v -race -timeout 120s ./...` (requires Docker/Podman running).

## 6. Public API surface parity audit (wrapped client libraries)

Rule: a caller must be able to reach each wrapped library's public API *through* our package. Run after the dependency bumps (sections 2–5, so both old and new versions resolve) and before the version bump. See design.md Decision 7 for the per-mechanism policy.

- [x] 6.1 For each wrapped library that changed version, capture the exported API delta of the wrapped type(s), old→new, in a scratch dir (`go doc <module>@<old> <Type>` vs `go doc <module>@<new> <Type>`, or `golang.org/x/exp/cmd/apidiff`): `go.mongodb.org/mongo-driver/mongo` v1.17.2→v1.17.9 (`Client`, `Database`, `Collection`, `Cursor`, `SingleResult`, `ChangeStream`); `go.mongodb.org/mongo-driver/v2/mongo` v2.6.0→v2.7.0 (same types); `github.com/nats-io/nats.go` v1.38.0→v1.52.0 (`Conn`, `Msg`, `Subscription`, package-level funcs); `github.com/nats-io/nats.go/jetstream` (same jump: `JetStream`, `Consumer`, `Stream`, `Msg`, `PubAck`, `StreamConfig`, `ConsumerConfig`, `StreamInfo`, `OrderedConsumerConfig`, `AckPolicy`). `gorilla/websocket` is unchanged — skip.
- [x] 6.2 Embedded / aliased wrappers (auto-parity — **verify only**): confirm `otel-mongo` & `otel-mongo/v2` facades (`*mongo.*` embeds), `otel-gorilla-ws.Conn` (`*websocket.Conn` embed), `oteljetstream.Msg` (`jetstream.Msg` embed), and the `oteljetstream` `type X = jetstream.X` aliases still compile after the bump (a renamed/removed upstream symbol breaks the embed line / alias line). For the embedded mongo types specifically, grep our source + `_test.go` for any direct call to an upstream method that the 6.1 delta shows was removed (embedding drops removed methods silently — no compile error unless referenced).
- [x] 6.3 Curated `otelnats.Conn` (hand-declared subset + `NatsConn()` escape hatch): for every ADDED exported `nats.Conn` method in the 6.1 delta, classify trace-relevant (a publish / subscribe / request / flush-with-context variant that must inject or extract trace context) vs not. Add an instrumented wrapper method for each trace-relevant addition — mirror the shape of `Conn.Publish` / `Conn.Subscribe`, cached-gate first statement `if !c.tracingEnabled { return c.nc.X(...) }` (see CLAUDE.md "Adding a new public method to a cached-gate wrapper"). Record non-trace additions as deliberately delegated to `NatsConn()`. Confirm no method the wrapper already declares was REMOVED upstream (would surface as a build error in section 4).
- [x] 6.4 Curated `oteljetstream` behavior interfaces (`JetStream`, `Consumer`, `Stream`, `ConsumeContext`, `MessagesContext`, `MessageBatch`): for every method ADDED to the corresponding upstream `jetstream` interface in the 6.1 delta, decide extend-our-interface (trace-relevant) vs omit-and-document. Note: extending one of these interfaces is a **breaking** change for any external implementer — record it in the change (the pre-1.0 `0.6.0` minor bump already permits breaking changes per CLAUDE.md versioning).
- [x] 6.5 If 6.3 / 6.4 added or changed any wrapper method: re-run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in the affected module(s), and add/extend a test exercising each newly-surfaced wrapper method. If nothing was added (pure passthrough parity held via embed/alias/escape-hatch), state that explicitly in the change notes — the audit's conclusion must be recorded either way, no silent "assumed fine".

## 7. Version bump to 0.6.0

- [x] 7.1 Bump `instrumentationVersion` from `"0.5.0"` to `"0.6.0"` in `otel-mongo/otelmongo/version.go`.
- [x] 7.2 Bump `instrumentationVersion` from `"0.5.0"` to `"0.6.0"` in `otel-mongo/v2/version.go`.
- [x] 7.3 Bump `instrumentationVersion` from `"0.5.0"` to `"0.6.0"` in `otel-nats/otelnats/conn.go`.
- [x] 7.4 Bump the literal returned by `Version()` from `"0.5.0"` to `"0.6.0"` in `otel-gorilla-ws/version.go`.
- [x] 7.5 Re-run `go test -v -race ./...` in all four top-level modules to confirm any test asserting the literal version string (e.g. `Version()` unit tests) is updated to expect `0.6.0`, not just left passing by accident.

## 8. Final verification

- [x] 8.1 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` one more time in all four top-level modules (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) as a final green pass after the version bump.
- [x] 8.2 Run the `internal/direct/` no-OTel-SDK-imports grep one final time in `otel-mongo/` and `otel-mongo/v2/`.
- [x] 8.3 Confirm `git diff` touches exactly the 11 `go.mod`/`go.sum` pairs, `.github/workflows/ci.yml`, the 4 version-constant files, plus any wrapper methods added by the section 6 parity audit and any nats.go v1.52.0 adaptation (if the v1.48.0 subject-validation check in 4.2 surfaced one) — no unrelated files changed.

## 9. Semconv import alignment (v1.37.0 → v1.41.0)

Bump generated semconv import paths to the latest version bundled in `go.opentelemetry.io/otel` v1.44.0. See design.md Decision 8.

- [x] 9.1 Replace `go.opentelemetry.io/otel/semconv/v1.37.0` with `go.opentelemetry.io/otel/semconv/v1.41.0` in all seven import sites: `otel-mongo/otelmongo/client.go`, `otel-mongo/v2/client.go`, `otel-nats/otelnats/conn.go`, `otel-nats/otelnats/conn_traced.go`, `otel-nats/oteljetstream/jetstream.go`, `otel-nats/oteljetstream/consumer.go`, `otel-gorilla-ws/options.go`.
- [x] 9.2 Grep the repo for stale `semconv/v1.37.0`, hardcoded `schemas/1.37.0`, or other semconv version strings in source, tests, and docs; update any hits. Zero hits repo-wide.
- [x] 9.3 Confirm `otel-mongo` `internal/shared/semconv.go` (hand-written stable keys) needs no change; only the `semconv.ServiceName` call sites in `client.go` move with the import path. Confirmed: zero `go.opentelemetry.io/otel/semconv` imports in either module's `internal/shared/semconv.go`.
- [ ] 9.4 Run `go build ./... && go test -v -race ./... && golangci-lint run ./...` in all four top-level modules. **`go build` and `golangci-lint`: 0 issues, all 4 modules.** `go test`: green in `otel-nats/` (60 tests) and `otel-gorilla-ws/` (46 tests). `otel-mongo/` and `otel-mongo/v2/` test suites (`TestCollectionUpdateByID_InjectTrace`, `TestCollectionDeleteOneByID`, etc.) fail with `server selection error ... ReplicaSetNoPrimary` from the `testcontainers-go/modules/mongodb` replica-set container — reproduced identically across 4 attempts (2 parallel, 2 serial-retry), in both independent mongo modules, with ample Docker host resources (15.6GB RAM ~idle). This is a pre-existing Docker/testcontainers environment flake, not caused by the semconv import-path swap (a no-op text change; `go build` succeeds in both modules). Blocking — needs a working Docker/testcontainers environment to verify green, or a deeper look at the mongodb module's replica-set-ready wait logic.
- [x] 9.5 Re-run the `internal/direct/` no-OTel-SDK-imports grep in `otel-mongo/` and `otel-mongo/v2/` — zero matches in both (this check doesn't require Docker).

## 10. Jetstream wrapper full-parity follow-up (revises §6.4 decision)

Extends the §6 parity audit: rather than keep `Stream`/`ConsumeContext` as curated subsets behind `Unwrap()`, bring them to full upstream parity and drop those escape hatches. `JetStream.Unwrap()` stays (KeyValueManager/ObjectStoreManager out of scope). See design.md Parity Audit Record and the `nats-jetstream-tracing` spec delta.

- [x] 10.1 `ConsumeContext`: add `Drain()` and `Closed() <-chan struct{}` passthroughs; remove `Unwrap()`. Full `jetstream.ConsumeContext` mirror.
- [x] 10.2 `Stream`: add message-management passthroughs (`GetMsg`/`GetLastMsgForSubject`/`DeleteMsg`/`SecureDeleteMsg`/`Purge`) and consumer-admin passthroughs (`PauseConsumer`/`ResumeConsumer`/`UnpinConsumer`/`ResetConsumer`/`ResetConsumerToSequence`) on both `directStream` and `tracedStream`; remove `Unwrap()`. Add `type X = jetstream.X` aliases (`RawStreamMsg`/`GetMsgOpt`/`StreamPurgeOpt`/`ConsumerPauseResponse`/`ConsumerResetResponse`).
- [x] 10.3 Fix `Consumer.Next` to return the local receive-span context (matching `Messages().Next`/`Consume`) instead of the raw extracted producer context.
- [x] 10.4 Update `TestUnwrapEscapeHatch` — exercise `ConsumeContext.Drain()`/`Closed()` and `Stream.CachedInfo()` directly in place of the removed `Unwrap()` calls; `js.Unwrap()` assertion retained.
- [x] 10.5 Run `go build ./... && go test -race ./... && golangci-lint run ./...` in `otel-nats/` — all green (60 tests, 0 lint issues).
- [x] 10.6 Update spec artifacts: `nats-jetstream-tracing` delta (MODIFIED unsupported-surface + ADDED ConsumeContext/Stream/Next requirements), `wrapper-api-parity` delta scenarios, design.md Parity Audit Record, proposal.md.
