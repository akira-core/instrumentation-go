# otel-nats-spans Delta: nats-request-conversation-id

## MODIFIED Requirements

### Requirement: NATS span attribute set

Message spans SHALL carry OTel messaging-semconv attributes: `messaging.system`, `messaging.destination.name`, `messaging.operation.type`, `messaging.operation.name`, `messaging.message.body.size` (when body non-empty), plus `server.address` / `server.port`. Conditional attributes SHALL be set when their value exists: `messaging.message.conversation_id` (per the dedicated "Request/reply conversation ID" requirement), `messaging.consumer.group.name` (queue group). `messaging.operation.type` for a pull-receive span SHALL be `receive`.

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

## ADDED Requirements

### Requirement: Request/reply conversation ID

Core-NATS spans SHALL carry `messaging.message.conversation_id` set to the reply inbox subject at every point where the inbox is observable to the wrapper, so that the requester and responder sides of one request/reply exchange are joinable by attribute query:

- **Request "send" (CLIENT) span**: on a successful reply, the wrapper SHALL set `messaging.message.conversation_id` to the reply message's subject (the inbox) before the span ends (a late attribute write from `recordReply`; OTel permits `SetAttributes` any time before `End()`). When the request fails (timeout, cancellation, no responders), the inbox is never observable to the wrapper and the attribute SHALL be omitted — conformant, as the attribute's semconv requirement level is Recommended. Because the write occurs after span start, samplers SHALL NOT be expected to observe it.
- **Reply-"receive" (CLIENT) span**: the wrapper SHALL set `messaging.message.conversation_id` to the reply message's subject (the inbox), in addition to the existing `messaging.destination.name` carrying the same value (structural field vs. join key).
- **Subscription "process" (CONSUMER) span**: when the received message's `Reply` field is non-empty, the wrapper SHALL set `messaging.message.conversation_id` to that `Reply` value. Messages without a `Reply` SHALL NOT carry the attribute.
- **Publish "send" (PRODUCER) span**: unchanged — when the caller's message has a non-empty `Reply` at span start (manual request/reply via `PublishMsg`), the attribute SHALL be set from it at span start.

`oteljetstream` spans SHALL NOT carry `messaging.message.conversation_id`: a JetStream message's `Reply` field is the `$JS.ACK.…` acknowledgement subject (protocol plumbing, not a conversation identifier). This is a deliberate divergence from the core-NATS attribute builders and SHALL be recorded where the builders instruct keeping the attribute sets in sync.

The wrapper SHALL NOT alter the underlying driver's request mechanics (e.g. pre-generating a reply inbox with its own subscription) to make the inbox observable earlier: instrumentation is behavior-preserving, and replacing the driver's mux-inbox design with per-request subscriptions would change server-side load characteristics.

#### Scenario: Successful round trip joins all three spans

- **WHEN** a caller invokes `Request` (or `RequestWithContext`/`RequestMsg`/`RequestMsgWithContext`) on subject `svc.echo`, an instrumented subscriber responds via `msg.Respond`, and the reply is received
- **THEN** the request "send" span, the reply-"receive" span, and the responder's "process" span SHALL all carry `messaging.message.conversation_id` with the same value — the reply inbox subject
- **AND** on the "send" span the value SHALL equal the reply message's subject

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
