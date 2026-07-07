## Why

The deliver-span pattern (synthetic broker-node spans emitted through an independent, `OTEL_EXPORTER_OTLP_ENDPOINT`-gated `TracerProvider`) adds a second exporter, extra feature-flag surface, background goroutines, and per-operation span overhead — all to render MongoDB/NATS as a node in the Grafana service graph. That value no longer justifies the cost: it complicates the disabled-mode invariant, duplicates provider-init logic across modules, and couples span emission to an exporter env var. Removing it lets each package emit a single, correct span per operation. While the span emission paths are open, we also right-size the exported attribute set and fix span kinds that are semantically wrong under the OTel messaging/database conventions.

## What Changes

- **BREAKING** Remove all deliver-span logic from `otel-mongo` (v1 + v2) and `otel-nats` (`otelnats` + `oteljetstream`):
  - Delete the independent deliver `TracerProvider` init (`initMongoProvider`, `initNATSProvider`) and every field/param that carries it (`DeliverTracer`, `deliverTracer`, `StartDeliverSpan`, `ConsumerContextWithDeliver`).
  - Delete the `_ deliver` span starts in `internal/traced/{collection,changestream}.go` (mongo) and `conn_traced.go` (nats), plus `DeliverAttributes` / `deliverAttrs` helpers.
  - Remove the deliver-span half of the `OTEL_EXPORTER_OTLP_ENDPOINT` gate — the packages no longer read that var for span emission.
  - Delete deliver-specific tests (`client_deliver_test.go`, deliver assertions in `changestream_test.go`, `conn_test.go`, `consumer_test.go`, integration tests).
  - `otel-gorilla-ws` never implemented deliver spans — no removal there, but its span kind/attributes are still in scope for goals 2–3.
- **Adjust exported span attributes by necessity** (add missing, drop unnecessary) across all three packages — not a wholesale refactor. Keep OTel semconv keys; remove attributes that duplicate span name or add no query value; add the minimal missing conventional attributes.
- **Fix span kinds** to the correct OTel value per span across all three packages (e.g. async consumer reads that are currently mislabeled).
- Bump each touched module to `0.6.0` and update READMEs/CLAUDE.md (delete the "Deliver Spans (Service Graph)" sections and the deliver-flag documentation).

## Capabilities

### New Capabilities
- `otel-mongo-spans`: the spans `otel-mongo` (v1 + v2) emits after this change — their kinds, attribute sets, and the explicit absence of deliver spans / deliver `TracerProvider`.
- `otel-nats-spans`: the spans `otelnats` + `oteljetstream` emit — kinds, attribute sets, and the explicit absence of deliver spans / deliver `TracerProvider`.
- `otel-gorilla-ws-spans`: the spans `otel-gorilla-ws` emits — read/write span kinds and attribute set.

### Modified Capabilities
<!-- none: no baseline specs exist in openspec/specs/ -->

## Impact

- **Modules**: `otel-mongo/otelmongo`, `otel-mongo/v2`, `otel-nats/otelnats`, `otel-nats/oteljetstream`, `otel-gorilla-ws` — all bump to `0.6.0`.
- **Source removed**: deliver `TracerProvider` init, `DeliverTracer`/`deliverTracer` fields, `StartDeliverSpan`, `ConsumerContextWithDeliver`, `DeliverAttributes`, `deliverAttrs`, and their wiring in `client.go`/`database.go`/`collection.go`/`conn.go`.
- **Public API**: exported deliver-related identifiers and any `WithDeliver*` options are removed — breaking for callers that referenced them; `0.x` minor bump covers it.
- **Env vars**: `OTEL_EXPORTER_OTLP_ENDPOINT` is no longer consulted by these packages for span emission (applications still use it to configure their own global exporter).
- **Disabled-mode invariant**: simplifies — one fewer SDK code path to gate. The "no deliver-span goroutine when disabled" scenarios become "no deliver-span goroutine, ever."
- **Docs**: README (all modules + root), CLAUDE.md deliver-span sections deleted; `otel-ws.md` unaffected.
