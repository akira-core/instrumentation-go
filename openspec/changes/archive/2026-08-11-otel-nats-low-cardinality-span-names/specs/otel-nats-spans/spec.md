# otel-nats-spans Delta

## ADDED Requirements

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
- Any core-NATS span whose resolved destination is a request/reply inbox SHALL omit the
  destination segment entirely and be named by its operation verb alone — `receive`, `publish`,
  or `process` (**BREAKING**: was `receive {inbox}`, `publish {inbox}`, `process {inbox}`). A
  reply inbox is an auto-generated, single-use subject, so no low-cardinality destination value
  exists and semconv directs omitting the `{destination}` segment. This covers the reply-receive
  span of a `Request`, a reply published with `conn.Publish(msg.Reply, …)`, and a handler on an
  inbox subscription.
- A destination SHALL be recognised as an inbox by subject prefix, testing the **resolved**
  destination rather than the concrete subject. Two prefixes SHALL be recognised: the
  connection's own (`nats.CustomInboxPrefix(p)` yielding `p + "."`) and the default `_INBOX.`
  unconditionally. The reply-receive span SHALL be treated as an inbox unconditionally, without
  a prefix test, since it is structurally always one.

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

### Requirement: Span-name destination resolution and template attribute

The `{destination}` segment of a subject-derived span name SHALL resolve in this order:

1. The subscription or consumer filter subject, where one exists and is single-valued: core
   NATS process spans use the subscription's subject (existing behavior, now normative);
   JetStream consumer receive/process spans use the consumer's filter subject when the
   consumer has exactly one.
2. The concrete message subject.

The resolved destination SHALL then be omitted from the span name when it is an inbox, per the
requirement above.

Whenever the resolved destination differs from the concrete message subject (a wildcard-bearing
subscription or filter), the span SHALL carry `messaging.destination.template` set to the
resolved destination. A span whose destination was omitted as an inbox SHALL NOT carry
`messaging.destination.template`. `messaging.destination.name` SHALL always carry the concrete
message subject regardless of what the span name uses.

#### Scenario: Wildcard subscription process span uses the subscription subject

- **WHEN** a handler subscribed to `orders.*` processes a message delivered on `orders.12345`
- **THEN** the CONSUMER span SHALL be named `process orders.*`
- **AND** the span SHALL carry `messaging.destination.template=orders.*` and
  `messaging.destination.name=orders.12345`

#### Scenario: Exact-subject destination sets no template attribute

- **WHEN** a caller publishes to the literal subject `orders.new`
- **THEN** the span SHALL be named `publish orders.new`
- **AND** the span SHALL NOT carry `messaging.destination.template`

## MODIFIED Requirements

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
