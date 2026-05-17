package shared

import (
	"encoding/json"
	"strconv"
	"sync"
)

const (
	// Traceparent is the canonical W3C trace context header key.
	Traceparent = "traceparent"
	// Tracestate is the canonical W3C trace context header key.
	Tracestate = "tracestate"
)

// WireEnvelope is the on-wire format shared with the JS instrumentation packages.
// Both otel-ws and otel-rxjs-ws produce and consume this exact structure.
type WireEnvelope struct {
	Header map[string]string `json:"header"`
	Data   json.RawMessage   `json:"data"`
}

// wireBufPool reuses byte buffers across MarshalWire calls.
var wireBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 512)
		return &b
	},
}

// MarshalWire wraps payload in the envelope format:
//
//	{"header":{"traceparent":"...","tracestate":"..."},"data":<payload>}
//
// Hand-written serializer — no reflection, pooled staging buffer.
func MarshalWire(carrier map[string]string, payload []byte) ([]byte, error) {
	bufp := wireBufPool.Get().(*[]byte)
	buf := (*bufp)[:0]
	defer func() {
		if cap(buf) <= 64*1024 {
			*bufp = buf[:0]
			wireBufPool.Put(bufp)
		}
	}()

	buf = append(buf, `{"header":{`...)
	wrote := false
	if tp := carrier[Traceparent]; tp != "" {
		buf = append(buf, `"traceparent":`...)
		buf = strconv.AppendQuote(buf, tp)
		wrote = true
	}
	if ts := carrier[Tracestate]; ts != "" {
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
		buf = strconv.AppendQuote(buf, string(payload))
	}
	buf = append(buf, '}')

	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// TryUnmarshalWire extracts trace headers from an incoming message and returns
// the original user payload. Handles envelope format (current) and legacy
// flat format (backward compat).
func TryUnmarshalWire(data []byte) (payload []byte, headers map[string]string, ok bool) {
	var env WireEnvelope
	if err := json.Unmarshal(data, &env); err == nil && env.Header != nil && env.Data != nil {
		h := make(map[string]string)
		if tp := env.Header[Traceparent]; tp != "" {
			h[Traceparent] = tp
		}
		if ts := env.Header[Tracestate]; ts != "" {
			h[Tracestate] = ts
		}
		return env.Data, h, true
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || len(m) == 0 {
		return nil, nil, false
	}

	h := make(map[string]string)
	if raw, exists := m[Traceparent]; exists {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			h[Traceparent] = s
		}
		delete(m, Traceparent)
	}
	if raw, exists := m[Tracestate]; exists {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			h[Tracestate] = s
		}
		delete(m, Tracestate)
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, false
	}
	return out, h, true
}
