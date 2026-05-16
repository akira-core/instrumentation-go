package otelgorillaws

import (
	"encoding/json"
	"strconv"
	"sync"
)

const (
	// TraceparentHeader is the canonical W3C trace context header key.
	TraceparentHeader = "traceparent"
	// TracestateHeader is the canonical W3C trace context header key.
	TracestateHeader = "tracestate"
)

// wireEnvelope is the on-wire format shared with the JS instrumentation packages.
// Both otel-ws and otel-rxjs-ws produce and consume this exact structure.
//
// Kept exported as a type for tryUnmarshalWire fallback parsing; marshalWire
// no longer constructs this struct — it writes the JSON form directly via a
// pooled byte buffer to avoid reflection cost on the hot send path.
type wireEnvelope struct {
	Header map[string]string `json:"header"`
	Data   json.RawMessage   `json:"data"`
}

// wireBufPool reuses byte buffers across marshalWire calls so the hot send
// path does not pay heap allocation for the staging buffer per message.
// Initial capacity 512 covers typical WS payloads; larger payloads grow the
// buffer once per pool entry and stabilise after a few cycles.
var wireBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 512)
		return &b
	},
}

// marshalWire wraps payload in the envelope format:
//
//	{"header":{"traceparent":"...","tracestate":"..."},"data":<payload>}
//
// All payload types are wrapped — objects, arrays, strings, numbers.
// If payload is not valid JSON (e.g. raw binary), it is JSON-encoded as a string first.
//
// Hand-written serializer — no reflection, pooled staging buffer.
// Output is structurally equivalent to the previous encoding/json path
// (round-trips through json.Unmarshal into wireEnvelope identically).
func marshalWire(carrier map[string]string, payload []byte) ([]byte, error) {
	bufp := wireBufPool.Get().(*[]byte)
	buf := (*bufp)[:0]
	defer func() {
		// Cap retained capacity to avoid pinning very large buffers in the pool.
		if cap(buf) <= 64*1024 {
			*bufp = buf[:0]
			wireBufPool.Put(bufp)
		}
	}()

	buf = append(buf, `{"header":{`...)
	wrote := false
	if tp := carrier[TraceparentHeader]; tp != "" {
		buf = append(buf, `"traceparent":`...)
		buf = strconv.AppendQuote(buf, tp)
		wrote = true
	}
	if ts := carrier[TracestateHeader]; ts != "" {
		if wrote {
			buf = append(buf, ',')
		}
		buf = append(buf, `"tracestate":`...)
		buf = strconv.AppendQuote(buf, ts)
	}
	buf = append(buf, `},"data":`...)

	if json.Valid(payload) {
		buf = append(buf, payload...)
	} else {
		// Non-JSON bytes: encode as a JSON string so the data field is always
		// valid JSON, mirroring the JS packages.
		buf = strconv.AppendQuote(buf, string(payload))
	}
	buf = append(buf, '}')

	// Copy out to a fresh slice — caller keeps a reference and the buffer
	// goes back to the pool. This is the one unavoidable alloc.
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// tryUnmarshalWire extracts trace headers from an incoming message and returns
// the original user payload. It handles two formats:
//
//  1. Envelope format (new, compatible with JS packages):
//     {"header":{"traceparent":"...","tracestate":"..."},"data":<payload>}
//
//  2. Legacy flat format (backward compat with old Go-only deployments):
//     {"traceparent":"...","tracestate":"...","field1":"value1"}
//
// Returns ok=false for non-JSON or non-object input.
func tryUnmarshalWire(data []byte) (payload []byte, headers map[string]string, ok bool) {
	// 1. Try envelope format first (JS packages + new Go code).
	var env wireEnvelope
	if err := json.Unmarshal(data, &env); err == nil && env.Header != nil && env.Data != nil {
		h := make(map[string]string)
		if tp := env.Header[TraceparentHeader]; tp != "" {
			h[TraceparentHeader] = tp
		}
		if ts := env.Header[TracestateHeader]; ts != "" {
			h[TracestateHeader] = ts
		}
		return env.Data, h, true
	}

	// 2. Fallback: legacy flat format (old Go clients).
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || len(m) == 0 {
		return nil, nil, false
	}

	h := make(map[string]string)
	if raw, exists := m[TraceparentHeader]; exists {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			h[TraceparentHeader] = s
		}
		delete(m, TraceparentHeader)
	}
	if raw, exists := m[TracestateHeader]; exists {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			h[TracestateHeader] = s
		}
		delete(m, TracestateHeader)
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, false
	}
	return out, h, true
}
