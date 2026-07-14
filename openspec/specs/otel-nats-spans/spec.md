# otel-nats-spans Specification

## Purpose
Span taxonomy for otel-nats/oteljetstream: no deliver spans, spec-correct span kinds (publish PRODUCER, pull-receive CLIENT, push process CONSUMER), and the normalized messaging.* attribute set.

## Requirements

### Requirement: No deliver spans or deliver TracerProvider

`otel-nats` (`otelnats` and `oteljetstream`) SHALL NOT emit synthetic "deliver" spans and SHALL NOT construct an independent deliver `TracerProvider`. No identifier `StartDeliverSpan`, `ConsumerContextWithDeliver`, `deliverTracer`, `deliverAttrs`, or `initNATSProvider` SHALL remain. The packages SHALL NOT read `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission. (The OTel messaging conventions define no `deliver` operation, so no such span has a conventional mapping.)

#### Scenario: No deliver span on publish or consume

- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT` is set and tracing is enabled and a caller publishes or a subscriber/consumer receives a message
- **THEN** no span named `"* deliver"` SHALL be emitted
- **AND** no separate deliver `TracerProvider`, `BatchSpanProcessor`, or OTLP exporter SHALL be created by the module

#### Scenario: Deliver identifiers removed

- **WHEN** the module source is compiled
- **THEN** no reference to `StartDeliverSpan`, `ConsumerContextWithDeliver`, `deliverTracer`, `deliverAttrs`, or `initNATSProvider` SHALL exist

### Requirement: Span kind per messaging operation

Span kind SHALL follow the OTel messaging "Span kind" mapping: `send` → `PRODUCER`, request/reply (caller awaits response) → `CLIENT`, `receive` (pull) → `CLIENT`, `process` (push) → `CONSUMER`.

#### Scenario: Core NATS span kinds

- **WHEN** the wrapper emits spans for `Publish`, `Request`, reply reception, and a subscription handler
- **THEN** `Publish` SHALL be `PRODUCER`
- **AND** `Request` SHALL be `CLIENT`
- **AND** the reply-reception (`receive`) span SHALL be `CLIENT`
- **AND** the subscription-handler (`process`) span SHALL be `CONSUMER`

#### Scenario: JetStream span kinds

- **WHEN** the wrapper emits spans for JetStream publish, pull consume (`Consume` / `Fetch` / `Messages` iterator), and a push-delivered handler
- **THEN** JetStream publish SHALL be `PRODUCER`
- **AND** pull-consume (`receive`) spans SHALL be `CLIENT`
- **AND** any push-delivered (`process`) span SHALL be `CONSUMER`

### Requirement: NATS span attribute set

Message spans SHALL carry OTel messaging-semconv attributes: `messaging.system`, `messaging.destination.name`, `messaging.operation.type`, `messaging.operation.name`, `messaging.message.body.size` (when body non-empty), plus `server.address` / `server.port`. Conditional attributes SHALL be set when their value exists: `messaging.message.conversation_id` (reply subject), `messaging.consumer.group.name` (queue group). `messaging.operation.type` for a pull-receive span SHALL be `receive`.

JetStream consumer spans (`receive` and `process`) SHALL additionally carry `messaging.consumer.group.name` set to the JetStream durable/consumer name (the semconv v1.39.0 key; this delta originally specified the non-semconv literal `messaging.consumer.name`, renamed by the address-o11y-feedback change — aligned here so archiving this change cannot reintroduce the old key). It is the only messaging attribute unique to `oteljetstream` — core `otelnats` spans do not carry it.

#### Scenario: Publish attributes

- **WHEN** a caller publishes a non-empty message to subject `orders.new`
- **THEN** the span SHALL carry `messaging.system=nats`, `messaging.destination.name=orders.new`, `messaging.operation.type=send`, `messaging.operation.name=publish`, `messaging.message.body.size=<len>`

#### Scenario: Pull-receive attributes and kind agree

- **WHEN** a JetStream pull consumer receives a message
- **THEN** the span SHALL carry `messaging.operation.type=receive`
- **AND** the span kind SHALL be `CLIENT`

#### Scenario: JetStream span carries consumer name

- **WHEN** a JetStream consumer named `orders-worker` receives or processes a message
- **THEN** the span SHALL additionally carry `messaging.consumer.group.name=orders-worker`
- **AND** an equivalent core-NATS `Publish` / subscribe span SHALL NOT carry `messaging.consumer.group.name`

### Requirement: Disabled tracing emits no spans or SDK objects

When the tracing gate is off (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` are not both truthy), the wrapper SHALL delegate to the native NATS / JetStream client and run no OTel SDK code path — no real-tracer `Start`, no `TracerProvider`, no exporter, no propagator inject/extract — consistent with the module-wide disabled-mode invariant. Removing the deliver `TracerProvider` shrinks this disabled surface (its init is gone, not merely gated off).

#### Scenario: Tracing disabled delegates to native client

- **WHEN** the tracing gate is off and a caller invokes `Publish` or `Request`, or a subscriber / consumer receives a message
- **THEN** the wrapper SHALL delegate to the native `*nats.Conn` / JetStream client
- **AND** no span SHALL be emitted
- **AND** no `TracerProvider`, `BatchSpanProcessor`, or OTLP exporter SHALL be constructed
- **AND** the trace propagator SHALL NOT be invoked to inject or extract
