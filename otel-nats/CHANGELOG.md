# Changelog

All notable changes to the `otel-nats` module (`otelnats` + `oteljetstream`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-nats/vX.Y.Z`) and predates the module's rename from `Marz32onE/instrumentation-go` — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.8.0] - 2026-07-31

### Changed

- **BREAKING** The module's tracing environment variable is demoted from *final say* to the **default value** used when the relay proxy has no opinion. A relay flag can now turn this module on when the environment variable leaves it off, and off when the environment variable turns it on. Operators who relied on setting it to `false` as a hard guarantee must use `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` (whole process) or `WithTracingEnabled(false)` (one connection) instead — neither can be crossed by the relay.
- **BREAKING** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` alone now decides whether the instrumented or the passthrough implementation is constructed. A process running with the global switch on and this module's switch off previously took the zero-cost passthrough path; it now allocates the instrumented wrapper and performs one atomic load plus one monotonic clock read per operation. It still emits no spans.

### Added

- Tracing is resolved at runtime through [OpenFeature](https://openfeature.dev) instead of once at process start, so an operator can turn it on or off through a GO Feature Flag relay proxy without restarting the application. Values are cached in a per-module snapshot with a fixed one-second TTL; hot paths never enter the OpenFeature evaluation pipeline.
- Applications opt in by installing a provider at startup — the library never calls `openfeature.SetProvider`, exactly as it never initializes a `TracerProvider`:

  ```go
  provider, _ := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{Endpoint: "http://relay:1031"})
  _ = openfeature.SetProviderAndWait(provider)
  ```

  With no provider installed, behavior is identical to the previous release.
- `github.com/open-feature/go-sdk` is a new dependency. The GO Feature Flag provider is an application-side dependency, not a library one.

### Flag keys

| OpenFeature key | Fallback environment variable |
|---|---|
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` |

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` has **no** relay counterpart: it is an out-of-band kill switch that works when the relay is unreachable or misconfigured.

## [0.7.0] - 2026-07-15

### Fixed

- `HeaderCarrier` implements `propagation.ValuesGetter` and falls back to the MIME-canonical header form on read (verbatim key first), fixing baggage-header truncation and trace loss on canonicalized or durable-stream-persisted messages. The fallback triggers on key absence, not value emptiness — a verbatim key present with an empty value wins over a canonical entry. `Set` is unchanged (writes remain verbatim).
- `Consumer.Next` now honors live context cancellation via `jetstream.FetchContext` instead of only converting a ctx deadline to `FetchMaxWait` — a deadline-less canceled ctx now aborts promptly instead of blocking for the ~30s default max wait. The wrapper's `FetchContext` is applied after all caller options, so a caller-supplied `jetstream.FetchContext(otherCtx)` cannot shadow `Next(ctx)`'s cancellation.
- `ConnectTLS` and `ConnectWithCredentials` no longer panic on every successful connection (a stray nil trace option reached the option applier); nil `Option` values are now skipped everywhere.
- `MessageBatch.Stop()` now takes effect promptly even while the forwarding goroutine is parked waiting to receive from the native batch (previously only observed while blocked sending to the wrapper channel).
- `Consume(nil)` and other nil-handler paths continue to surface upstream's `ErrHandlerRequired` rather than panicking (carried from 0.6.0).
- Request/reply "send" (CLIENT) spans no longer have their `messaging.message.body.size` overwritten with the reply payload size after the round trip — the attribute now always reports the request payload size. The reply size is unchanged and lives on the reply "receive" span, where it already was.

### Changed — BREAKING

- JetStream consumer spans now attach the consumer/durable name under the semconv v1.39.0 key `messaging.consumer.group.name` instead of the non-semconv literal `messaging.consumer.name`. Update any dashboards/queries keyed on the old attribute.
- `Consumer.Next` with a **cancelable** ctx (`WithCancel`/`WithTimeout`/`WithDeadline`) can no longer be combined with a caller-supplied `jetstream.FetchMaxWait` opt: upstream rejects `FetchContext` + `FetchMaxWait`, so the call now returns `jetstream.ErrInvalidOption` (on 0.6.0 the cancelable-ctx case silently ignored cancellation and used the max wait). **Migration:** use the ctx's own deadline (`context.WithTimeout`) instead of a separate `FetchMaxWait`; `context.Background()` + `FetchMaxWait` keeps working unchanged.
- Batch (`MessageBatch`) and `MessagesContext.Next` receive spans now end **at handover** (the span is already ended by the time the caller observes the message — the batch forwarder ends it just before the channel send) instead of when the next message arrives or the batch closes. Span durations for these paths are shorter and now measure receive-to-handover only; caller-side processing should be measured with the caller's own child spans.
- **Deliver spans removed.** The synthetic "deliver" span pattern (independent OTLP-gated `TracerProvider`, `ConsumerContextWithDeliver`/`deliverTracer`/`deliverAttrs`/`initNATSProvider`, and every call site) is gone. The package no longer reads `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission, and the Grafana service-graph broker node is no longer emitted.
- Span kinds corrected to the OTel spec: reply-receive and JetStream pull-consume (`Consume`/`Fetch`/`Messages`) spans are now `CLIENT` (were `CONSUMER`); `publish` remains `PRODUCER`, push `process` remains `CONSUMER`.
- Pull-receive spans now carry `messaging.operation.type=receive`.

### Added

- `WithTracingEnabled(v bool) Option` overrides the env-gate default (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_NATS_TRACING_ENABLED`) for a single `Conn`, in either direction. Applies to `ConnectWithOptions`, `ConnectTLSWithOptions`, and `ConnectWithCredentialsWithOptions`; `oteljetstream` wrappers built from an option-configured `Conn` inherit its effective tracing state automatically.
- Core-NATS request/reply spans now carry `messaging.message.conversation_id` (the reply inbox subject) so the requester's send span, the requester's reply-receive span, and the responder's process span are all joinable by attribute query, not just by span link. On the send span the attribute is set only once a reply arrives (a late `SetAttributes` call before `End()`) — a request that times out or errors never observes the inbox, so its send span carries no `conversation_id`; this is spec-conformant (the attribute's semconv requirement level is Recommended) but means the value is invisible to samplers, which only see span-start attributes. `oteljetstream` spans are deliberately excluded: a JetStream message's `Reply` field is the native `$JS.ACK.…` acknowledgement subject, not a conversation ID.

## [0.6.0] - 2026-07-08

Highlights for this module:

- Dependencies refreshed: `go.opentelemetry.io/otel` v1.44.0, `nats.go` v1.50.0 (downstream-policy pin), `semconv` v1.39.0 (downstream-policy pin). Go toolchain floor raised to 1.25.
- `oteljetstream.PushConsumer` added (push consumers now wrapped): `PushConsumer`/`CreatePushConsumer`/`CreateOrUpdatePushConsumer`/`UpdatePushConsumer` on `JetStream` and `Stream`.
- `Stream` and `ConsumeContext` fully mirror their `jetstream` counterparts at the nats.go v1.50.0 surface — `Unwrap()` removed from both (breaking for custom implementers only).
- `Consumer.Next`'s returned context now bears the wrapper's local receive span (matching `Messages().Next` and `Consume`) instead of the raw extracted producer context.
- `MessagesContext.Next` gained `opts ...jetstream.NextOpt` (variadic, source-compatible).
- Data race fixed in `tracedMessagesContext`'s in-flight span bookkeeping (superseded by the 0.7.0 end-at-handover rewrite, which removes that bookkeeping entirely).
- Module path renamed from `Marz32onE/instrumentation-go` to `akira-core/instrumentation-go`.
