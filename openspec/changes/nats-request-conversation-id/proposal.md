# Proposal: nats-request-conversation-id

## Why

Downstream tracker item **F6** (flywindy/o11y `docs/upstream-otel-nats.md`): the request/reply "send" (CLIENT) span never carries `messaging.message.conversation_id`, because `requestAttrs` only sets it when `msg.Reply != ""` **at span start** — but on the standard `Request`/`RequestWithContext` path nats.go allocates the reply inbox *after* the span starts and never writes it back to the caller's message. The guard is a dead branch on the primary API. Worse, *no* span in the exchange carries the inbox as a correlation key today (`receiveAttrs` omits `conversation_id` entirely), so the requester and responder sides of one exchange cannot be joined by attribute query — span links are the only correlation, and links break under sampling, uninstrumented peers, and most tracing UIs' weak link navigation.

The current `otel-nats-spans` spec text ("Conditional attributes SHALL be set when their value exists: `messaging.message.conversation_id` (reply subject)") describes intent, not reality, and is silent on *which spans* and *when the value becomes observable*. This change fixes the behavior and sharpens the spec so the decision — including what we deliberately do **not** do — is recorded.

## What Changes

- **Request "send" span (core NATS)**: on a successful reply, `recordReply` late-sets `messaging.message.conversation_id` to `reply.Subject` (the reply inbox) on the request span before it ends. OTel permits `SetAttributes` any time before `End()` (same pattern as HTTP `http.response.status_code`). On timeout/error paths the inbox is never observable to the wrapper, so the attribute is omitted — conformant, since the attribute's semconv requirement level is Recommended.
- **Reply-"receive" span (core NATS)**: carries `messaging.message.conversation_id = reply.Subject` (the inbox is the subject the reply arrives on).
- **Subscription "process" span (core NATS)**: `receiveAttrs` sets `messaging.message.conversation_id = msg.Reply` when non-empty — the responder side of a request/reply exchange sees the inbox naturally and now records the matching join key.
- **JetStream spans are explicitly excluded**: a JetStream message's `Reply` field is the `$JS.ACK.…` acknowledgement subject, not a conversation ID. `oteljetstream` receive/process attrs SHALL NOT map it to `conversation_id`. This is a documented, deliberate divergence from the core-NATS attribute builder (the "keep the attribute sets in sync" comment in `conn.go` gains a carve-out).
- **Non-goal (recorded decision)**: the wrapper SHALL NOT reimplement nats.go request internals (e.g. pre-generating an inbox via `NewRespInbox()` + own subscription) to learn the inbox before span start. Instrumentation must be behavior-preserving; replacing the mux-inbox fast path with per-request subscriptions changes server-side load characteristics for the sake of one Recommended attribute.
- **Unchanged**: `publishAttrs` keeps its existing `msg.Reply != ""` guard (callers doing manual request/reply via `PublishMsg` with an explicit `Reply` already get the attribute at span start).
- Sampling caveat documented: the late-set attribute on the send span is invisible to samplers (sampling decisions read span-start attributes only); `conversation_id` is a join key, not a sampling key.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `otel-nats-spans`: the "NATS span attribute set" requirement's vague conditional clause for `messaging.message.conversation_id` is replaced by a dedicated requirement specifying, per span type (request send / reply receive / process / JetStream), whether and when the attribute is set, the late-set timing on the send span, the omission on error paths, and the JetStream exclusion.

## Impact

- `otel-nats/otelnats/conn.go` — `receiveAttrs` gains the `msg.Reply != ""` → `conversation_id` clause (core process spans; also flows to the reply-receive span, whose message *is* the reply); the JetStream sync comment gains the divergence note.
- `otel-nats/otelnats/conn_traced.go` — `recordReply` sets `conversation_id = reply.Subject` on the request span (late-set) and ensures the receive span carries it.
- `oteljetstream` — no code change; its attr builders already omit `conversation_id`, now pinned by spec + comment.
- Tests: `otel-nats/otelnats/conn_test.go` — new round-trip test asserting all three core spans carry the same inbox value; JetStream exclusion asserted in existing consumer attr tests or a targeted addition.
- `otel-nats/CHANGELOG.md` — 0.7.0 (Unreleased) Added entry.
- No public API change. Additive attributes only — not breaking.
- Downstream: resolves o11y tracker F6 (Bundle C item) ahead of their umbrella issue.
