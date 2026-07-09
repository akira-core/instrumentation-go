package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerAttributes(t *testing.T) {
	t.Run("empty address returns nil", func(t *testing.T) {
		assert.Nil(t, ServerAttributes("", 27017))
	})

	t.Run("default port omitted", func(t *testing.T) {
		attrs := ServerAttributes("host", 27017)
		require.Len(t, attrs, 1)
		assert.Equal(t, "server.address", string(attrs[0].Key))
		assert.Equal(t, "host", attrs[0].Value.AsString())
	})

	t.Run("non-default port included", func(t *testing.T) {
		attrs := ServerAttributes("host", 27018)
		require.Len(t, attrs, 2)
		assert.Equal(t, "server.address", string(attrs[0].Key))
		assert.Equal(t, "server.port", string(attrs[1].Key))
		assert.Equal(t, int64(27018), attrs[1].Value.AsInt64())
	})

	t.Run("zero port omitted", func(t *testing.T) {
		attrs := ServerAttributes("host", 0)
		require.Len(t, attrs, 1)
		assert.Equal(t, "server.address", string(attrs[0].Key))
	})
}

func TestDBAttributes_UsesServerAttributesInternally(t *testing.T) {
	// DBAttributes and ServerAttributes must not drift: DBAttributes' server.*
	// tail is produced by calling ServerAttributes internally (see design.md
	// Decision 2, "Helper split").
	dbAttrs := DBAttributes("db", "coll", "find", 0, "host", 27018)
	serverAttrs := ServerAttributes("host", 27018)
	require.GreaterOrEqual(t, len(dbAttrs), len(serverAttrs))
	assert.Equal(t, serverAttrs, dbAttrs[len(dbAttrs)-len(serverAttrs):])
}
