# Design: otel-nats-low-cardinality-span-names

## Context

See `proposal.md` — Why. Current mechanics that shape the approach:

- Span names are built inline at four sites in `otelnats` (`conn_traced.go`: `startSendSpan`,
  `startRequestSpan`, `recordReply`, `wrapMsgHandler`) and four in `oteljetstream`
  (`jetstream_traced.go` publish; `consumer_traced.go` ×3; `consumer.go` ordered-consumer
  fallback). There is no shared name builder today.
- Attributes already carry `messaging.operation.name=publish` on publish spans — only the span
  *name* says `send`. `receiveAttrs`/`receiveBaseAttrs` pass the op verb through, so `receive`/
  `process` names already agree with their attributes.
- Core process spans already use the **subscription** subject (`wrapMsgHandler(subject, …)`),
  not the delivered `msg.Subject` — the natural template. JetStream consumer spans use the
  delivered `msg.Subject()`.
- `oteljetstream` reads per-connection state via exported accessors on `otelnats.Conn`
  (`TracingEnabled()`, `TraceDest()`, `ServerAttrs()`, `TraceContext()`); both packages live in
  one `go.mod`.
- Module version `0.8.0`; pre-1.0 breaking → minor bump per `VERSIONING.md`.

## Goals / Non-Goals

**Goals:**
- **Primary: eliminate inbox-subject cardinality.** Every span whose destination is a
  request/reply inbox — on any of the three halves of an exchange — names itself without that
  subject.
- Bounded span-name cardinality on the other path an operator cannot control (wildcard
  subscriptions and single-filter consumers), using subjects the library already holds.
- Span names conform to semconv v1.39.0 `{messaging.operation.name} {destination}`.
- One shared destination-resolution implementation — no per-site drift.
- **Zero new configuration.** Every improvement lands without the application changing a line.

**Non-Goals:**
- No change to span kinds, links, context propagation, attribute keys already emitted, or the
  feature-flag ladder. `messaging.operation.type` values are untouched.
- No metrics emission — this module emits spans only; metrics guidance in the proposal is about
  downstream span-metrics pipelines.
- No templating heuristics (e.g. auto-detecting numeric tokens) and no caller-supplied mapping:
  the library never guesses, and semconv permits recording `messaging.destination.template` only
  when it is already available. Only the subscription/filter subject produces a template. Subjects
  that embed IDs are a collector-side rename (D4).
- otel-mongo / otel-gorilla-ws naming untouched (`websocket.send`/`websocket.receive` are not
  messaging-semconv spans; mongo already conforms to DB semconv).

## Decisions

**D1 — Rename span names to match the attribute, not vice versa.** `publish {dest}` /
`request {dest}` win over relabeling `messaging.operation.name` to `send`. Rationale: the
attribute is correct today (NATS's native verb is Publish; semconv examples use `publish` for
broker systems, `send` for Kafka), spans with the attribute are already queried by it, and
either direction is equally breaking for span-name consumers — so pick the one that leaves
attributes stable. `messaging.operation.type=send` is a semconv enum and stays.

**D2 — Bare `receive` for the reply span, not `(anonymous)`.** semconv v1.39.0 dropped the old
`(anonymous)` literal in favor of omitting the `{destination}` segment when no low-cardinality
value exists. The inbox stays on `messaging.destination.name` and
`messaging.message.conversation_id`: semconv scopes the low-cardinality rule to the span **name**,
and `destination.name` stays Conditionally Required with no temporary/anonymous carve-out. No
`_INBOX.` prefix sniffing in `recordReply`: its span is structurally always an inbox — hardcode the
markers there, correct even under `nats.CustomInboxPrefix` and even when the peer's prefix is one
this connection would not recognise.

**D2a — The same omission applies to every core-NATS span whose destination is an inbox, by
prefix test.** `recordReply` is only one of three halves of a manual request/reply exchange. A
responder that replies with `conn.Publish(msg.Reply, data)` (rather than `msg.Respond`, which is
the raw driver call and emits no span) produced `publish _INBOX.<nuid>`, and a requester that
subscribes to its own inbox produced `process _INBOX.<nuid>` — both unbounded, both invisible to
D2. The prefix is a fact the library holds (`nc.Opts.InboxPrefix`, else `nats.InboxPrefix`), not a
guess, so testing it is compatible with semconv's "MUST NOT infer" rule.

**D2b — The inbox test runs on the RESOLVED destination, not on `concrete`.** A subscription to
`<inbox>.>` has a *filter* that carries the request's nuid, so a test on the concrete subject alone
would still name the span after an unbounded string. The test therefore lives inside `Resolve`,
after the filter/concrete precedence has picked a destination — one site, eight call sites
automatically consistent.

**D2c — Two inbox prefixes are recognised, not one:** `nc.Opts.InboxPrefix + "."` when set, and
`nats.InboxPrefix` (`_INBOX.`) unconditionally. Recognising only the local prefix fails exactly
where custom prefixes are deployed. A responder sees the **requester's** inbox in `msg.Reply`, and
the requester is the side that customises — the reason to customise is that `subscribe: _INBOX.>`
hands a client every other client's replies, which is a requester permission, while a responder
needs no inbox permission at all (`allow_responses` covers it). Residual gap: two peers on two
*different* custom prefixes. Accepted; a collector-side rename covers it. `oteljetstream` passes
no prefixes — streams do not capture inbox subjects.

