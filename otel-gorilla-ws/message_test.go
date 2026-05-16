package otelgorillaws

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalWire_EnvelopeFormat(t *testing.T) {
	carrier := map[string]string{
		TraceparentHeader: "00-12345678901234567890123456789012-0123456789012345-01",
		TracestateHeader:  "k=v",
		"baggage":         "a=b",
	}
	raw, err := marshalWire(carrier, []byte(`{"x":1}`))
	require.NoError(t, err)

	var env wireEnvelope
	require.NoError(t, json.Unmarshal(raw, &env))

	// traceparent and tracestate must be in header
	assert.Equal(t, "00-12345678901234567890123456789012-0123456789012345-01", env.Header[TraceparentHeader])
	assert.Equal(t, "k=v", env.Header[TracestateHeader])

	// baggage must NOT appear in header
	assert.NotContains(t, env.Header, "baggage")

	// original user data must be preserved in data field
	var data map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env.Data, &data))
	var x int
	require.NoError(t, json.Unmarshal(data["x"], &x))
	assert.Equal(t, 1, x)

	// trace fields must NOT appear at top level or inside data
	assert.NotContains(t, data, TraceparentHeader, "traceparent must not leak into data field")
}

func TestMarshalWire_NonObjectPayload_WrappedInEnvelope(t *testing.T) {
	carrier := map[string]string{
		TraceparentHeader: "00-12345678901234567890123456789012-0123456789012345-01",
	}

	cases := []struct {
		payload  string
		wantData string
	}{
		{`"hello"`, `"hello"`},
		{`[1,2,3]`, `[1,2,3]`},
		{`42`, `42`},
	}

	for _, tc := range cases {
		raw, err := marshalWire(carrier, []byte(tc.payload))
		require.NoError(t, err, "marshalWire(%s)", tc.payload)

		var env wireEnvelope
		require.NoError(t, json.Unmarshal(raw, &env), "unmarshal envelope for %s", tc.payload)
		require.NotNil(t, env.Header, "expected non-nil header for payload %s", tc.payload)
		assert.NotEmpty(t, env.Header[TraceparentHeader], "expected traceparent in header for payload %s", tc.payload)
		assert.Equal(t, tc.wantData, string(env.Data), "data field for payload %s", tc.payload)
	}
}

func TestMarshalWire_NonJSONPayload_WrappedAsString(t *testing.T) {
	carrier := map[string]string{
		TraceparentHeader: "00-12345678901234567890123456789012-0123456789012345-01",
	}
	raw, err := marshalWire(carrier, []byte("plain text"))
	require.NoError(t, err)

	var env wireEnvelope
	require.NoError(t, json.Unmarshal(raw, &env))
	// data must be the JSON-encoded string
	var s string
	require.NoError(t, json.Unmarshal(env.Data, &s))
	assert.Equal(t, "plain text", s)
}

func TestTryUnmarshalWire_EnvelopeFormat(t *testing.T) {
	input := `{"header":{"traceparent":"00-aabb-01","tracestate":"k=v"},"data":{"x":1,"y":"hello"}}`
	payload, hdrs, ok := tryUnmarshalWire([]byte(input))
	require.True(t, ok)
	assert.Equal(t, "00-aabb-01", hdrs[TraceparentHeader])
	assert.Equal(t, "k=v", hdrs[TracestateHeader])

	// payload must be the data field contents
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &out))
	var x int
	require.NoError(t, json.Unmarshal(out["x"], &x))
	assert.Equal(t, 1, x)
	// trace fields must not appear in the returned payload
	assert.NotContains(t, out, TraceparentHeader, "traceparent must not appear in returned payload")
}

func TestTryUnmarshalWire_LegacyFlatFormat(t *testing.T) {
	// Old Go flat-inject format — must still be parseable for backward compat.
	input := `{"x":1,"traceparent":"00-aabb-01","tracestate":"k=v","y":"hello"}`
	payload, hdrs, ok := tryUnmarshalWire([]byte(input))
	require.True(t, ok, "expected ok=true for legacy flat format")
	assert.Equal(t, "00-aabb-01", hdrs[TraceparentHeader])
	assert.Equal(t, "k=v", hdrs[TracestateHeader])

	// trace fields must be stripped from the returned payload
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &out))
	assert.NotContains(t, out, TraceparentHeader, "traceparent must be stripped from legacy payload")
	assert.NotContains(t, out, TracestateHeader, "tracestate must be stripped from legacy payload")
	var x int
	require.NoError(t, json.Unmarshal(out["x"], &x))
	assert.Equal(t, 1, x)
}

