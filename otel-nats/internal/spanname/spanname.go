package spanname

import "strings"

// Resolve computes a subject-derived span name, the messaging.destination.template
// attribute value when one applies, and whether the resolved destination is a reply
// inbox.
//
// Precedence: filter (if non-empty) beats concrete. filter is a subject the
// subscriber itself declared — a core-NATS subscription subject or a JetStream
// consumer's single filter subject — so it is a fact the library holds, never a
// guess about which token of a subject is an identifier.
//
// name is "op" alone when the resolved destination is empty or is an inbox, else
// "op destination". semconv v1.39.0 omits the {destination} segment when no
// low-cardinality value is available rather than substituting a literal.
// templateAttr is "" unless the resolved destination DIFFERS from concrete, in
// which case templateAttr equals that destination.
//
// The inbox test runs on the RESOLVED destination, not on concrete: a subscription
// to "<inbox>.>" has a filter that carries the request's nuid just as its delivered
// subjects do, so testing concrete alone would still put an unbounded string in the
// span name.
func Resolve(op, concrete, filter string, inboxPrefixes []string) (name, templateAttr string, inbox bool) {
	dest := filter
	if dest == "" {
		dest = concrete
	}
	if dest == "" || hasAnyPrefix(dest, inboxPrefixes) {
		return op, "", dest != ""
	}
	if dest != concrete {
		templateAttr = dest
	}
	return op + " " + dest, templateAttr, false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
