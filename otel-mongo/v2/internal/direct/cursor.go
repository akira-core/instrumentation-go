package direct

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Cursor is the disabled-path passthrough impl of the otelmongo.Cursor strategy.
type Cursor struct {
	cur *mongo.Cursor
}

// NewCursor wraps cur with the disabled-path passthrough Cursor impl.
func NewCursor(cur *mongo.Cursor) *Cursor { return &Cursor{cur: cur} }

// DecodeAndTrace decodes the current document and returns ctx unchanged.
func (c *Cursor) DecodeAndTrace(ctx context.Context, val any) (context.Context, error) {
	if err := c.cur.Decode(val); err != nil {
		return ctx, err
	}
	return ctx, nil
}

// Decode delegates to *mongo.Cursor.Decode.
func (c *Cursor) Decode(val any) error { return c.cur.Decode(val) }