func TestTryUnmarshalWire_NonObject_ReturnsFalse(t *testing.T) {
	for _, input := range []string{`"hello"`, `[1,2]`, `42`, `not-json`} {
		_, _, ok := tryUnmarshalWire([]byte(input))
		assert.False(t, ok, "tryUnmarshalWire(%q) expected ok=false", input)
	}
}

// TestMarshalWireRoundTripStable verifies the hand-written serializer
// produces output that round-trips through json.Unmarshal into the legacy
// wireEnvelope struct identically — guarantees structural compatibility with
// the JS peer that decodes the envelope.
func TestMarshalWireRoundTripStable(t *testing.T) {
	cases := []struct {
		name    string
		carrier map[string]string
		payload string
	}{
		{
			name:    "tp_and_ts",
			carrier: map[string]string{TraceparentHeader: "00-aaa-bbb-01", TracestateHeader: "k=v"},
			payload: `{"x":1}`,
		},
		{
			name:    "tp_only",
			carrier: map[string]string{TraceparentHeader: "00-aaa-bbb-01"},
			payload: `[1,2,3]`,
		},
		{
			name:    "non_json_payload",
			carrier: map[string]string{TraceparentHeader: "00-aaa-bbb-01"},
			payload: `raw binary text`,
		},
		{
			name:    "empty_carrier",
			carrier: map[string]string{},
			payload: `{"y":"hello"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := marshalWire(tc.carrier, []byte(tc.payload))
			require.NoError(t, err)

			var env wireEnvelope
			require.NoError(t, json.Unmarshal(out, &env), "output must be valid envelope JSON")

			// header round-trip equality
			if tp, ok := tc.carrier[TraceparentHeader]; ok && tp != "" {
				assert.Equal(t, tp, env.Header[TraceparentHeader])
			}
			if ts, ok := tc.carrier[TracestateHeader]; ok && ts != "" {
				assert.Equal(t, ts, env.Header[TracestateHeader])
			}

			// data round-trip: either original JSON payload preserved, or
			// non-JSON wrapped as JSON string.
			if json.Valid([]byte(tc.payload)) {
				assert.JSONEq(t, tc.payload, string(env.Data))
			} else {
				var s string
				require.NoError(t, json.Unmarshal(env.Data, &s))
				assert.Equal(t, tc.payload, s)
			}
		})
	}
}

// TestMarshalWirePoolReuseSafety stresses concurrent marshalWire calls — the
// sync.Pool buffer must not leak between goroutines. Failure mode: garbled
// output where one call's payload bleeds into another.
func TestMarshalWirePoolReuseSafety(t *testing.T) {
	const goroutines = 16
	const iterations = 200

	tpFor := func(g int) string {
		// Construct a unique 32-hex traceID per goroutine so cross-pollution
		// shows up as a header mismatch.
		base := "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		return base + string(rune('a'+g)) + "-bbbbbbbbbbbbbbbb-01"
	}

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			carrier := map[string]string{TraceparentHeader: tpFor(g)}
			payload := []byte(`{"g":` + string(rune('0'+(g%10))) + `}`)
			for i := range iterations {
				out, err := marshalWire(carrier, payload)
				if err != nil {
					t.Errorf("g=%d i=%d: %v", g, i, err)
					return
				}
				var env wireEnvelope
				if err := json.Unmarshal(out, &env); err != nil {
					t.Errorf("g=%d i=%d unmarshal: %v", g, i, err)
					return
				}
				if env.Header[TraceparentHeader] != tpFor(g) {
					t.Errorf("g=%d i=%d header bleed: got %q want %q", g, i, env.Header[TraceparentHeader], tpFor(g))
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestMarshalWireNonJSONPayloadStable repeats the non-JSON wrap behaviour
// from the existing test suite but explicitly asserts the pooled path retains
// the same string-encoding semantics.
func TestMarshalWireNonJSONPayloadStable(t *testing.T) {
	carrier := map[string]string{TraceparentHeader: "00-aaa-bbb-01"}
	out, err := marshalWire(carrier, []byte("plain text"))
	require.NoError(t, err)

	var env wireEnvelope
	require.NoError(t, json.Unmarshal(out, &env))
	var s string
	require.NoError(t, json.Unmarshal(env.Data, &s))
	assert.Equal(t, "plain text", s)
}
