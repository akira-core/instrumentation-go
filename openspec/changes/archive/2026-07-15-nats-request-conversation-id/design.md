# Design: nats-request-conversation-id

## Context

`messaging.message.conversation_id` (semconv v1.39.0, requirement level **Recommended**) is the correlation-ID attribute for messaging spans. In NATS request/reply the natural conversation ID is the reply inbox subject (`_INBOX.…`): the requester's wire message carries it as `reply`, the responder sees it as `msg.Reply`, and the reply message arrives *on* it (`reply.Subject`). Three observation points, one value.

Current state (0.7.0 branch):

- `requestAttrs`/`publishAttrs` set the attribute only when `msg.Reply != ""` at span start. On `Request`/`RequestWithContext`/`RequestMsg*` the caller's message has an empty `Reply` — nats.go generates the mux inbox *inside* the driver call and never mutates the caller's message — so the guard is a dead branch on the primary API.
- `receiveAttrs` (process + reply-receive spans) has no `conversation_id` clause at all.
- Net: **no span in a standard request/reply exchange carries the inbox**, on either side. Correlation is span-link-only, which fails under sampling, uninstrumented peers, and UIs with weak link support.

Constraint: instrumentation must be behavior-preserving — no change to the library's wire behavior, subscription model, or performance characteristics.

## Goals / Non-Goals

**Goals:**

- Every core-NATS span in a successful request/reply exchange carries the same `messaging.message.conversation_id` value (the inbox), queryable as a plain attribute on both sides.
- The spec records *when* the value is observable per span and why omission on error paths is conformant.
- JetStream exclusion is pinned (spec + comment) so future "sync the attr builders" work cannot leak `$JS.ACK.…` subjects into `conversation_id`.

**Non-Goals:**

- No pre-knowledge of the inbox on the send span's start (would require reimplementing nats.go request internals — rejected, see Decisions).
- No app-level correlation-ID hook (that is downstream tracker item R4, a separate API discussion).
- No change to `publishAttrs` semantics.
- No JetStream `conversation_id` mapping.

## Decisions

### D1. Late-set on the send span from `reply.Subject`, success path only

`recordReply` appends `SetAttributes(MessagingMessageConversationID(reply.Subject))` on the request span before it ends (`recordReply` runs before the deferred `reqSpan.End()` in both `requestWithTimeout` and `requestWithCtx`).

- **Why**: OTel explicitly allows attribute writes any time before `End()`; this is the canonical pattern for values discovered mid-operation (HTTP instrumentations set `http.response.status_code` the same way). `reply.Subject` *is* the inbox — the value becomes observable to the wrapper exactly when the reply arrives.
- **Alternative rejected — wrapper-generated inbox** (`nc.NewRespInbox()` + own subscription to know the ID up front): reimplements the driver's request path; replaces the mux-inbox single-wildcard-subscription design with per-request subscriptions, changing server-side load. Violates the behavior-preserving constraint for the sake of one Recommended attribute.
- **Alternative rejected — asking nats.go to write the inbox back**: right long-term fix for span-start availability, but upstream of us; can be pursued independently without blocking this.
- **Consequence (accepted)**: timeout/error paths omit the attribute (the inbox never becomes observable). Recommended-level attributes are conformant when omitted. Documented in the requirement.
- **Consequence (accepted)**: samplers cannot key on the late-set attribute (sampling reads span-start attributes). `conversation_id` is a join key, not a sampling key.

### D2. Reply-receive span: explicit set in `recordReply`, not in `receiveAttrs`

The reply message's own `Reply` field is empty (a responder's `Respond()` is a plain publish to the inbox), so a generic `msg.Reply != ""` clause in `receiveAttrs` cannot cover this span. The conversation ID here is `reply.Subject` — same value, different field. `recordReply` appends it to the receive span's start attributes explicitly.

- **Why not teach `receiveAttrs` a special case**: `receiveAttrs` is shared by process spans (source: `msg.Reply`) and the reply-receive span (source: `msg.Subject`); parameterizing "which field means conversation" adds a mode flag for one call site. Explicit append at the call site is smaller and self-documenting.

### D3. Process span: `msg.Reply != ""` clause in `receiveAttrs`

The responder-side subscription handler sees the request's `msg.Reply` (the inbox) natively. `receiveAttrs` gains the same guard `publishAttrs`/`requestAttrs` already use. Non-request messages have empty `Reply` → no attribute, unchanged.

### D4. JetStream: explicitly excluded, divergence documented

JetStream `msg.Reply` is the `$JS.ACK.<stream>.<consumer>.…` acknowledgement subject — protocol plumbing, not a conversation. `oteljetstream`'s `receiveBaseAttrs`/`receiveMsgAttrs` stay `conversation_id`-free. The `conn.go:311` "keep the attribute sets in sync" comment gains a carve-out sentence naming `conversation_id` as core-NATS-only, so a future sync pass cannot copy the clause over.

### D5. Attribute key reuse

Use the generated `semconv.MessagingMessageConversationID` (already imported in `conn.go`); no string literals.

## Risks / Trade-offs

- **[Risk] Send-span attribute invisible to samplers** → Accepted; documented in spec scenario. Join key, not sampling key.
- **[Risk] Timeout/error request spans lack the attribute → dashboards joining on it silently miss failed exchanges** → Documented in CHANGELOG and requirement text; failed exchanges have no reply and thus no inbox observable anywhere in the wrapper. Span links (unchanged) still cover the instrumented-peer case.
- **[Risk] Core `process` spans on subscribers that receive non-request traffic with a stray `Reply` set** → The attribute reflects what the message carries; a set `Reply` field *is* a reply-expected message by NATS semantics. No mitigation needed.
- **[Risk] Attribute-count growth on hot process spans** → One `KeyValue` per request/reply message, none for plain publishes. Negligible.
- **[Trade-off] Reply-receive span carries the inbox twice** (`messaging.destination.name` and `conversation_id`) → Accepted: `destination.name` is structural (the subject it arrived on), `conversation_id` is the queryable join key; backends index/query them differently, and dropping either would break one use case.

## Migration Plan

Additive attributes only; no API or wire change. Ships in the unreleased 0.7.0 entry (branch not yet tagged) — no separate release needed. Rollback = revert the commit.

## Open Questions

None. (Upstream nats.go inbox-exposure request and the R4 caller-attribute hook are deliberately out of scope.)