**D3 — Shared resolution in a new `otel-nats/internal/spanname` package.** One function,
`Resolve(op, concrete, filter string, inboxPrefixes []string) (name, templateAttr string, inbox
bool)`, used by both packages (same module, `internal/` visible to both). Alternative — exported
helper on `otelnats` — rejected: public surface for an implementation detail. Alternative —
duplicate per package with sync comments (the `receiveAttrs` pattern) — rejected: this logic has
branching (filter > concrete, the inbox test, template-attr emission) where the drift risk
actually bites.

**D4 — No caller-supplied subject→template option.** Subjects that embed identifiers
(`orders.12345.created`) keep the concrete subject in the span name on publish/request paths and on
JetStream consumers with no filter or several wildcard filters. Rationale, in order of weight: it
contributes nothing to the primary goal, since what an inbox needs is *omission* of the destination
and a `func(string) string` cannot express that; semconv permits recording
`messaging.destination.template` only when it is already available, and a caller mapping is exactly
the inference the spec pushes downstream; and re-adding an option later is non-breaking while
shipping one is not. The residual case is served by the collector `span` processor's
`name.to_attributes` rules, which also cover the multi-filter fallback in D5. A
`WithSubjectTemplate(func(subject string) string)` option was implemented and removed before
release; its "returning the argument unchanged means no opinion" rule also behaved differently on
publish paths (no filter) than on consume paths (filter present), which no godoc could make safe.

**D4a — Consumer filter subject as destination is NOT the same mechanism.** It requires no new
public API: the subscription subject is already a parameter of `Subscribe`, and the JetStream
filter subject is already in `ConsumerConfig`. The library reads facts it holds; it never asks the
caller for a template.

**D5 — JetStream filter subject resolved once per consumer wrapper, not per message.**
`Consumer.CachedInfo().Config` gives `FilterSubject`/`FilterSubjects` without RTT; the traced
consumer computes its static destination (single filter or empty=fallback) at wrap time and
stores it. Per-message work remains only the template-attr append. Ordered-consumer and any path
where config is not observable fall back to concrete subject, per spec. Note `FilterSubject == ""`
(whole stream) counts as "not single-valued" — falls back to concrete subject rather than naming
spans after an empty string; `>` as an explicit filter is used verbatim.

**D5a — A consumer with no filter does NOT fall back to the stream's `Config.Subjects`.** That
would be a legitimate fact rather than a guess, and it would cover the most common JetStream shape
(consumer with no filter on a wildcard stream). It is rejected because the `js.Consumer(ctx,
streamName, name)` path holds only the stream *name* and would need an extra `StreamInfo` round
trip at wrapper construction, and because the same deployment almost always also publishes — where
no filter exists at all and the concrete subject is used regardless. The value is confined to
services that only consume, and the collector-side rename covers them too.

**D6 — Multi-filter and no-filter consumers keep the concrete subject; they do not omit the
destination.** The span-name `{destination}` ladder's second rung is "use
`messaging.destination.name` when the destination is neither temporary nor anonymous", which a
wildcard-derived subject satisfies — the omission rung is for structurally unnamed destinations
(inboxes), not for names that merely have many values. Omitting would also discard which stream the
message came from, and `FilterSubjects[0]` would mislabel messages matching the other filters.

**D7 — Release: single `otel-nats` minor bump (0.8.0 → 0.9.0), BREAKING entries in
CHANGELOG with an old→new name migration table.** No other module changes; no `otel-flags`
release ordering involved.

## Risks / Trade-offs

- [Dashboards/alerts keyed on `send {subject}`, `{subject} request`, `receive _INBOX.*`,
  `publish _INBOX.*`, `process _INBOX.*` break] → CHANGELOG migration table listing every rename;
  pre-1.0 minor-bump policy already signals breakage; span attributes
  (`messaging.operation.name`, `conversation_id`) are stable join keys through the transition.
- [All inbox spans of a given operation now share one name] → intended aggregation; per-exchange
  drill-down stays via `messaging.message.conversation_id`.
- [A prefix test can false-positive on a business subject under `_INBOX.`] → `_INBOX.` is a NATS
  reserved convention (`nats.InboxPrefix`, used by `NewInbox()`); a subject that merely shares a
  token boundary (`_INBOXES.orders`) does not match, and the only consequence of a true
  false-positive is a span name missing its destination.
- [Two peers on two different custom inbox prefixes do not recognise each other] → accepted (D2c);
  rare, and a collector-side rename covers it.
- [Multi-filter, no-filter, and ID-embedding subjects still emit concrete subjects] → documented
  fallback pointing at the collector `span` processor; auto-joining filters into one name would
  fabricate a destination that exists nowhere in semconv, and inferring a template is what semconv
  forbids (D4, D6).
- [`CachedInfo` snapshot could lag a consumer-config edit] → destination is fixed at wrapper
  construction, same lifetime rule as the rest of the wrapper config; a stale filter subject
  still names a valid, low-cardinality grouping.

## Migration Plan

1. Land code + tests behind one PR; all three checks (`go build`, `go test -race`,
   `golangci-lint`) green in `otel-nats/`.
2. Update `otel-nats/tests/integration` span-name assertions (incl. `nats-sampling-e2e`
   fixtures if they assert names).
3. Bump `instrumentationVersion` to `0.9.0`, write CHANGELOG with the rename table, tag
   `otel-nats/v0.9.0`. Rollback = repin previous tag; no data migration exists (spans are
   ephemeral).

## Open Questions

None — inbox detection scope, prefix set, fallback ordering, and release shape are decided above.
