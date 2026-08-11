# nats-jetstream-tracing Delta

## ADDED Requirements

### Requirement: No caller-supplied subject template mapping

The connection API SHALL NOT offer an option accepting a caller-defined subject→template
function. Span-name destinations SHALL be derived only from subjects the library already holds:
the subscription subject passed to `Subscribe`/`QueueSubscribe`, and a JetStream consumer's
single-valued filter subject.

- A subject that embeds an identifier (`orders.12345.created`) and has no subscription or
  filter subject to resolve against SHALL keep the concrete subject as its span-name
  `{destination}`.
- The wrapper SHALL NOT infer a template from a subject's shape (numeric tokens, UUID-like
  tokens, token counts, or any other heuristic).

#### Scenario: Publish to an ID-bearing subject keeps the concrete subject

- **WHEN** a caller publishes to `orders.12345.created` on a connection with no subscription
  or filter subject involved
- **THEN** the PRODUCER span SHALL be named `publish orders.12345.created`
- **AND** the span SHALL NOT carry `messaging.destination.template`

### Requirement: JetStream consumer spans derive their destination from the filter subject

JetStream consumer receive/process spans SHALL resolve their span-name `{destination}` as
follows:

- A consumer with exactly one filter subject SHALL use that filter subject. When the filter
  contains a wildcard token (`*` or `>`), the span SHALL additionally carry
  `messaging.destination.template` set to the filter subject.
- A consumer with multiple filter subjects, or whose filter configuration is not observable
  to the wrapper, SHALL fall back to the concrete delivered subject. It SHALL NOT omit the
  destination, join the filters into one value, or select one filter arbitrarily.
- `messaging.destination.name` SHALL always carry the concrete delivered subject.

#### Scenario: Wildcard filter consumer emits one span name

- **WHEN** a consumer with the single filter subject `orders.*` receives messages delivered
  on `orders.1`, `orders.2`, … with tracing enabled
- **THEN** every receive span SHALL be named `receive orders.*`
- **AND** each span SHALL carry `messaging.destination.template=orders.*` and its own
  concrete `messaging.destination.name`

#### Scenario: Exact filter keeps current naming

- **WHEN** a consumer with the single exact filter subject `orders.new` receives a message
- **THEN** the receive span SHALL be named `receive orders.new` with no
  `messaging.destination.template` attribute

#### Scenario: Multi-filter consumer falls back to the delivered subject

- **WHEN** a consumer configured with filter subjects `orders.new` and `orders.cancelled`
  receives a message on `orders.cancelled`
- **THEN** the receive span SHALL be named `receive orders.cancelled`
