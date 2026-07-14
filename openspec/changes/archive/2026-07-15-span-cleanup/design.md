## Context

Four modules (`otel-mongo/otelmongo`, `otel-mongo/v2`, `otel-nats/otelnats`, `otel-nats/oteljetstream`, `otel-gorilla-ws`) emit trace spans. Two of them (mongo, nats) additionally emit **deliver spans**: synthetic broker-node spans on an independent `OTEL_EXPORTER_OTLP_ENDPOINT`-gated `TracerProvider`, purely to render the datastore/broker as a node in Grafana's service graph. This adds a second exporter, extra flag surface, background goroutines, and duplicated provider-init across modules.

The OTel messaging conventions define no `deliver` operation (operations are `create`/`send`/`receive`/`process`/`settle`), so deliver spans have no conventional mapping. Removing them both simplifies the code and aligns with the spec. While these paths are open we also fix span kinds against the spec and right-size attributes.

Constraints: `otel-mongo` v1 and v2 are parallel implementations with a strict parity rule (including `internal/{direct,traced,shared}/`). The disabled-mode invariant (no OTel SDK code path when a flag is off) must be preserved. Mongo Collection/Cursor/SingleResult/ChangeStream use the strategy-split layout; Client/Database and the nats/ws wrappers use the cached-gate pattern.

## Goals / Non-Goals

**Goals:**
- Delete all deliver-span logic from mongo (v1+v2) and nats (otelnats+oteljetstream): providers, fields, helpers, options, tests, docs.
- Stop consulting `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission in these packages.
- Set each span's kind to the OTel-correct value (see Decisions table).
- Normalize the exported attribute set: keep semconv keys, add missing required ones, drop wrong-namespace/no-value ones, retain minimal useful custom keys.
- Bump every touched module to `0.6.0`; update READMEs + CLAUDE.md.

**Non-Goals:**
- No wholesale attribute refactor — only add/remove by necessity.
- No migration of cached-gate wrappers to the strategy-split pattern.
- No change to trace propagation carriers (`_oteltrace` doc field, NATS headers, WS envelope) or to the span-link vs parent-child model for async consumers.
- No change to `otel-gorilla-ws` span kinds (already spec-correct).

## Decisions

### D1 — Remove deliver spans entirely (not flag-off)

Delete rather than gate. Removed surface: mongo `StartDeliverSpan`, `DeliverTracer` field, `DeliverAttributes`, `initMongoProvider`, deliver-span starts in `internal/traced/{collection,changestream}.go`, `client_deliver_test.go`; nats `ConsumerContextWithDeliver`, `StartDeliverSpan`, `deliverTracer`, `deliverAttrs`, `initNATSProvider`, and their call sites in `startSendSpan`/`startRequestSpan`/`wrapMsgHandler`. Any exported `WithDeliver*` option is removed.

*Rationale:* the spec has no `deliver` operation; a flag would keep dead SDK paths and complicate the disabled-mode invariant. Removal is the simpler end state. *Alternative considered:* keep behind a default-off flag — rejected; retains all the cost this change exists to delete.

### D2 — Span kinds grounded in the OTel spec

Per OTel SpanKind (`CLIENT`/`SERVER` = caller awaits response; `PRODUCER`/`CONSUMER` = async, producer does not wait) and the messaging "Span kind" table (`receive`→CLIENT, `process`→CONSUMER, `send`→PRODUCER):

| Span | Before | After | Basis |
|---|---|---|---|
| mongo CRUD/command | Client | Client | DB semconv (unchanged) |
| mongo cursor decode | Internal | Internal | local work (unchanged) |
| mongo change-stream read | Consumer/Producer(deliver) | **Client** | Mongo is a DB; getMore is a synchronous DB call |
| nats publish | Producer | Producer | `send` (unchanged) |
| nats request | Client | Client | RPC, awaits reply (unchanged) |
| nats reply receive | Consumer | **Client** | `receive` (pull) → CLIENT |
| nats subscribe handler | Consumer | Consumer | `process` (push) (unchanged) |
| jetstream publish | Producer | Producer | `send` (unchanged) |
| jetstream pull consume | Consumer | **Client** | `receive` (pull) → CLIENT |
| jetstream push process | Consumer | Consumer | `process` (push) (unchanged) |
| ws write / read | Producer / Consumer | Producer / Consumer | async frame; CLIENT would imply an awaited response (unchanged) |

*Alternative considered for ws:* CLIENT/SERVER — rejected, `WriteMessage` awaits no response so CLIENT's definition does not hold. *Alternative considered for pull spans:* keep CONSUMER (common in existing instrumentations) — rejected in favor of the spec letter.

### D3 — Attributes: semconv + minimal custom

- **mongo:** already DB-semconv-conformant; only drop `DeliverAttributes`. No positive attribute change needed.
- **nats:** already messaging-semconv-conformant (`messaging.system`, `messaging.destination.name`, `messaging.operation.type`, `messaging.operation.name`, `messaging.message.body.size`, conditional `conversation_id`/`consumer.group.name`, server attrs). Ensure the pull-receive path emits `messaging.operation.type=receive` so it agrees with the new CLIENT kind.
- **ws:** WebSocket is not a covered messaging system, so drop the `messaging.*` namespace. Keep custom `websocket.message.type`; move body size from `messaging.message.body.size` to `websocket.message.body.size`.

### D4 — v1/v2 parity and strategy-split boundary preserved

Every mongo change is applied identically to `otelmongo/` and `v2/`, including `internal/{direct,traced,shared}/`. The change-stream kind change lives only in `internal/traced/changestream.go`; `internal/direct/` stays free of `otel/sdk` imports (CI grep still enforced). No new public method is added, so the three-file lockstep for strategy-split additions does not apply.

## Risks / Trade-offs

- **Loss of the service-graph broker node** → Accepted; that was the sole purpose of deliver spans and the change explicitly drops it. Document the removal in READMEs so users relying on the Grafana node know.
- **Breaking public API (removed `WithDeliver*` / exported deliver identifiers)** → `0.x` minor bump communicates breakage; note in READ­MEs/CHANGELOG.
- **Strict pull=CLIENT deviates from many existing OTel instrumentations** → Accepted per user decision; documented with the spec citation so it is defensible in review.
- **v1/v2 drift during removal** → Mitigate by editing both trees in the same task and running build+test+lint for both before completion.
- **ws `websocket.message.body.size` is a non-standard key** → Accepted; no ws semconv exists, and it stays in a self-consistent `websocket.*` namespace rather than mislabeled `messaging.*`.

## Migration Plan

1. Remove deliver logic per module (mongo v1, mongo v2, nats), delete deliver tests, run build+test+lint each.
2. Apply span-kind fixes (D2) in `internal/traced/*` (mongo) and `conn_traced.go`/`consumer_traced.go` (nats).
3. Apply attribute fixes (D3), primarily ws namespace + nats pull `operation.type`.
4. Update version consts to `0.6.0` and READMEs/CLAUDE.md (delete "Deliver Spans" sections + deliver-flag docs).
5. Update integration tests that assert deliver spans.

Rollback: revert the branch; no data/schema migration involved.

## Open Questions

- None outstanding — span-kind (strict pull=CLIENT, change-stream=CLIENT, ws unchanged) and attribute policy (semconv + minimal custom) are decided.
