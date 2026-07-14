# Proposal: address-o11y-feedback

## Why

Downstream consumer `flywindy/o11y` audited otel-nats v0.6.0 (their PR #75) and published a 12-item upstream tracker; all 12 claims were verified against our code on 2026-07-10 and found accurate. This change implements the 8 items we decided to own directly: four consumer-path/propagation bugs, per-connection tracing options across all four modules, batch receive-span lifecycle alignment, and release hygiene (per-module CHANGELOGs, a written versioning policy, and a CI tag↔version guard). Fixing these keeps a high-quality downstream on the collaboration path (their documented fallback is a hard fork) and removes real production footguns for every consumer.

## What Changes

### otel-nats bug fixes (tracker F1–F4)

- **F1** — `otelnats.HeaderCarrier` implements `propagation.ValuesGetter` (`Values`) and adds a MIME-canonical fallback to `Get`/`Values`: look up the verbatim key first, then `textproto.CanonicalMIMEHeaderKey` form. Fixes silent baggage truncation and trace-link loss on messages written by canonicalizing producers (including messages still sitting in durable streams).
- **F2 — BREAKING (attribute key)** — JetStream consumer spans replace the non-semconv key `messaging.consumer.name` with the semconv v1.39.0 key `messaging.consumer.group.name` (JetStream durable consumer ≙ consumer group). Applies to the ordered-consumer fallback path too. Dashboards/queries keyed on the old attribute must migrate.
- **F3** — `Consumer.Next` honors live context cancellation: wire the ctx into the underlying fetch (`jetstream.FetchContext` semantics), not just a deadline-to-`FetchMaxWait` conversion, handling the `FetchContext`/`FetchMaxWait` mutual exclusion.
- **F4** — batch forwarding goroutines (`newDirectMessageBatch`, `newTracedMessageBatch`) select on `done` on the **receive** side as well as the send side, so `MessageBatch.Stop()` takes effect promptly even while parked on `raw.Messages()`.

### otel-nats span-lifecycle change (tracker R5)

- **BREAKING (span duration semantics)** — batch (`MessageBatch`) and `MessagesContext.Next` receive spans end **at handover** (message delivered to the caller) instead of when the next message is read (`lastSpan` pattern). Aligns all three consume paths with single-shot `Consumer.Next` (already ends immediately) and makes post-receive `SetAttributes` behavior predictable (callers enrich their own child spans).

### Per-connection tracing options, all four modules (tracker R1)

- New functional option (working name `WithTracing(bool)`) on every wrapper constructor — `otelnats.ConnectWithOptions` (+ TLS/credentials variants), `otelmongo.ConnectWithOptions` (v1 **and** v2, parity rule), `otelgorillaws.NewConn`/`Dial`/`Upgrader` — overriding the env-gate default per connection/client. Env gates stay as the process-wide default; default-OFF posture unchanged. Restores downstream testability lost when `ResetGatesForTest()` was removed in v0.6.0.
- `internal/flags` is preferred untouched; if it must change, all four module copies stay byte-identical per the existing rule.

### Release hygiene (tracker D1, D3)

- **D1** — restore module-level `CHANGELOG.md` in all four module directories (so it ships in the module zip), backfilled from v0.6.0; add repo-level `VERSIONING.md` documenting the 0.x semver policy (breaking → minor bump), tag format `<module>/v<x.y.z>`, and where release notes live. Root cause documented: v0.5.x was tagged on a side branch; `main` never carried a CHANGELOG.
- **D3** — new CI job on tag push (`otel-*/v*`) comparing the tag version against the module's `instrumentationVersion` constant (locations differ per module: `otelnats/conn.go`, `otelmongo/version.go`, `v2/version.go`, `otel-gorilla-ws` `Version()` literal); mismatch fails the release.

### Explicitly excluded (decided 2026-07-10, one by one)

- **R2** (`RequestWithTimeout`) — rejected: the wrapper mirrors nats.go's Request family 1:1; timeout is achievable via `context.WithTimeout` + `RequestWithContext`. No code or doc change; rationale goes in the upstream issue reply.
- **R3** (deliver-span opt-in/sampler) — deferred to the upstream design discussion (affects otel-mongo identically; not to be decided unilaterally here).
- **R4** (reply-attribute hook) — deferred to issue-first API discussion.
- **D2** (Marz32onE namespace retention) — account-level action by the owner, not a code change.

## Capabilities

### New Capabilities

- `release-versioning`: release metadata guarantees — per-module CHANGELOG shipped in the module zip, written 0.x semver policy, CI-enforced tag↔version-constant consistency on release tags.

### Modified Capabilities

- `nats-jetstream-tracing`: carrier read semantics (F1), consumer span attribute key (F2), `Next` cancellation contract (F3), `Stop()` promptness (F4), receive-span end-at-handover lifecycle (R5), per-Conn tracing option (R1).
- `shared-feature-flags`: gates become per-connection-overridable defaults — env resolution unchanged, but a constructor option takes precedence for that connection/client.
- `mongodb-tracing`: per-Client `WithTracing` option (v1 and v2) overriding the env default.
- `websocket-tracing`: per-Conn `WithTracing` option overriding the env default.

## Impact

- **Code**: `otel-nats/otelnats/{propagation.go,conn.go,options,env_flags.go}`, `otel-nats/oteljetstream/{consumer.go,consumer_traced.go,jetstream.go}`; `otel-mongo/otelmongo` + `otel-mongo/v2` option plumbing (v1↔v2 parity, both `internal/` trees untouched by R1 if gating stays at Connect); `otel-gorilla-ws` option plumbing; `.github/workflows/` new tag-guard job.
- **API**: additive options everywhere (R1); no signature changes. Attribute key change (F2) and span-duration change (R5) are behavioral breaks → **otel-nats 0.7.0**; other modules get their own minor bump when R1 lands in them.
- **Docs**: 4× `CHANGELOG.md`, `VERSIONING.md`, README gate-documentation updates, release notes must flag F2 (key migration) and R5 (duration semantics).
- **Downstream coordination**: F1–F4 overlap flywindy/o11y's announced Bundle A contribution plan — when their umbrella issue arrives, reply that these land upstream in 0.7.0 (with credit to their tracker), and re-scope the remaining discussion to R2/R3/R4/D2 positions already prepared in `UPSTREAM-ENGAGEMENT-NOTES.md`.
- **Tests**: new coverage required — canonical-header + multi-value baggage (F1), attribute key (F2), mid-wait cancellation (F3), prompt `Stop()` (F4), handover-time span end (R5), option-over-env precedence per module (R1). All existing suites must stay green (`go build` / `go test -race` / `golangci-lint` per module).
