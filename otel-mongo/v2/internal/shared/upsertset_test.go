package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Test_upsertSetField tests the internal upsert helper that injects trace
// metadata into the $set field of an operator update. Moved here from the
// parent package because upsertSetField is private to shared/.
func Test_upsertSetField(t *testing.T) {
	t.Run("existing_set_appends_metadata", func(t *testing.T) {
		doc := bson.D{
			{Key: "$set", Value: bson.D{{Key: "x", Value: 1}}},
		}
		meta := TraceMetadata{Traceparent: "00-abc-1-2-01", Tracestate: ""}

		out, err := upsertSetField(doc, meta)
		require.NoError(t, err)
		require.Len(t, out, 1)
		setDoc, ok := out[0].Value.(bson.D)
		require.True(t, ok)
		require.Len(t, setDoc, 2)
		assert.Equal(t, "x", setDoc[0].Key)
		assert.Equal(t, TraceMetadataKey, setDoc[1].Key)
	})

	t.Run("no_set_creates_set_element", func(t *testing.T) {
		doc := bson.D{{Key: "$inc", Value: bson.D{{Key: "n", Value: 1}}}}
		meta := TraceMetadata{Traceparent: "00-abc-1-2-01", Tracestate: ""}

		out, err := upsertSetField(doc, meta)
		require.NoError(t, err)
		require.Len(t, out, 2)
		var setElem *bson.E
		for i := range out {
			if out[i].Key == "$set" {
				setElem = &out[i]
				break
			}
		}
		require.NotNil(t, setElem)
		setDoc, ok := setElem.Value.(bson.D)
		require.True(t, ok)
		require.Len(t, setDoc, 1)
		assert.Equal(t, TraceMetadataKey, setDoc[0].Key)
	})
}
