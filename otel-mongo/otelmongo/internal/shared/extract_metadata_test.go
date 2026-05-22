package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestExtractMetadataFromRaw_DirectLookupParity verifies that the
// allocation-free direct-lookup implementation returns the same
// TraceMetadata that the previous reflection-based bson.Unmarshal path did.
func TestExtractMetadataFromRaw_DirectLookupParity(t *testing.T) {
	cases := []struct {
		name string
		meta TraceMetadata
	}{
		{"traceparent_only", TraceMetadata{Traceparent: "00-12345678901234567890123456789012-0123456789012345-01"}},
		{"traceparent_and_tracestate", TraceMetadata{
			Traceparent: "00-abcdef00112233445566778899aabbcc-1122334455667788-01",
			Tracestate:  "vendor=value,other=v2",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := bson.D{
				{Key: "field", Value: "v"},
				{Key: TraceMetadataKey, Value: tc.meta},
			}
			raw, err := bson.Marshal(doc)
			require.NoError(t, err)

			got, ok := ExtractMetadataFromRaw(raw)
			require.True(t, ok, "expected metadata extracted")
			assert.Equal(t, tc.meta.Traceparent, got.Traceparent)
			assert.Equal(t, tc.meta.Tracestate, got.Tracestate)
		})
	}
}

func TestExtractMetadataFromRaw_Missing(t *testing.T) {
	raw, err := bson.Marshal(bson.D{{Key: "field", Value: "v"}})
	require.NoError(t, err)
	_, ok := ExtractMetadataFromRaw(raw)
	assert.False(t, ok)
}

func TestExtractMetadataFromRaw_WrongType(t *testing.T) {
	// _oteltrace stored as a string instead of a sub-document.
	raw, err := bson.Marshal(bson.D{{Key: TraceMetadataKey, Value: "not-a-document"}})
	require.NoError(t, err)
	_, ok := ExtractMetadataFromRaw(raw)
	assert.False(t, ok)
}

func TestExtractMetadataFromRaw_EmptyTraceparent(t *testing.T) {
	raw, err := bson.Marshal(bson.D{{Key: TraceMetadataKey, Value: bson.D{
		{Key: "traceparent", Value: ""},
		{Key: "tracestate", Value: "vendor=v"},
	}}})
	require.NoError(t, err)
	_, ok := ExtractMetadataFromRaw(raw)
	assert.False(t, ok, "empty traceparent must be treated as absent")
}

func BenchmarkExtractMetadataFromRaw(b *testing.B) {
	doc := bson.D{
		{Key: "_id", Value: "x"},
		{Key: "field", Value: "value"},
		{Key: TraceMetadataKey, Value: TraceMetadata{
			Traceparent: "00-12345678901234567890123456789012-0123456789012345-01",
			Tracestate:  "vendor=value",
		}},
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, ok := ExtractMetadataFromRaw(raw)
		if !ok {
			b.Fatal("expected ok")
		}
	}
}

// TestExtractMetadataFromBsonD_PositionInvariance asserts the function finds
// the _oteltrace key at any position in the bson.D — head, middle, or tail.
func TestExtractMetadataFromBsonD_PositionInvariance(t *testing.T) {
	otel := bson.E{Key: TraceMetadataKey, Value: bson.D{
		{Key: "traceparent", Value: "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"},
	}}
	cases := map[string]bson.D{
		"head_position":   {otel, {Key: "a", Value: 1}, {Key: "b", Value: 2}, {Key: "c", Value: 3}},
		"middle_position": {{Key: "a", Value: 1}, otel, {Key: "b", Value: 2}, {Key: "c", Value: 3}},
		"tail_position":   {{Key: "a", Value: 1}, {Key: "b", Value: 2}, {Key: "c", Value: 3}, otel},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			meta, ok := ExtractMetadataFromBsonD(d)
			require.True(t, ok)
			assert.Equal(t, "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01", meta.Traceparent)
		})
	}
}

// TestExtractMetadataFromBsonD_ReverseScanFindsLast documents (and locks in)
// the reverse-scan optimisation: when the same key appears more than once
// the LAST occurrence wins. Wrapper-injected docs always append the entry
// last, so reverse scan keeps inject/extract symmetric. Any future flip to
// forward scan breaks this assertion.
func TestExtractMetadataFromBsonD_ReverseScanFindsLast(t *testing.T) {
	first := bson.E{Key: TraceMetadataKey, Value: bson.D{
		{Key: "traceparent", Value: "00-1111111111111111111111111111111a-1111111111111111-01"},
	}}
	second := bson.E{Key: TraceMetadataKey, Value: bson.D{
		{Key: "traceparent", Value: "00-2222222222222222222222222222222a-2222222222222222-01"},
	}}
	d := bson.D{first, {Key: "value", Value: "x"}, second}
	meta, ok := ExtractMetadataFromBsonD(d)
	require.True(t, ok)
	assert.Equal(t, "00-2222222222222222222222222222222a-2222222222222222-01", meta.Traceparent,
		"reverse scan must return the last occurrence")
}

func BenchmarkExtractMetadataFromBsonD_TailVsHead(b *testing.B) {
	const n = 32
	make1 := func(otelAt int) bson.D {
		out := make(bson.D, 0, n+1)
		for i := range n {
			if i == otelAt {
				out = append(out, bson.E{Key: TraceMetadataKey, Value: bson.D{{Key: "traceparent", Value: "00-deadbeefdeadbeefdeadbeefdeadbeef-1111111111111111-01"}}})
				continue
			}
			out = append(out, bson.E{Key: "field", Value: i})
		}
		if otelAt >= n {
			out = append(out, bson.E{Key: TraceMetadataKey, Value: bson.D{{Key: "traceparent", Value: "00-deadbeefdeadbeefdeadbeefdeadbeef-1111111111111111-01"}}})
		}
		return out
	}
	tail := make1(n)
	head := make1(0)

	b.Run("Tail", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = ExtractMetadataFromBsonD(tail)
		}
	})
	b.Run("Head", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = ExtractMetadataFromBsonD(head)
		}
	})
}
