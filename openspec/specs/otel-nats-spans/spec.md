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

### Requirement: Span names follow the operation-first semconv format

Span names SHALL follow the OTel messaging semconv v1.39.0 format
`{messaging.operation.name} {destination}` — the operation verb SHALL equal the span's
`messaging.operation.name` attribute value, and the destination SHALL follow the span-name
destination resolution requirement below:

- Publish spans (core NATS `Publish`/`PublishMsg` and JetStream publish) SHALL be named
  `publish {destination}` (**BREAKING**: was `send {subject}` while the span already carried
  `messaging.operation.name=publish`).
- Request spans SHALL be named `request {destination}` (**BREAKING**: was the destination-first
  `{subject} request`).
- Subscription-handler process spans SHALL be named `process {destination}` and JetStream
  receive spans `receive {destination}` (verbs unchanged).
- Any span whose resolved destination is an **unbounded** request/reply inbox SHALL omit the
  destination segment entirely and be named by its operation verb alone — `receive`, `publish`,
  or `process` (**BREAKING**: was `receive {inbox}`, `publish {inbox}`, `process {inbox}`). An
  unbounded inbox is an auto-generated, single-use subject, so no low-cardinality destination
  value exists and semconv directs omitting the `{destination}` segment. This covers the
  reply-receive span of a `Request`, a reply published with `conn.Publish(msg.Reply, …)`, a
  handler on an inbox subscription, and — since it is not a core-NATS-only rule — any JetStream
  span over a stream that captures inbox subjects.
- A resolved destination that consists of a recognised inbox prefix followed **only by wildcard
  tokens** (`_INBOX.>`, `_INBOX.*.*`) SHALL be treated as bounded: it SHALL be kept in the span
  name and SHALL set `messaging.destination.template`. Such a destination is a fixed string the
  subscriber declared, and semconv attaches the temporary/anonymous exclusion to
  `messaging.destination.name` — its second choice for `{destination}` — not to
  `messaging.destination.template`, its first. A resolved destination carrying any literal token
  after the prefix (`_INBOX.<nuid>.>`) is per-request and SHALL be treated as unbounded.
- A destination SHALL be recognised as an inbox by subject prefix, testing the **resolved**
  destination rather than the concrete subject. Two prefixes SHALL be recognised: the
  connection's own (`nats.CustomInboxPrefix(p)` yielding `p + "."`) and the default `_INBOX.`
  unconditionally. Where several recognised prefixes match, the **longest** SHALL be stripped
  before the remainder is tested for boundedness, since one prefix may nest inside another. The
  reply-receive span SHALL be treated as an inbox unconditionally, without a prefix test, since
  it is structurally always one.
- The inbox verdict SHALL drive the inbox attributes independently of whether the destination
  survived in the span name: a bounded inbox filter SHALL still carry the temporary, anonymous
  and conversation-ID markers, which describe the delivery rather than the name.

#### Scenario: Publish span name matches its operation name

- **WHEN** a caller publishes to subject `orders.new` with tracing enabled
- **THEN** the PRODUCER span SHALL be named `publish orders.new`
- **AND** its `messaging.operation.name` SHALL be `publish`

#### Scenario: Request span is operation-first

- **WHEN** a caller invokes `Request` on subject `svc.echo`
- **THEN** the CLIENT span SHALL be named `request svc.echo`
- **AND** no span named `svc.echo request` SHALL be emitted

#### Scenario: Reply-receive span omits the inbox

- **WHEN** a `Request` on any subject receives a reply on inbox `_INBOX.<nuid>`
- **THEN** the reply-receive CLIENT span SHALL be named exactly `receive`
- **AND** the inbox subject SHALL NOT appear in any span name

#### Scenario: Manual reply publish omits the inbox

- **WHEN** a responder replies with `conn.Publish(ctx, msg.Reply, data)` where `msg.Reply` is an
  inbox subject
- **THEN** the PRODUCER span SHALL be named exactly `publish`

