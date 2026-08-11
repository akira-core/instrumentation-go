# otel-nats Tracing

Vocabulary for the NATS/JetStream span-naming and attribute model. Exists because
"where a message goes" and "which exchange it belongs to" are different concepts
that coincide on some paths and diverge on others.

## Language

**Destination**:
The subject a message is published to or delivered on. Recorded as
`messaging.destination.name`, always in its concrete form.
_Avoid_: target, topic, channel

**Resolved destination**:
The low-cardinality form of a destination used for span naming: the subscription
or single-valued consumer-filter subject when one exists, otherwise the concrete
subject.
_Avoid_: template subject (that names the attribute, not the resolution)

**Inbox**:
An auto-generated, single-use subject (`_INBOX.<nuid>` or a custom prefix) that
serves as a reply address. Always a temporary, anonymous destination; its
concrete form never appears in a span name, though a prefix-only wildcard
filter over inboxes (`_INBOX.>`) may.
_Avoid_: reply subject (ambiguous — see Reply inbox)

**Conversation**:
One request/reply exchange, identified by the reply inbox that carries its
answer. A message belongs to the conversation whose inbox it is sent into: a
request sent to a peer's advertised inbox belongs to that (outer) conversation,
while the reply to that request belongs to the (nested) conversation identified
by the requester's own reply inbox. Recorded as
`messaging.message.conversation_id`; set once at span start when the destination
is an inbox, otherwise set when the reply arrives.
_Avoid_: correlation (semconv's alias, not this codebase's term)

**Reply inbox**:
The inbox a requester generates for one request (`msg.Reply`). Identifies the
nested conversation of that exchange. Not observable at request-span start —
the NATS client assigns it inside the request call.
_Avoid_: reply-to, response subject
