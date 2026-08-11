# Proposal: otel-nats-low-cardinality-span-names

## Why

NATS request/reply replies arrive on auto-generated inbox subjects (`_INBOX.<nuid>`), and wildcard
consumers deliver messages on unbounded concrete subjects — both currently flow verbatim into span
names (`receive _INBOX.7DdzezSzAoWgyNKcSMYUKZ`), so every span-metrics pipeline keyed on span name
explodes in cardinality. Separately, the current names violate the OTel messaging semconv v1.39.0
rule the module already claims (schema URL v1.39.0): the span name SHOULD be
`{messaging.operation.name} {destination}`, but publish spans are named `send {subject}` while
carrying `messaging.operation.name="publish"`, and request spans are named `{subject} request` —
destination-first, wrong order.

## What Changes

- **BREAKING** Every core-NATS span whose resolved destination is a request/reply inbox drops the
  destination from its name entirely — bare `receive`, `publish`, `process` — per semconv v1.39.0
  ("if no low-cardinality destination value exists, omit the `{destination}`"), and gains
  `messaging.destination.temporary=true`, `messaging.destination.anonymous=true` and
  `messaging.message.conversation_id`. `messaging.destination.name` keeps the inbox subject: semconv
  scopes the low-cardinality rule to the span **name** and leaves `destination.name` Conditionally
  Required with no temporary/anonymous carve-out. This covers all three halves of a manual exchange
  — the reply-receive span, a reply published with `conn.Publish(msg.Reply, …)`, and a handler on an
  inbox subscription.
- **BREAKING** Publish span names align with the `messaging.operation.name` attribute already
  emitted: `publish {subject}` replaces `send {subject}` (core NATS and JetStream). Request spans
  become `request {subject}` (operation-first) instead of `{subject} request`.
  `messaging.operation.type` stays `send` — the semconv enum is unchanged.
- JetStream consumer receive/process spans use the consumer's **filter subject** as the span-name
  destination when it is a usable low-cardinality template (e.g. `receive orders.*`), recording it
  as `messaging.destination.template`; the concrete delivered subject stays in
  `messaging.destination.name`. Fallback rules for multi-filter and `>`-catch-all consumers are a
  design decision.
- **No** subject→template option. Subjects that embed identifiers (`orders.12345.created`) keep the
  concrete subject in the span name wherever no subscription or filter subject exists to resolve
  against. semconv forbids instrumentation from inferring `messaging.destination.template`; it may
  only record one already known to the system, which for this module means a subscription subject or
  a consumer filter subject. The residual case belongs to a collector-side span rename.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `otel-nats-spans`: span-naming requirements added (operation-first format, inbox-destination
  omission on every affected span, filter-subject templates) and the attribute set gains
  `messaging.destination.temporary` / `messaging.destination.anonymous` /
  `messaging.destination.template` in the affected scenarios.
- `nats-jetstream-tracing`: JetStream consumer spans derive their span-name destination from the
  consumer filter subject. No change to the public API surface.

## Impact

- Code: `otel-nats/otelnats/` (`conn.go`, `conn_traced.go`), `otel-nats/oteljetstream/`
  (`jetstream.go`, `jetstream_traced.go`, `consumer.go`, `consumer_traced.go`), options plumbing in
  `connect.go`/`env_flags.go` neighborhood. No other module is touched.
- Tests: span-name assertions across `otel-nats` unit tests and `otel-nats/tests/integration`.
- Consumers: dashboards, alerts, and span-metrics keyed on `send {subject}`, `{subject} request`,
  `receive _INBOX.*`, `publish _INBOX.*` or `process _INBOX.*` must migrate — pre-1.0 breaking
  change, released as a **minor** bump per `VERSIONING.md`, documented in
  `otel-nats/CHANGELOG.md`.
- Docs: README span tables and CLAUDE.md notes mentioning span-name shapes.
