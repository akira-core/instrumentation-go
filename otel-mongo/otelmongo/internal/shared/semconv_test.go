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

func TestDBAttributes_EmitsNoServerAttrs(t *testing.T) {
	// Post Decision 2: DBAttributes emits db.* only. server.* is emitted once,
	// post-call, via ServerAttributes (see design.md Decision 2) — so DBAttributes
	// must carry no server.address/server.port key at span start.
	attrs := DBAttributes("db", "coll", "find", 0)
	for _, kv := range attrs {
		k := string(kv.Key)
		assert.NotEqual(t, "server.address", k)
		assert.NotEqual(t, "server.port", k)
	}
}
