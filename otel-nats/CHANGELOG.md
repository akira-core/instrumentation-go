# Changelog

All notable changes to the `otel-nats` module (`otelnats` + `oteljetstream`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-nats/vX.Y.Z`) and predates the module's rename from `Marz32onE/instrumentation-go` — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.9.1] - 2026-08-12

Follow-up to `0.9.0`, closing the two span-naming gaps its own review found. Both are
fixes to `0.9.0` behaviour; no API is removed and no span is renamed except the ones
`0.9.0` had already failed to bound.

### Fixed

- **JetStream span names are now bounded when a stream captures inbox subjects.** All
  five JetStream resolve sites — `PublishMsg`, `Consumer.Next`, the `Consume` delivery
  handler, the `MessagesContext` iterator and the `Fetch` batch forwarder — passed no
  inbox prefixes, on the assumption that a JetStream subject is never an inbox. NATS
  does not enforce that: a stream over `_INBOX.>` is legal and is how
  request/reply-over-JetStream deployments make replies durable. An unfiltered consumer
  over such a stream resolved its destination to the concrete per-request subject, so
  every span was named `receive _INBOX.<nuid>` — the same unbounded-name defect `0.9.0`
  fixed for core NATS. Those spans are now named `receive` / `process` / `publish` and
  carry `messaging.destination.temporary`, `.anonymous` and
  `messaging.message.conversation_id`, matching the core NATS paths. Span-metrics
  pipelines keyed on span name will see the cardinality drop.
- **A request addressed AT an inbox keeps its span-start `conversation_id`.** In the
  callback-style RPC where a peer advertises its own inbox, `startRequestSpan` recorded
  `messaging.message.conversation_id` = the peer's inbox, and `recordReply` then
  overwrote it with this request's own reply inbox. The attribute held two values during
  the span's life and exported the one nothing had observed at start. The late write is
  now suppressed when the destination is an inbox; the nested conversation stays recorded
  on the reply-`receive` span, which is the span the reply message belongs to. Ordinary
  requests to a non-inbox subject are unaffected.

### Changed

- **A consumer filtered on an inbox prefix plus wildcards keeps its destination in the
  span name.** `_INBOX.>` is a fixed low-cardinality string the subscriber declared, so
  semconv's first choice for `{destination}` — use `messaging.destination.template` when
  available — applies unchanged; the temporary/anonymous exclusion is written into the
  second choice (`messaging.destination.name`) only. Such spans are named
  `receive _INBOX.>` and carry `messaging.destination.template`. A filter carrying a
  literal token (`_INBOX.<nuid>.>`, the `<inbox>.>` subscription shape) is still
  unbounded and still drops the destination.
- Removed three unreachable `messaging.destination.template` branches on the publish and
  request paths. `spanname.Resolve` cannot return a template for a call with no filter
  subject, so those spans never carried the attribute — the dead code implied otherwise.

### Added

- `otelnats.Conn.InboxPrefixes()` — the connection's recognised inbox prefixes, needed by
  `oteljetstream` to apply the same inbox test. Additive; existing callers are unaffected.

### Known limits

Two sources of unbounded span names remain, both outside what the library can see:

- A peer using a **custom inbox prefix this connection does not share** is not recognised
  when only its concrete subject is available. `recordReply` is unaffected (it knows
  structurally that it holds an inbox), and any fixed subscription or consumer filter is
  unaffected (a declared filter is bounded whatever its prefix).
- **Subjects embedding identifiers** (`orders.12345.created`) on paths with no filter.
  semconv permits recording a known `messaging.destination.template`, not inferring one.

Both are handled collector-side with the `span` processor's `name.to_attributes` rules —
see the "Residual span-name cardinality" section of `otel-nats/README.md`.

## [0.9.0] - 2026-08-11

### Breaking

Span names now follow the OTel messaging semconv v1.39.0 format `{messaging.operation.name} {destination}`. Migrate any dashboard, alert, or span-metrics query keyed on the old span names:

| Old name | New name |
|---|---|
| `send {subject}` (publish, core NATS and JetStream) | `publish {subject}` |
| `{subject} request` | `request {subject}` |
| `receive {inbox}` (reply-receive) | `receive` (bare, no destination) |
| `publish {inbox}` (manual reply via `conn.Publish(msg.Reply, …)`) | `publish` (bare, no destination) |
| `process {inbox}` (handler on an inbox subscription) | `process` (bare, no destination) |

- Publish spans (`Publish`/`PublishMsg`, core NATS and JetStream) are now named `publish {subject}` — the span already carried `messaging.operation.name=publish`; only the name was wrong.
- Request spans (`Request`/`RequestWithContext`/`RequestMsg`/`RequestMsgWithContext`) are now named `request {subject}`, operation-first, replacing the destination-first `{subject} request`.
- **Any** core-NATS span whose resolved destination is a request/reply inbox now drops the destination segment from its name, not just the reply-receive span. That covers all three halves of a manual exchange: the reply-receive span (`receive`), a reply published with `conn.Publish(msg.Reply, data)` (`publish`), and a handler on an inbox subscription (`process`). The inbox subject is auto-generated and single-use, and semconv v1.39.0 directs omitting `{destination}` when no low-cardinality value exists. The inbox stays queryable on every one of those spans via `messaging.destination.name`, `messaging.message.conversation_id`, `messaging.destination.temporary=true` and `messaging.destination.anonymous=true`.
- JetStream consumer `receive`/`process` spans for a wildcard-filter consumer (`orders.*`, `orders.>`) are now named after the filter subject (e.g. `receive orders.*`) instead of the concrete delivered subject, bounding cardinality for wildcard consumers. A consumer with an exact filter subject, multiple filter subjects, or an unobservable filter configuration is unaffected (or falls back to the concrete subject, unchanged from `0.8.0`).

### Added

- JetStream consumer receive/process spans resolve their span-name destination from the consumer's filter subject when it is single-valued: an exact filter keeps the existing concrete name, and a filter containing a wildcard token (`*` or `>`) is used as the span name and additionally recorded as `messaging.destination.template`. A consumer with more than one filter subject, or whose filter configuration is not observable to the wrapper, falls back to the concrete delivered subject.
- `messaging.destination.template` — set on any span whose resolved span-name destination differs from the concrete message subject (a wildcard subscription or consumer filter). `messaging.destination.name` continues to carry the concrete subject on every span, regardless of what the name uses.
- `messaging.destination.temporary` / `messaging.destination.anonymous` / `messaging.message.conversation_id` — set on every span whose destination is a reply inbox, not only the reply-receive span.
- Inbox detection recognises **two** subject prefixes: this connection's own (`nats.CustomInboxPrefix(p)` ⇒ `p + "."`) and the default `_INBOX.` unconditionally. Recognising only the local prefix would fail precisely where custom prefixes are deployed — a responder sees the *requester's* inbox in `msg.Reply`, and the requester is the side that customises, since granting it `subscribe: _INBOX.>` would hand it every other client's replies while a responder needs no inbox permission at all. Two peers on two *different* custom prefixes do not recognise each other's inboxes; a collector-side span rename covers that.
- Shared span-name and destination-resolution logic lives in a new internal package, `otel-nats/internal/spanname`, used by both `otelnats` and `oteljetstream` — no per-package drift in the filter-then-concrete precedence or the inbox test.

### Not included

No option is provided to template subjects that embed identifiers (`orders.12345.created`). No library can tell which token of a NATS subject is an identifier, and semconv forbids instrumentation from inferring `messaging.destination.template` — it may only record one that is already known, which for this module means a subscription subject or a consumer filter subject. Publish and request spans (which have no subscription to resolve against) and JetStream consumers with no filter or several wildcard filters therefore keep the concrete subject in the span name. Rewrite those downstream, where the pattern is known:

```yaml
span/to_attributes:
  name:
    to_attributes:
      rules:
        - ^receive orders\.(?P<orderId>[^.]+)\.created$
```

## [0.8.0] - 2026-08-07

> **Every `0.8.0-rc.*` tag is superseded by this release.** `rc.1` and `rc.2` both
> `require otel-flags v0.1.0`, whose API this module's code no longer matches, so neither builds for
> a consumer at all; `rc.2` additionally carries a version constant reading `0.8.0-rc.1`. `rc.3`
> builds and is functionally close to this release, but predates the final documentation and the
> measured resolution cost.

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
- The relay is resolved on **every operation** and nothing is cached, so a flag change takes effect on the next one. An instrumented operation now makes two evaluations. A process with **no relay configured** pays none of it, allocates no instrumented implementation it cannot reach, and measures 0 allocations per operation — the pre-dynamic zero-cost path is preserved exactly. **The cost is paid whatever the flag's value**: a relay-reachable connection whose module flag is `false` still evaluates on every operation. Measured against a real GO Feature Flag relay provider on a 2-vCPU Xeon, one evaluation costs roughly **12 µs and 23 allocations** — an order of magnitude above the `2 µs / 336 B / 7 allocations` earlier drafts of the guide quoted, which was never reproducible and is withdrawn. The benchmark lives in [`akira-core/instrumentation-demo`](https://github.com/akira-core/instrumentation-demo/tree/main/backend/internal/flagperf); the full table and its caveats are in [`docs/feature-flags.md`](../docs/feature-flags.md).
- **New ordering requirement.** Whether a relay can exist is resolved once, when a wrapper is constructed. An application that installs its own OpenFeature provider must do so **before** constructing any wrapper; one built earlier resolves statically for its whole life and never consults the relay. Applications using the zero-code path (`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`) are unaffected.
- The non-blocking startup window is **fail-safe for enabling and deliberately not for disabling**: until the provider's first fetch every switch resolves to its local value, so the window can delay a relay-driven enable but can never introduce one — and equally, **a relay `false` does not survive a restart**. A process whose environment enables this module traces again from start-up until its first fetch, and indefinitely while the relay is unreachable. Reading not-ready as `false` is refused: it applies per key and the master's local default is `true`, so every restart of every relay-configured process would be fully vetoed, making the control plane an availability dependency. The relay is runtime control; durable state belongs in the environment variable. Treat a relay flip as the first half of an incident brake and land the variable in the deployment before anything restarts.

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