#### Scenario: Inbox subscription handler omits the inbox

- **WHEN** a handler subscribed to an inbox subject processes a message delivered on it
- **THEN** the CONSUMER span SHALL be named exactly `process`

#### Scenario: Inbox detection uses the resolved destination

- **WHEN** a handler subscribed to `<inbox>.>` processes a message delivered on `<inbox>.3`
- **THEN** the CONSUMER span SHALL be named exactly `process`, since the resolved destination —
  the subscription subject — is itself an inbox

#### Scenario: A custom-prefix connection still recognises default-prefix peers

- **WHEN** a connection created with `nats.CustomInboxPrefix("SVCA")` publishes to `SVCA.<nuid>`
  and to `_INBOX.<nuid>`
- **THEN** both PRODUCER spans SHALL be named exactly `publish`

#### Scenario: A prefix-only wildcard filter stays in the span name

- **WHEN** a consumer whose single filter subject is `_INBOX.>` receives a message delivered on
  `_INBOX.<nuid>`
- **THEN** the span SHALL be named `receive _INBOX.>`
- **AND** it SHALL carry `messaging.destination.template=_INBOX.>`
- **AND** it SHALL still carry `messaging.destination.temporary=true`,
  `messaging.destination.anonymous=true` and `messaging.message.conversation_id` set to the
  concrete delivered subject

#### Scenario: A filter carrying a literal token remains unbounded

- **WHEN** a handler whose filter is `_INBOX.<nuid>.>` processes a message delivered on
  `_INBOX.<nuid>.3`
- **THEN** the span SHALL be named exactly `process`, since the literal nuid token makes the
  filter per-request

### Requirement: Span-name destination resolution and template attribute

The `{destination}` segment of a subject-derived span name SHALL resolve in this order:

1. The subscription or consumer filter subject, where one exists and is single-valued: core
   NATS process spans use the subscription's subject (existing behavior, now normative);
   JetStream consumer receive/process spans use the consumer's filter subject when the
   consumer has exactly one.
2. The concrete message subject.

The resolved destination SHALL then be omitted from the span name when it is an **unbounded**
inbox, per the requirement above; a bounded inbox form SHALL be kept.

Whenever the resolved destination differs from the concrete message subject (a wildcard-bearing
subscription or filter), the span SHALL carry `messaging.destination.template` set to the
resolved destination. A span whose destination was omitted from the name SHALL NOT carry
`messaging.destination.template` — omission means no low-cardinality value was available, and
a template attribute would contradict that. `messaging.destination.name` SHALL always carry the
concrete message subject regardless of what the span name uses.

A resolve call with no filter subject — every publish and request path — SHALL NOT produce a
template value, since the resolved destination and the concrete subject are then the same string.
Implementations SHALL NOT carry a template-setting branch on those paths.

#### Scenario: Wildcard subscription process span uses the subscription subject

- **WHEN** a handler subscribed to `orders.*` processes a message delivered on `orders.12345`
- **THEN** the CONSUMER span SHALL be named `process orders.*`
- **AND** the span SHALL carry `messaging.destination.template=orders.*` and
  `messaging.destination.name=orders.12345`

#### Scenario: Exact-subject destination sets no template attribute

- **WHEN** a caller publishes to the literal subject `orders.new`
- **THEN** the span SHALL be named `publish orders.new`
- **AND** the span SHALL NOT carry `messaging.destination.template`

### Requirement: NATS span attribute set

Message spans SHALL carry OTel messaging-semconv attributes: `messaging.system`, `messaging.destination.name`, `messaging.operation.type`, `messaging.operation.name`, `messaging.message.body.size` (when body non-empty), plus `server.address` / `server.port`. Conditional attributes SHALL be set when their value exists: `messaging.message.conversation_id` (per the dedicated "Request/reply conversation ID" requirement), `messaging.consumer.group.name` (queue group), `messaging.destination.template` (when the span-name destination resolution produced a value differing from the concrete subject, per the "Span-name destination resolution and template attribute" requirement). `messaging.operation.type` for a pull-receive span SHALL be `receive`.

