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
//
// An inbox destination is dropped from the name only when it is actually unbounded.
// A filter that is nothing but an inbox prefix plus wildcards ("_INBOX.>") is a fixed
// string the subscriber declared, so semconv's first choice for {destination} — use
// messaging.destination.template when available — applies unchanged; the temporary and
// anonymous exclusion is written into the SECOND choice (messaging.destination.name)
// only. The inbox verdict still reports true for such a destination, because it drives
// the temporary/anonymous/conversation_id attributes, which are about the delivery
// rather than about the name.
func Resolve(op, concrete, filter string, inboxPrefixes []string) (name, templateAttr string, inbox bool) {
	dest := filter
	if dest == "" {
		dest = concrete
	}
	if dest == "" {
		return op, "", false
	}
	if prefix := matchInboxPrefix(dest, inboxPrefixes); prefix != "" {
		if !allWildcard(strings.TrimPrefix(dest, prefix)) {
			return op, "", true
		}
		inbox = true
	}
	if dest != concrete {
		templateAttr = dest
	}
	return op + " " + dest, templateAttr, inbox
}

// matchInboxPrefix returns the longest inbox prefix s starts with, or "" if none do.
// Longest wins because one recognised prefix can nest inside another (a custom prefix
// of "_INBOX.svca" sits under the default "_INBOX."), and the remainder that decides
// boundedness has to be measured against the more specific one.
func matchInboxPrefix(s string, prefixes []string) string {
	match := ""
	for _, p := range prefixes {
		if p != "" && len(p) > len(match) && strings.HasPrefix(s, p) {
			match = p
		}
	}
	return match
}

// allWildcard reports whether every token of s is a NATS wildcard. An empty s is not:
// a destination equal to the prefix has no tokens to be bounded by.
func allWildcard(s string) bool {
	if s == "" {
		return false
	}
	for _, token := range strings.Split(s, ".") {
		if token != "*" && token != ">" {
			return false
		}
	}
	return true
}
