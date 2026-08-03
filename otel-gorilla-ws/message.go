package otelgorillaws

import (
	"encoding/json"
)

const (
	// TraceparentHeader is the canonical W3C trace context header key.
	TraceparentHeader = "traceparent"
	// TracestateHeader is the canonical W3C trace context header key.
	TracestateHeader = "tracestate"
)

// wireEnvelope is the on-wire format shared with the JS instrumentation packages.
// Both otel-ws and otel-rxjs-ws produce and consume this exact structure.
type wireEnvelope struct {
	Header map[string]string `json:"header"`
	Data   json.RawMessage   `json:"data"`
}

// marshalWire wraps payload in the envelope format:
//
//	{"header":{"traceparent":"...","tracestate":"..."},"data":<payload>}
//
// All payload types are wrapped — objects, arrays, strings, numbers.
// If payload is not valid JSON (e.g. raw binary), it is JSON-encoded as a string first.
func marshalWire(carrier map[string]string, payload []byte) ([]byte, error) {
	header := make(map[string]string)
	if tp := carrier[TraceparentHeader]; tp != "" {
		header[TraceparentHeader] = tp
	}
	if ts := carrier[TracestateHeader]; ts != "" {
		header[TracestateHeader] = ts
	}

	var rawData json.RawMessage
	if json.Valid(payload) {
		rawData = payload
	} else {
		// Non-JSON bytes (e.g. raw binary text): encode as a JSON string so the
		// data field is always valid JSON, mirroring what the JS packages do.
		encoded, err := json.Marshal(string(payload))
		if err != nil {
			return payload, nil
		}
		rawData = encoded
	}
	return json.Marshal(wireEnvelope{Header: header, Data: rawData})
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
// Returns ok=false — leaving the caller's bytes untouched — for non-JSON input,
// non-object input, and any JSON object carrying NEITHER trace key.
//
// That last case is load-bearing. The legacy branch rebuilds its result by
// re-marshalling a map, and Go serialises maps with keys sorted, so returning
// an ordinary payload through it would hand the application a byte-different
// frame: reordered keys, normalised whitespace, collapsed duplicates. Any
// caller that hashes or signature-verifies the frame would break. A message
// with neither trace key is by definition not a legacy envelope, so there is
// nothing to strip and no reason to rebuild it.
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

	// Neither trace key ⇒ not a legacy envelope. Return the original bytes
	// rather than a re-marshalled, key-sorted copy of them.
	_, hasTraceparent := m[TraceparentHeader]
	_, hasTracestate := m[TracestateHeader]
	if !hasTraceparent && !hasTracestate {
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
