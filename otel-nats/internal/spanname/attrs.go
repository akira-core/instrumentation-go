package spanname

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// InboxAttrs marks a span whose destination is a request/reply inbox. The subject is
// auto-generated and single-use, so the span NAME drops it unless a bounded filter
// form is available (see Resolve), while these attributes keep it queryable either
// way: semconv scopes the low-cardinality rule to the span name, and
// messaging.destination.name stays Conditionally Required with no carve-out for
// temporary or anonymous destinations.
//
// messaging.destination.name is set by the caller's own attribute builder; this adds
// the three that identify the destination as an inbox. temporary and anonymous are
// Conditionally Required once true, so recognising an inbox obliges the instrumentation
// to record them. conversation_id is Recommended: an inbox IS the identifier of the
// exchange a message sent into it belongs to.
//
// Lives here rather than in either wrapper package because core NATS and JetStream
// both need it and the inbox concept is already this package's.
func InboxAttrs(subject string) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.MessagingMessageConversationID(subject),
		semconv.MessagingDestinationTemporary(true),
		semconv.MessagingDestinationAnonymous(true),
	}
}
