## 1. otel-mongo v1 — remove deliver spans

- [x] 1.1 Delete `StartDeliverSpan` and the `DeliverTracer`/`ServerAddr`/`ServerPort` deliver wiring from `otelmongo/internal/traced/collection.go`; remove the `injectCtx, deliverSpan := t.StartDeliverSpan(...)` blocks from every operation (keep propagation inject using the operation-span context).
- [x] 1.2 Delete the deliver span (SpanKindProducer) from `otelmongo/internal/traced/changestream.go`.
- [x] 1.3 Delete `DeliverAttributes` from `otelmongo/internal/shared/semconv.go`.
- [x] 1.4 Delete `initMongoProvider` and its call sites in `otelmongo/client.go` / `database.go` / `collection.go`; remove any `DeliverTracer` field and `WithDeliver*` option.
- [x] 1.5 Stop reading `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission in `otelmongo` (remove from the deliver gate; leave global exporter setup to the app).
- [x] 1.6 Delete `otelmongo/client_deliver_test.go` and remove deliver assertions from `changestream_test.go`, `tracing_test.go`, `client_test.go`.
- [x] 1.7 `go build ./... && go test -race ./... && golangci-lint run ./...` in `otel-mongo/` — all pass; CI "direct/ has no OTel SDK imports" grep still clean.

## 2. otel-mongo v2 — mirror v1 (parity)

- [x] 2.1 Apply tasks 1.1–1.6 identically to `otel-mongo/v2/` (including `v2/internal/{direct,traced,shared}/`, `v2/client_deliver_test.go`).
- [x] 2.2 `go build ./... && go test -race ./... && golangci-lint run ./...` in `otel-mongo/v2/` — all pass; direct/ grep clean.

## 3. otel-mongo — span kind + attributes

- [x] 3.1 Set change-stream read span to `SpanKindClient` in `internal/traced/changestream.go` (v1 + v2); confirm cursor decode stays `Internal` and CRUD stays `Client`.
- [x] 3.2 Confirm operation attribute set matches DB semconv (no positive change expected); ensure no deliver-only attributes remain (v1 + v2).
- [x] 3.3 Add/keep a test asserting the disabled gate (`OTEL_MONGO_TRACING_ENABLED` unset) delegates via `internal/direct`, emits zero spans, injects no `_oteltrace`, and builds no provider/exporter (v1 + v2) — covers the "Disabled tracing emits no spans or SDK objects" requirement.

## 4. otel-nats otelnats — remove deliver + span kind

- [x] 4.1 Delete `ConsumerContextWithDeliver`, `StartDeliverSpan`, `deliverTracer`, `deliverAttrs`, `initNATSProvider` and their call sites in `conn_traced.go` / `conn.go` / `conn_direct.go`; keep propagation inject/extract using the operation-span context.
- [x] 4.2 Stop reading `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission in `otelnats`.
- [x] 4.3 Change the reply-reception (`receive`) span in `recordReply` from `SpanKindConsumer` to `SpanKindClient`; keep `Publish`=Producer, `Request`=Client, subscribe `process`=Consumer.
- [x] 4.4 Ensure the pull-receive path emits `messaging.operation.type=receive` (agrees with CLIENT kind).
- [x] 4.5 Add/keep a test asserting the disabled gate (`OTEL_NATS_TRACING_ENABLED` unset) delegates to the native `*nats.Conn`, emits no span, invokes no propagator inject/extract, and builds no provider/exporter — covers the "Disabled tracing emits no spans or SDK objects" requirement.

## 5. otel-nats oteljetstream — remove deliver + span kind

- [x] 5.1 Remove any deliver wiring from `consumer.go` / `consumer_traced.go` / `jetstream_traced.go`.
- [x] 5.2 Change pull-consume spans (`Consume` / `Fetch` / `Messages` iterator, `receive`) from `SpanKindConsumer` to `SpanKindClient`; keep publish=Producer and push `process`=Consumer.
- [x] 5.3 Delete deliver assertions from `oteljetstream/consumer_test.go` and `otelnats/conn_test.go`.
- [x] 5.4 Add/keep a test asserting JetStream `receive`/`process` spans carry `messaging.consumer.name=<consumer>` and that core-NATS spans do not — covers the JetStream-consumer-name addition to the "NATS span attribute set" requirement.
- [x] 5.5 `go build ./... && go test -race ./... && golangci-lint run ./...` in `otel-nats/` — all pass.

## 6. otel-gorilla-ws — attributes only

- [x] 6.1 In `conn.go`, remove `messaging.message.body.size`; add `websocket.message.body.size`; keep `websocket.message.type`. Confirm no `messaging.*` attribute remains.
- [x] 6.2 Confirm write=`Producer` / read=`Consumer` span kinds unchanged.
- [x] 6.3 Add/keep a test asserting a `tracingEnabled == false` connection passes `WriteMessage`/`ReadMessage` through to the native `*websocket.Conn` (no JSON envelope), emits no span, and invokes no propagator — covers the "Disabled tracing emits no spans or SDK objects" requirement.
- [x] 6.4 `go build ./... && go test -race ./... && golangci-lint run ./...` in `otel-gorilla-ws/` — all pass.

## 7. Version bump + docs

- [x] 7.1 Bump `instrumentationVersion` to `0.6.0` in `otel-nats/otelnats/conn.go`, `otel-mongo/otelmongo/version.go`, `otel-mongo/v2/version.go`, and `otel-gorilla-ws` `Version()`.
- [x] 7.2 Delete "Deliver Spans (Service Graph)" sections + deliver-flag docs from `otel-mongo/README*.md`, `otel-nats/README*.md`, root `README*.md`, and CLAUDE.md; update span-kind/attribute descriptions to match.
- [x] 7.3 Update `otel-mongo/examples/main.go` and `otel-nats/examples/main.go` to drop deliver usage.

## 8. Integration tests + final verification

- [x] 8.1 Update/remove deliver assertions in `otel-nats/tests/integration/{jetstream_test.go,nats_test.go}`; add/adjust assertions for the new span kinds.
- [x] 8.2 Run integration tests (Docker) for `otel-nats`, `otel-mongo`, `otel-mongo/v2`, `otel-gorilla-ws` — all pass.
- [x] 8.3 Full sweep: `go build`, `go test -race`, `golangci-lint run` green in all four modules; grep confirms zero deliver identifiers remain.
