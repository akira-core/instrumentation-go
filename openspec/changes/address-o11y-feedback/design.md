# Design: address-o11y-feedback

## Context

Downstream consumer `flywindy/o11y` audited otel-nats v0.6.0 and published a 12-item tracker (their PR #75, `docs/upstream-otel-nats.md`); every claim was verified against this repo on 2026-07-10. The proposal scopes 8 items in: four otel-nats consumer-path/propagation bugs (F1–F4), the batch receive-span lifecycle change (R5), per-connection tracing options across all four modules (R1), and release hygiene (D1 CHANGELOGs + versioning policy, D3 CI tag guard).

Current state (all verified against v0.6.0 = current `main`):

- `otelnats.HeaderCarrier` (`otelnats/propagation.go`) implements only `Get`/`Set`/`Keys` — exact-case lookup, no `Values`, no canonical fallback. `nats.Header` preserves case (unlike `http.Header`), so producers that canonicalize header keys strand their trace context.
- JetStream consumer spans attach the literal `messaging.consumer.name` (`oteljetstream/consumer.go:89`, plus the ordered-consumer fallback in `jetstream.go:117-122`) — not a semconv v1.39.0 key.
- `tracedConsumer.Next` (`oteljetstream/consumer_traced.go:45-46`) converts a ctx deadline to `FetchMaxWait` and then calls the native blocking `Next`; live cancellation of a deadline-less ctx is ignored.
- Batch forwarding goroutines (`oteljetstream/consumer.go:155-158` direct, `180, 201-204` traced) `range` over `raw.Messages()` and select `done` only around the send — `Stop()` is not observed while parked on the receive.
- Batch and `MessagesContext.Next` receive spans use the `lastSpan` pattern (message N's span ends when N+1 is read); single-shot `Next` already ends its span at return, so the three consume paths disagree.
- Tracing is gated solely by two process-global env vars latched via `sync.Once` (`internal/flags.Gate`); v0.6.0 removed the exported `ResetGatesForTest()`, so downstreams cannot influence gating programmatically at all.
- No module ships a `CHANGELOG.md` (root cause: v0.5.x was tagged on a side branch; `main` never carried one); no written semver policy; CI has no tag↔version-constant consistency check (v0.5.0 actually shipped a stale constant).

Constraints: `internal/flags` is copied into all four modules and must stay byte-identical; otel-mongo v1↔v2 parity rule; `internal/direct/` must stay free of OTel SDK imports (CI-enforced); existing lint/test gates per module.

Upstream facts verified for this design (nats.go v1.50.0, otel v1.44.0):
- `jetstream.Consumer.Next(opts ...FetchOpt)` has no ctx parameter; it is literally `Fetch(1, opts...)` + a blocking receive on `res.Messages()`.
- Native `jetstream.MessageBatch` (`{ Messages() <-chan Msg; Error() error }`) has **no** `Stop()` method — corrected during implementation; the earlier belief that it did was a misread of `MessagesContext`/`ConsumeContext`'s `Stop()` in the same file. `jetstream.FetchContext(ctx)` is the actual cancellation mechanism: a `FetchOpt` honored by `Fetch`'s internal goroutine and mutually exclusive with a caller-supplied `FetchMaxWait`.
- semconv v1.39.0 ships the generated key `semconv.MessagingConsumerGroupNameKey` = `"messaging.consumer.group.name"` and the `MessagingConsumerGroupName(...)` helper.

## Goals / Non-Goals

**Goals:**

- Fix the four verified consumer-path/propagation bugs in otel-nats with tests that pin the fixed behavior.
- Make receive-span lifecycle identical across all three JetStream consume paths (end at handover).
- Add a per-connection/per-client tracing override option to all four modules without touching `internal/flags` and without changing env-default behavior for callers that don't use the option.
- Ship release-hygiene guarantees: per-module CHANGELOG in the module zip, a written 0.x semver policy, and a CI guard that a release tag matches the module's version constant.
- Preserve every existing invariant: default-OFF posture, disabled-mode "no OTel SDK code path" rule, v1↔v2 parity, byte-identical `flags` copies.

**Non-Goals:**

- R2 (`RequestWithTimeout`) — rejected on API-mirror grounds; no code or doc change.
- R3 (deliver-span opt-in/sampler redesign) — deferred to the upstream design discussion; `initNATSProvider`/`initMongoProvider` are not touched.
- R4 (reply-attribute hook) — deferred to issue-first API discussion.
- D2 (old-namespace retention) — account-level action, not code.
- Migrating cached-gate wrappers to the strategy-split pattern (tracked separately in CLAUDE.md as planned work).

## Decisions

### 1. F1 — carrier semantics mirror the downstream's proven implementation

`HeaderCarrier` gains:

- `Values(key) []string` implementing `propagation.ValuesGetter`: return values for the verbatim key if present, else for `textproto.CanonicalMIMEHeaderKey(key)`.
- `Get` gains the same verbatim-first / canonical-fallback order.
- `Set` unchanged — we keep writing verbatim W3C lowercase keys; the fallback is read-side only, so current producers see zero behavior change.
- Add `var _ propagation.ValuesGetter = HeaderCarrier{}` compile-time assertion next to the existing `TextMapCarrier` one.

*Why this shape:* it is exactly the semantics o11y already runs in production (`nats/middleware.go`), so we inherit a battle-tested contract, and their facade shrinks to a delegate afterward. *Alternative considered:* canonicalize on write instead — rejected: changes on-wire bytes for every current producer and still leaves durable-stream history unreadable.

### 2. F2 — adopt the generated semconv key, both attachment sites

Replace the `attrConsumerName` literal with `semconv.MessagingConsumerGroupNameKey` (generated, verified present in v1.39.0) at both sites: per-message consumer spans (`consumer.go`) and the ordered-consumer fallback (`jetstream.go`). JetStream durable consumer ≙ consumer group (multiple instances pulling from one durable), which is the semconv meaning of the key.

*Alternative considered:* library-owned `nats.consumer.name` — rejected per the user's decision: staying inside semconv beats inventing a namespace, and the group semantics genuinely fit. **Breaking:** dashboards keyed on the old string must migrate; CHANGELOG and release notes carry an old→new table.

### 3. F3 — wire ctx via `jetstream.FetchContext`, not a manual `Stop()`

**Correction made during implementation:** the original plan below assumed `jetstream.MessageBatch` (the type `Fetch` returns) exposes `Stop()`. It does not — `Stop()` belongs to `MessagesContext` and `ConsumeContext`, a misread of an earlier grep match during design. `jetstream.MessageBatch` is `{ Messages() <-chan Msg; Error() error }` only.

The actual upstream mechanism is simpler and already correct: `jetstream.FetchContext(ctx)` is a `FetchOpt` that (a) derives the server-side expiry from `ctx`'s own deadline, with its own 90%-of-remaining/1s-cap buffer logic — strictly better than the manual `time.Until(dl)` conversion this package used before — and (b) is honored by `Fetch`'s internal goroutine, which selects on `ctx.Done()` alongside message arrival and the server timeout, closing `res.Messages()` and setting `res.Error()` to `ctx.Err()` when it fires. Native `Next` is exactly `Fetch(1, opts...)` + `<-res.Messages()`, so passing `FetchContext(ctx)` through to `c.c.Next(opts...)` gets full cancellation and deadline handling for free — no manual `Stop()`, drain, or `Nak()`, and no wrapper goroutine.

`applyCtxDeadlineToFetchOpts` (deadline→`FetchMaxWait`) is replaced by `applyCtxToFetchOpts` (ctx→`FetchContext`), used identically by both `directConsumer.Next` and `tracedConsumer.Next`:

```go
func applyCtxToFetchOpts(ctx context.Context, opts []jetstream.FetchOpt) ([]jetstream.FetchOpt, error) {
	if ctx == nil || ctx.Done() == nil {   // Done()==nil: context.Background()/TODO() — can never fire
		return opts, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err                     // fail fast, no round trip
	}
	out := make([]jetstream.FetchOpt, 0, len(opts)+1)
	out = append(out, opts...)
	out = append(out, jetstream.FetchContext(ctx))  // appended LAST: method ctx is authoritative
	return out, nil
}
```

The wrapper's `FetchContext` is appended **after** the caller's opts (post-review correction — the first implementation prepended it): jetstream applies fetch options in order and `FetchContext` overwrites the request ctx, so appending last prevents a caller-supplied `FetchContext(otherCtx)` from silently shadowing `Next(ctx)`'s cancellation.

The `ctx.Done() == nil` guard (not just `ctx == nil`) is required, not cosmetic: `context.Background()` is non-nil but its `Done()` is nil, and an existing test (`TestOrderedConsumerNamePrefixAttr`) passes `context.Background()` alongside an explicit `jetstream.FetchMaxWait` — combining `FetchContext` with a caller-supplied `FetchMaxWait` is a native `ErrInvalidOption` (mutual exclusion, checked inside `Fetch`/`FetchBytes` regardless of opt order). Keying off `Done() == nil` restores exact prior behavior for inert contexts while still wiring every genuinely cancelable ctx (`WithCancel`/`WithTimeout`/`WithDeadline`).

*Alternatives considered:* (a) an upstream ctx-aware `FetchOpt` beyond `FetchContext` — unnecessary, `FetchContext` already is one; (b) goroutine + select around native `Next` with manual cleanup — the leak/race-handling problem this design originally (incorrectly) tried to solve doesn't exist once `FetchContext` is used directly, since upstream's own goroutine already owns that lifecycle.

### 4. F4 — nested select in both batch forwarders

Rewrite the forwarding loops in `newDirectMessageBatch` and `newTracedMessageBatch` from `for msg := range raw.Messages()` to an explicit two-level select so `done` is observed on the receive side too:

```go
for {
    select {
    case msg, ok := <-raw.Messages():
        if !ok { return }
        select { case ch <- wrap(msg): case <-done: return }
    case <-done:
        return
    }
}
```

Mirrors the fix o11y already proved in their facade (`wrapMessageBatch`). Test: `Stop()` unblocks a batch parked on an empty stream promptly (bounded wait, no message required).

### 5. R5 — end receive spans at handover; delete the `lastSpan` bookkeeping

- Traced batch forwarder: start and end the message's span **before** the send into `ch`. Ending after a successful send is not an option: an unbuffered channel rendezvous lets the receiver run concurrently with the sender, so the receiver could observe `IsRecording() == true` before the sender's `End()` executes — violating the spec's ended-at-delivery contract. The trade-off is accepted that a span may be emitted for one final message that `Stop()` prevents from delivering. The `lastSpan` variable and its deferred cleanup disappear.
- `tracedMessagesContext.Next`: end the span before returning, exactly like single-shot `Consumer.Next` does today (same rationale, same doc comment pattern: an ended span's SpanContext still parents children correctly).
- The span now measures receive→handover only; processing time belongs to caller-owned child spans.

*Why:* three consume paths converge on one lifecycle; `SetAttributes` on a delivered message's span stops being a silent race. **Breaking (semantics):** receive-span durations shrink; release notes flag the baseline shift. The `mu` guarding `lastSpan` in `tracedMessagesContext` becomes unnecessary for span lifecycle — remove only if nothing else needs it.

### 6. R1 — `WithTracingEnabled(bool)` option, resolved at construction, authoritative when present

**Name:** `WithTracingEnabled(v bool)` — follows the existing `WithTracePropagationEnabled(v bool)` naming precedent in otelmongo.

**Semantics:** a functional option on every wrapper constructor. Tri-state by presence:

- Option absent → today's behavior, bit-for-bit: env gates via `flags` decide (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_<MODULE>_TRACING_ENABLED`, default off).
- Option present → its value is **authoritative for that connection/client**, overriding both env gates in either direction.

Resolution happens once at construction (where the cached-gate pattern already reads env), producing the same `tracingEnabled bool` the wrappers already branch on — so the disabled-mode invariant (noop tracer, no SDK code paths, no deliver-provider init) is inherited unchanged; `initNATSProvider` stays behind the resolved value.

**Per module:**

- `otelnats`: new `Option` in `conn.go` alongside `WithTracerProvider`; all connect variants (`ConnectWithOptions`, TLS, credentials) funnel through the same option application. `oteljetstream` inherits via the wrapped `Conn`.
- `otelmongo` v1 **and** v2 (parity rule): new `ClientOption` in `client.go`; the resolved value feeds the existing construction-time decisions — noop-TP substitution, deliver wiring, and the strategy-split direct/traced impl selection for Collection/Cursor/ChangeStream. `internal/{direct,traced,shared}` interfaces unchanged (the split already keys off a construction-time boolean).
- `otelgorillaws`: new `Option` in `options.go`; applies to `NewConn`, `Dial`, and `Upgrader.Upgrade` paths.

**`internal/flags` is not touched.** The override composes at the wrapper layer: `enabled := opt.tracingEnabled if set, else gate.Enabled()`. No new exported reset hooks; downstream tests simply pass the option.

*Why authoritative-over-both-gates (the maintainer-relevant trade-off):* the downstream's concrete unlock is "derive tracing from the SDK's own toggle instead of exporting two env vars in every deployment" — a module-gate-only override would still require the global master env var everywhere, leaving the pain in place. The cost is that `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` stops being a kill switch *for connections constructed with the option* — documented explicitly on the option: apps that adopt `WithTracingEnabled` own their toggle. *Alternative considered:* distinguishing "env unset" from "env explicitly off" so an explicit off could still veto the option — rejected: requires changing `flags.EnvEnabled` semantics across four byte-identical copies for a marginal safety story.

### 7. D3 — release-tag guard as a standalone workflow with a version-location map

New workflow `.github/workflows/release-guard.yml`, triggered on `push: tags: ['otel-*/v*']`. A single script:

1. Parses module path and version from the tag (`otel-nats/v0.7.0` → module `otel-nats`, version `0.7.0`).
2. Extracts the module's version constant from a hard-coded location map — `otel-nats/otelnats/conn.go` (`instrumentationVersion`), `otel-mongo/otelmongo/version.go`, `otel-mongo/v2/version.go`, `otel-gorilla-ws/version.go` (`Version()` return literal).
3. Fails the run on mismatch.

*Why grep-based over `go test`/build-info:* the constants are plain literals in four different shapes; a location map + regex is the entire problem, needs no toolchain, and the workflow failure message can print both values. `debug.ReadBuildInfo()` (the downstream's other suggestion) was rejected — it changes runtime behavior and reports `(devel)` in tests.

### 8. D1 — Keep-a-Changelog per module + repo-level VERSIONING.md

- `CHANGELOG.md` inside each of the four module directories (that's what ships in the module zip). Backfilled with a `0.6.0` entry summarizing that release; a pointer line notes that pre-0.6.0 history lived on the legacy branch line (root cause documented, not hidden).
- `VERSIONING.md` at repo root: tag format `<module>/v<x.y.z>`, per-module independent versioning, 0.x policy (breaking change → minor bump; additive/fix → patch), where release notes live (module CHANGELOG + GitHub Releases), and the version-constant locations (doubles as documentation for the D3 guard).
- This change itself lands the `0.7.0` CHANGELOG entries (F1–F4, R5, R1 for otel-nats; R1 for the other three).

### 9. Release vehicle and version bumps

- otel-nats → **0.7.0** (breaking: F2 key, R5 span semantics; features: F1, R1; fixes: F3, F4).
- otel-mongo v1, otel-mongo v2, otel-gorilla-ws → **0.7.0** each (feature: R1; aligned number keeps the four-module story simple).
- Version constants bumped in the same PR as the code; tags pushed after merge; the new D3 guard validates its own first release.

## Risks / Trade-offs

- **[F2] Downstream dashboards/queries keyed on `messaging.consumer.name` break silently** → old→new key table in CHANGELOG, release notes, and the umbrella-issue reply; the key change is the headline breaking item of 0.7.0.
- **[R5] Span-duration baselines shift (receive spans get shorter)** → called out in release notes; semantically the new value is the honest one (receive, not receive+idle).
- **[R1] Option-holders opt out of the env kill switch** → explicit doc on the option; recommended pattern stated (SDKs: wire your own toggle; apps: prefer env gates unless you need per-conn control).
- **[F3] Cancel/deliver race** → owned entirely by upstream's `FetchContext` implementation (its goroutine selects message-arrival vs. `ctx.Done()` in one `select`, same as any direct `jetstream.FetchContext` user); no wrapper-side race to manage since there is no wrapper goroutine on this path.
- **[R1 mongo] Threading the resolved flag through Client→Database→Collection construction touches the v1 and v2 trees** → parity checklist in tasks; both modules' full lint+test gates run; no `internal/` interface changes needed keeps the diff mechanical.
- **[Scope] Preempting o11y's announced Bundle A contribution** → deliberate user decision (2026-07-10); umbrella-issue reply credits their tracker and re-scopes remaining discussion to R2/R3/R4/D2.
- **[D3] Guard regexes rot if version constants move** → `VERSIONING.md` documents the locations; moving a constant without updating the map fails the next release loudly (fail-closed, which is the point).

## Migration Plan

1. otel-nats: F1 → F4 → F3 → R5 → F2 (mechanical-first, breaking-last), tests alongside each.
2. otel-nats R1 option; oteljetstream inherits.
3. otel-mongo v1 + v2 R1 (one commit per module, parity-reviewed together); otel-gorilla-ws R1.
4. D1 docs + D3 workflow.
5. Version constants → 0.7.0 ×4; full gate (`go build` / `go test -race` / `golangci-lint`) per module; mongo `internal/direct` import-check job must stay green.
6. Merge, tag `otel-nats/v0.7.0` first (guard validates), then the other three tags; publish GitHub Releases with migration notes.
7. Downstream: when the o11y umbrella issue arrives, reply per `UPSTREAM-ENGAGEMENT-NOTES.md` — F1–F4/R5/R1 shipped in 0.7.0, R2 declined (mirror philosophy), R3/R4 open for design discussion, D2 committed.

Rollback: pre-tag, revert commits normally. Post-tag, a bad release gets a follow-up patch tag (0.7.1); Go module proxy makes yanking impractical, and `retract` directives are available as a last resort.

## Open Questions

- Resolved during implementation: F3's cancellation shape simplified to `jetstream.FetchContext` (see Decision 3's correction note) once `MessageBatch.Stop()` turned out not to exist upstream — no drain/Nak logic was needed. `tracedMessagesContext.mu`/`lastSpan`/`stopped` are removed entirely under R5 (Decision 5) — ending each span before `Next` returns eliminates the cross-call shared state the mutex protected.