Every span whose destination is a request/reply inbox SHALL additionally carry `messaging.destination.temporary=true`, `messaging.destination.anonymous=true` and `messaging.message.conversation_id` set to that inbox subject: the reply inbox is auto-generated (anonymous) and ceases to exist after the exchange (temporary). Its `messaging.destination.name` SHALL remain the concrete inbox subject — semconv scopes the low-cardinality requirement to the span name and leaves `messaging.destination.name` Conditionally Required with no carve-out for temporary or anonymous destinations — so the exchange stays joinable by attribute query even though the span name omits the inbox. No span with a non-inbox destination SHALL carry `messaging.destination.temporary` or `messaging.destination.anonymous`.

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

#### Scenario: Reply-receive span marks the inbox anonymous and temporary

- **WHEN** a `Request` receives a reply on inbox `_INBOX.<nuid>`
- **THEN** the reply-receive span SHALL carry `messaging.destination.temporary=true`,
  `messaging.destination.anonymous=true`, and `messaging.destination.name=_INBOX.<nuid>`

#### Scenario: A manual reply publish carries the same markers

- **WHEN** a responder replies with `conn.Publish(ctx, msg.Reply, data)` to inbox `_INBOX.<nuid>`
- **THEN** the PRODUCER span SHALL carry `messaging.destination.temporary=true`,
  `messaging.destination.anonymous=true`, `messaging.destination.name=_INBOX.<nuid>` and
  `messaging.message.conversation_id=_INBOX.<nuid>`

#### Scenario: Ordinary spans carry no anonymous/temporary markers

- **WHEN** a caller publishes to `orders.new` or a subscriber processes a message from it
- **THEN** neither span SHALL carry `messaging.destination.temporary` or
  `messaging.destination.anonymous`

#### Scenario: A subject sharing a prefix boundary is not an inbox

- **WHEN** a caller publishes to `_INBOXES.orders`
- **THEN** the span SHALL be named `publish _INBOXES.orders`
- **AND** it SHALL NOT carry `messaging.destination.temporary` or
  `messaging.destination.anonymous`

### Requirement: Request/reply conversation ID

Core-NATS spans SHALL carry `messaging.message.conversation_id` set to the reply inbox subject at every point where the inbox is observable to the wrapper, so that the requester and responder sides of one request/reply exchange are joinable by attribute query:

- **Request "send" (CLIENT) span**: on a successful reply, the wrapper SHALL set `messaging.message.conversation_id` to the reply message's subject (the inbox) before the span ends (a late attribute write from `recordReply`; OTel permits `SetAttributes` any time before `End()`). When the request fails (timeout, cancellation, no responders), the inbox is never observable to the wrapper and the attribute SHALL be omitted — conformant, as the attribute's semconv requirement level is Recommended. Because the write occurs after span start, samplers SHALL NOT be expected to observe it (`messaging.message.conversation_id` is absent from semconv's list of attributes to provide at span creation time).
- **Request "send" span addressed AT an inbox**: when the request's own destination is an inbox, the span already carries `messaging.message.conversation_id` from span start — the conversation the outgoing message belongs to is the one the target inbox identifies — and the late write above SHALL be suppressed. A single attribute SHALL NOT hold two values over one span's lifetime. The nested conversation opened by this request's own reply inbox SHALL remain recorded on the reply-"receive" span, which is the span the reply message belongs to.
- **Reply-"receive" (CLIENT) span**: the wrapper SHALL set `messaging.message.conversation_id` to the reply message's subject (the inbox), in addition to the existing `messaging.destination.name` carrying the same value (structural field vs. join key).
- **Subscription "process" (CONSUMER) span**: when the received message's `Reply` field is non-empty, the wrapper SHALL set `messaging.message.conversation_id` to that `Reply` value. Messages without a `Reply` SHALL NOT carry the attribute.
- **Publish "send" (PRODUCER) span**: unchanged — when the caller's message has a non-empty `Reply` at span start (manual request/reply via `PublishMsg`), the attribute SHALL be set from it at span start.

`oteljetstream` SHALL NOT derive `messaging.message.conversation_id` from a JetStream message's `Reply` field: that field is the `$JS.ACK.…` acknowledgement subject (protocol plumbing, not a conversation identifier). This is a deliberate divergence from the core-NATS attribute builders and SHALL be recorded where the builders instruct keeping the attribute sets in sync. It SHALL, however, carry the attribute when the message's own destination is recognised as an inbox — an inbox is by definition the identifier of the exchange, whatever transport carried the message — so that a stream archiving replies stays joinable by the same query as core NATS.

The wrapper SHALL NOT alter the underlying driver's request mechanics (e.g. pre-generating a reply inbox with its own subscription) to make the inbox observable earlier: instrumentation is behavior-preserving, and replacing the driver's mux-inbox design with per-request subscriptions would change server-side load characteristics.

#### Scenario: Successful round trip joins all three spans

- **WHEN** a caller invokes `Request` (or `RequestWithContext`/`RequestMsg`/`RequestMsgWithContext`) on subject `svc.echo`, an instrumented subscriber responds via `msg.Respond`, and the reply is received
- **THEN** the request "send" span, the reply-"receive" span, and the responder's "process" span SHALL all carry `messaging.message.conversation_id` with the same value — the reply inbox subject
- **AND** on the "send" span the value SHALL equal the reply message's subject

#### Scenario: A request addressed at an inbox keeps its span-start conversation ID

- **WHEN** a caller invokes `Request` on a peer's advertised inbox subject and a reply is received
  on a different, auto-generated reply inbox
- **THEN** the request "send" span's `messaging.message.conversation_id` SHALL equal the target
  inbox it was addressed to, not the reply inbox
- **AND** the reply-"receive" span's `messaging.message.conversation_id` SHALL equal the reply
  inbox

#### Scenario: Failed request omits the attribute

- **WHEN** a `Request` times out or errors before any reply is received
- **THEN** the request "send" span SHALL NOT carry `messaging.message.conversation_id`
- **AND** the span SHALL still record the error status per the existing error-handling behavior

#### Scenario: Fire-and-forget message carries no conversation ID

- **WHEN** a subscriber's handler processes a message published with no `Reply` subject
- **THEN** the "process" span SHALL NOT carry `messaging.message.conversation_id`

#### Scenario: JetStream ack subject is not a conversation ID

- **WHEN** a JetStream consumer receives or processes a message whose `Reply` field carries the `$JS.ACK.…` acknowledgement subject
- **THEN** the JetStream span SHALL NOT carry `messaging.message.conversation_id`

#### Scenario: Manual PublishMsg with explicit Reply keeps span-start attribute

- **WHEN** a caller publishes via `PublishMsg` with `msg.Reply` set to a caller-chosen inbox
- **THEN** the "send" (PRODUCER) span SHALL carry `messaging.message.conversation_id` equal to that `Reply` value, set at span start

### Requirement: Disabled tracing emits no spans or SDK objects

When the tracing gate is off (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` are not both truthy), the wrapper SHALL delegate to the native NATS / JetStream client and run no OTel SDK code path — no real-tracer `Start`, no `TracerProvider`, no exporter, no propagator inject/extract — consistent with the module-wide disabled-mode invariant. Removing the deliver `TracerProvider` shrinks this disabled surface (its init is gone, not merely gated off).

#### Scenario: Tracing disabled delegates to native client

- **WHEN** the tracing gate is off and a caller invokes `Publish` or `Request`, or a subscriber / consumer receives a message
- **THEN** the wrapper SHALL delegate to the native `*nats.Conn` / JetStream client
- **AND** no span SHALL be emitted
- **AND** no `TracerProvider`, `BatchSpanProcessor`, or OTLP exporter SHALL be constructed
- **AND** the trace propagator SHALL NOT be invoked to inject or extract
