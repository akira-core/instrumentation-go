package otelnats

import (
	"net/textproto"

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
// then falls back to the MIME-canonical form — producers that canonicalize header
// keys (including messages already sitting in durable streams) still extract.
// The fallback triggers on key absence, not value emptiness: a verbatim key
// present with an empty value wins over a canonical entry, matching Values.
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
	return c.H.Get(textproto.CanonicalMIMEHeaderKey(key))
}

// Values returns all values for key, implementing propagation.ValuesGetter so
// multi-instance headers (e.g. W3C baggage) are not truncated to the first value.
// Same verbatim-first (by key presence), canonical-fallback lookup order as Get.
func (c HeaderCarrier) Values(key string) []string {
	if len(c.H) == 0 {
		return nil
	}
	if vs, ok := c.H[key]; ok {
		return vs
	}
	return c.H.Values(textproto.CanonicalMIMEHeaderKey(key))
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
