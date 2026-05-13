package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// cursorImpl is the polymorphic core of Cursor. Two implementations exist
// (internal/traced.Cursor / internal/direct.Cursor); selection happens at
// construction so per-method gates are unnecessary.
type cursorImpl interface {
	DecodeWithContext(ctx context.Context, val any) (context.Context, error)
	Decode(val any) error
}

// Compile-time impl assertions. Forces the impls in internal/traced/ and
// internal/direct/ to satisfy cursorImpl — adding a method to the interface
// without updating both impls breaks the build here.
var (
	_ cursorImpl = (*traced.Cursor)(nil)
	_ cursorImpl = (*direct.Cursor)(nil)
)

// Cursor wraps *mongo.Cursor with optional trace propagation. The embedded
// *mongo.Cursor preserves the upstream API; DecodeWithContext + Decode delegate
// to a strategy impl chosen at construction time.
type Cursor struct {
	*mongo.Cursor
	impl cursorImpl
}

// DecodeWithContext decodes the current document into val and returns a
// context enriched with the trace context extracted from the document's
// "_oteltrace" field. When tracing is off (or the field is absent) the
// returned context is unchanged.
func (c *Cursor) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	return c.impl.DecodeWithContext(ctx, val)
}

// Decode decodes the current document into val.
func (c *Cursor) Decode(val any) error { return c.impl.Decode(val) }
