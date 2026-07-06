package direct

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"
)

// ChangeStream is the disabled-path passthrough impl of the
// otelmongo.ChangeStream strategy.
type ChangeStream struct {
	cs *mongo.ChangeStream
}

// NewChangeStream wraps cs with the disabled-path passthrough ChangeStream.
func NewChangeStream(cs *mongo.ChangeStream) *ChangeStream { return &ChangeStream{cs: cs} }

// DecodeWithContext decodes the current change document and returns ctx unchanged.
func (c *ChangeStream) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	if err := c.cs.Decode(val); err != nil {
		return ctx, err
	}
	return ctx, nil
}

// Decode delegates to *mongo.ChangeStream.Decode.
func (c *ChangeStream) Decode(val any) error { return c.cs.Decode(val) }
