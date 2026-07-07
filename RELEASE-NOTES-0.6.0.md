# Release 0.6.0 — dependency refresh + JetStream push-consumer support

All four modules (`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) are tagged `0.6.0`. This release brings every upstream dependency to its latest stable version and, for `otel-nats/oteljetstream`, extends the wrapper's public API surface to keep parity with `nats.go` v1.52.0.

## ⚠️ Breaking changes

**1. Go 1.25 toolchain floor (all modules).** The `go` directive moved from `1.24.0` to `1.25.0` in every module. This is forced by `go.opentelemetry.io/otel` v1.42.0+, whose own `go.mod` requires `go 1.25.0`. **You must build with Go ≥ 1.25.**

**2. `oteljetstream` interface extensions (`otel-nats` only).** These break only code that **implements** the wrapper interfaces itself (e.g. custom mocks/decorators). Code that merely calls `oteljetstream.New(...)` and uses the returned value is **unaffected**.

| Interface | Change |
|---|---|
| `JetStream` | added `PushConsumer`, `CreatePushConsumer`, `CreateOrUpdatePushConsumer`, `UpdatePushConsumer`, `Unwrap() jetstream.JetStream` |
| `Stream` | added the same four push methods + `Unwrap() jetstream.Stream` |
| `ConsumeContext` | added `Unwrap() jetstream.ConsumeContext` (was `Stop()`-only) |
| `MessagesContext` | `Next()` → `Next(opts ...jetstream.NextOpt)` — variadic, so existing `iter.Next()` call sites still compile; only implementers must update the signature |

Permitted under pre-1.0 (`0.x`) versioning: a minor bump may carry breaking changes.

## New — `otel-nats/oteljetstream`

- **Push consumers are now wrapped.** `PushConsumer` interface + `PushConsumer` / `CreatePushConsumer` / `CreateOrUpdatePushConsumer` / `UpdatePushConsumer` on both `JetStream` and `Stream`. The returned `PushConsumer.Consume` carries and extracts trace context exactly like the pull path.
- **`Unwrap()` escape hatches** on `JetStream`, `Stream`, and `ConsumeContext` — return the raw `jetstream.*` handle to reach upstream APIs the wrapper does not re-expose (e.g. `PauseConsumer` / `ResumeConsumer` / `UnpinConsumer`, `Conn()`, `Options()`, `Closed()`). Calls made through `Unwrap()` bypass tracing (documented).
- **`MessagesContext.Next(opts ...jetstream.NextOpt)`** — mirrors upstream; `jetstream.NextContext` / `jetstream.NextMaxWait` now pass through to the iterator.
- **`AckFlowControlPolicy`** re-exported alongside the existing ack-policy constants.
- **`OrderedConsumerConfig.NamePrefix`** now feeds the `messaging.consumer.name` span attribute (falls back to `ordered-consumer` when unset).

## Fixes — `otel-nats/oteljetstream`

- **Data race fixed** in `tracedMessagesContext`: the in-flight span is now mutex-guarded, matching jetstream's contract that `Stop`/`Drain` may be called from another goroutine to unblock a pending `Next`. Clean under `-race`.
- **Nil handler**: `Consume(nil)` now surfaces upstream's `ErrHandlerRequired` instead of panicking in the delivery goroutine.

## Dependency upgrades

| Dependency | From | To | Modules |
|---|---|---|---|
| `go.opentelemetry.io/otel` (+ `sdk`, `trace`, `metric`, OTLP exporters) | v1.39.0 | v1.44.0 | all 4 |
| `go.mongodb.org/mongo-driver` | v1.17.2 | v1.17.9 | otel-mongo (v1) |
| `go.mongodb.org/mongo-driver/v2` | v2.6.0 | v2.7.0 | otel-mongo/v2 |
| `github.com/nats-io/nats.go` | v1.38.0 | v1.52.0 | otel-nats |
| `github.com/nats-io/nats-server/v2` (test-only) | v2.11.0-preview.2 | v2.14.3 | otel-nats |
| `github.com/testcontainers/testcontainers-go` (test-only) | v0.34.0 / v0.40.0 | v0.43.0 | mongo modules + all `tests/integration/` |

`gorilla/websocket` (v1.5.3), `stretchr/testify` (v1.11.1), `go.opentelemetry.io/auto/sdk` (v1.2.1) were already current — no direct bump.

## Behavior notes

- **`nats.go` v1.48.0** added stricter publish-subject validation (rejects subjects with protocol-breaking characters). If you publish to unusual subjects, verify they pass; opt out with the new `nats.SkipSubjectValidation()` option on the raw connection if needed.
- **Reported instrumentation-scope version** is now `0.6.0` on every span from all four modules (`otel.scope.version` / exporter equivalent). Downstream dashboards keyed on that field will see the new value.
- Public API for callers of the mongo and gorilla-ws wrappers is **unchanged**; those modules are dependency-currency-only.

## Module tags

```
otel-mongo/v0.6.0
otel-mongo/v2/v0.6.0
otel-nats/v0.6.0
otel-gorilla-ws/v0.6.0
```

## Upgrade

```bash
go get github.com/akira-core/instrumentation-go/otel-mongo@otel-mongo/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-mongo/v2@otel-mongo/v2/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-nats@otel-nats/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-gorilla-ws@otel-gorilla-ws/v0.6.0
```
Then ensure your build uses Go ≥ 1.25.
