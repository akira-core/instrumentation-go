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
