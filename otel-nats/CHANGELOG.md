# Changelog

All notable changes to the `otel-nats` module (`otelnats` + `oteljetstream`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-nats/vX.Y.Z`) and predates the module's rename from `Marz32onE/instrumentation-go` — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.8.0] - unreleased

> **Release candidates.** `v0.8.0-rc.3` is the first one that carries the `otel-flags` 0.2.0 flag
> layer, and the first that builds for a consumer at all: `rc.1` and `rc.2` predate it and both
> `require otel-flags v0.1.0`, whose API this module's code no longer matches. `rc.2` also carries a
> version constant reading `0.8.0-rc.1`. Test the relay against `rc.3` or later.
>
> What `rc.3` adds over `rc.2`: flag keys passed by name rather than by index; the OpenFeature
> provider installed at construction through `otelflags.ValidateAndInstall`; an unreadable
> `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` or `_POLL_INTERVAL` failing construction rather than
> warning and falling back; evaluation error codes logged once per transition, two-tier, so relay
> silence is distinguishable from relay failure in the log even though it is not in the value; and a
> failed provider install that is retried rather than latched.

### Changed

- **BREAKING** Feature switches now resolve down a **four-step ladder** — `relay > env > option > default`, first source with an opinion winning — and the relay proxy is **authoritative in both directions**: it can turn this module off, and it can turn it on when the deployment left it off. This replaces the revoke-only model that was developed but never released. Safety now comes from the defaults rather than from a restriction on the relay: every per-module switch defaults to **off**, and the process-wide master switch defaults to **on** only because it is a veto rather than an enabler.
- **BREAKING** `OTEL_NATS_TRACING_ENABLED` now takes effect **without** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` also being set. The master switch defaults to enabled, so the module variable alone decides. Previously it was inert unless the global one was set too — a common "I set the flag and nothing happened" report. Deployments that set the module variable expecting it to be ignored will now see this module trace.
- **BREAKING** `WithTracingEnabled` now sits **below** its environment variable, reversing `0.7.0`, where the option won. `OTEL_NATS_TRACING_ENABLED` overrides the option, so a deployment can disable this module even where the application's Go code asked for tracing. With the variable unset the option still decides, so two connections in one process can still differ. Three reasons, in order of weight: the operator gets a per-module setting code cannot override; the rule is uniform across the repository, and the case that forces it is `otel-mongo`'s document propagation, where an option must not be able to bypass an operator's `OTEL_MONGO_PROPAGATION_ENABLED=false` and write permanent fields into their documents; and the ladder stays monotonic in deployment order.
- **BREAKING** The option no longer supplies the process-wide master switch. It supplies this module's tier for one connection or client. A per-connection value cannot coherently spell a process-wide switch, and keeping the master option-free is what guarantees a single environment variable still stops everything.
- **BREAKING** Environment values are now a strict **tri-state**, and an unreadable one **fails construction**. Unset means "no opinion" and falls through to the option, then the default. `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off` (trimmed, case-insensitive) decide. **Everything else — including the empty string, `enabled`, `2`, `y` and `t` — returns an error** wrapping `otelflags.ErrInvalidFlagValue`, naming the variable and the observed value. Under a ladder there is no safe direction to guess in: the master tier defaults to `true` and every other tier to `false`, so a value silently read as `false` would stop a whole fleet on one tier and change nothing on the others. **Before upgrading, grep your deployment configuration for `OTEL_*_ENABLED` and confirm every value is in one of those two lists** — an unexpanded `${SOMETHING}` in a Kubernetes manifest reaches exactly the empty-string case, and this is the one change that can stop a process from starting.
- **BREAKING** A constructor that reads several switches reports **all** the bad values in one joined error, so one run tells you everything to fix.
- **BREAKING** The two relay-connection variables `otel-flags` owns are now validated at construction, and an unreadable one **fails it**: `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` must be a positive Go duration and `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` a URL with a scheme and a host, both checked whether or not a relay is configured. The interval previously warned and fell back to `60s`. Blank still means "not configured" for both, and the API key is never validated. **Before upgrading, grep your deployment configuration for `=60` on the poll interval** — it was never read as seconds and is now rejected outright rather than ignored. Requires `otel-flags v0.2.0`, which also renames `InstallProvider` to `SetNamedProvider` for applications that install their own provider.
- A failing relay is no longer silent. `otel-flags v0.2.0` logs a flag key's OpenFeature error code when it **changes** — `FLAG_NOT_FOUND` and `PROVIDER_NOT_READY` at debug, everything else at warn, a recovery at info — so a provider stuck in `ERROR` or a relay rule that never matches is visible in the application's own logs. The resolved value is unchanged: relay silence and relay failure both still mean "the next rung down decides".
- **BREAKING** This module now requires `github.com/akira-core/instrumentation-go/otel-flags`, which replaces the vendored `internal/flags` package. That module carries the OpenFeature SDK and the GO Feature Flag provider, so their dependency trees enter your build transitively — roughly ten modules including a full ANTLR runtime, a JSONLogic evaluator and a rules engine. The cost lands on `go.sum`, vulnerability-scanning surface and licence review, not on runtime; the linker drops unreached code. It exists to guarantee **exactly one** OpenFeature provider per binary, which four independent vendored copies could not.
- **BREAKING** The master switch gains a relay key, `otel-instrumentation-go-tracing`. Setting it to `true` has **no effect** — that is already the default. Setting it to `false` stops every module in every process the relay serves, including connections whose Go code passed an option. Do not create it expecting an enable; it will read as a broken flag.
- The relay is resolved on **every operation** and nothing is cached, so a flag change takes effect on the next one. An instrumented operation now makes two evaluations at roughly 2 µs and 7 allocations each. A process with **no relay configured** pays none of it and allocates no instrumented implementation it cannot reach — the pre-dynamic zero-cost path is preserved exactly.
- **New ordering requirement.** Whether a relay can exist is resolved once, when a wrapper is constructed. An application that installs its own OpenFeature provider must do so **before** constructing any wrapper; one built earlier resolves statically for its whole life and never consults the relay. Applications using the zero-code path (`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`) are unaffected.
- The non-blocking startup window is now **fail-safe**: until the provider's first fetch every switch resolves to its local value, so the window can delay a relay-driven enable but can never introduce one.

### Removed

- **BREAKING** `ErrTracingConfigConflict` and the mutual-exclusion rule they reported. Supplying an option alongside its paired environment variable is now ordinary configuration with a defined winner (the variable). Roughly 89 in-repo call sites that combined the two become legal again.
- **BREAKING (internal)** The vendored `internal/flags` package, including `Gate`, `NewGate`, `ResetForTest`, `EnvEnabled` and `EnvSet`. `otelflags.Lookup` replaces the last two; the rest have no successor, because nothing is cached and therefore nothing needs re-arming.

### Notes

- **Flag changes are not immediate.** End-to-end latency is the provider's poll interval — 60 s by default — in **both** directions. This module adds none of its own.
- The library still never touches the **default** OpenFeature provider, the global evaluation context, hooks or shutdown. The one piece of state it may write is a **named** provider on `otel-instrumentation-go`, and only when the environment asks for one and the application installed none. `DataCollectorDisabled: true` and in-process evaluation are hardcoded on that path.
- Full reference: [`docs/feature-flags.md`](../docs/feature-flags.md) ([繁體中文](../docs/feature-flags.zh-TW.md)).

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
