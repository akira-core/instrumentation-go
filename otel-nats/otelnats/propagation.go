package otelnats

import (
	"net/textproto"
	"strings"

	"go.opentelemetry.io/otel/propagation"

	nats "github.com/nats-io/nats.go"
)

// HeaderCarrier adapts nats.Header to propagation.TextMapCarrier for W3C TraceContext inject/extract.
// Used by oteljetstream and by Conn internally.
type HeaderCarrier struct {
	H nats.Header
}

// Get returns the first value for key from the underlying header. nats.Header is
// case-sensitive (unlike http.Header), so this looks up the verbatim key first,
// then falls back to the MIME-canonical form, then to a case-insensitive scan of
// all keys — producers that canonicalize header keys (including messages already
// sitting in durable streams), or that use some other casing entirely (e.g.
// all-caps "TRACEPARENT"), still extract. The fallback triggers on key absence,
// not value emptiness: a verbatim key present with an empty value wins over a
// canonical or case-insensitive entry, matching Values.
func (c HeaderCarrier) Get(key string) string {
	if len(c.H) == 0 {
		return ""
	}
	if vs, ok := c.H[key]; ok {
		if len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	if vs, ok := c.H[textproto.CanonicalMIMEHeaderKey(key)]; ok {
		if len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	if vs, ok := c.lookupFold(key); ok {
		if len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	return ""
}

// Values returns all values for key, implementing propagation.ValuesGetter so
// multi-instance headers (e.g. W3C baggage) are not truncated to the first value.
// Same verbatim-first (by key presence), canonical-fallback, then case-insensitive
// fallback lookup order as Get.
func (c HeaderCarrier) Values(key string) []string {
	if len(c.H) == 0 {
		return nil
	}
	if vs, ok := c.H[key]; ok {
		return vs
	}
	if vs, ok := c.H[textproto.CanonicalMIMEHeaderKey(key)]; ok {
		return vs
	}
	if vs, ok := c.lookupFold(key); ok {
		return vs
	}
	return nil
}

// lookupFold performs a case-insensitive linear scan over the header's keys,
// used as the last-resort fallback once verbatim and MIME-canonical lookups
// both miss (e.g. a producer wrote "TRACEPARENT" or "TraceParent"). If multiple
// keys differ only by casing and both prior exact-match stages miss the queried
// key, which one wins is unspecified (Go map iteration order) — accepted as
// fine since that is pathological/adversarial input, not one this fallback
// needs to make deterministic.
func (c HeaderCarrier) lookupFold(key string) ([]string, bool) {
	for k, vs := range c.H {
		if strings.EqualFold(k, key) {
			return vs, true
		}
	}
	return nil, false
}

// Set sets key to value in the underlying header, always writing the verbatim
// key. The canonical fallback in Get/Values is read-side only, so this does not
// change what current writers put on the wire.
func (c HeaderCarrier) Set(key, value string) {
	if c.H == nil {
		return
	}
	c.H.Set(key, value)
}

// Keys returns all keys in the underlying header.
func (c HeaderCarrier) Keys() []string {
	if c.H == nil {
		return nil
	}
	keys := make([]string, 0, len(c.H))
	for k := range c.H {
		keys = append(keys, k)
	}
	return keys
}

var _ propagation.TextMapCarrier = (*HeaderCarrier)(nil)
var _ propagation.ValuesGetter = (*HeaderCarrier)(nil)
