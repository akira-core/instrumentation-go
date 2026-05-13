package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/shared"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// Compile-time checks that the impl-package Cursor types satisfy the shared
// polymorphic interface consumed by the facade Cursor. Adding a method to
// shared.CursorImpl without updating both impls breaks the build here.
var (
	_ shared.CursorImpl = (*traced.Cursor)(nil)
	_ shared.CursorImpl = (*direct.Cursor)(nil)
)

// Cursor wraps *mongo.Cursor with optional trace propagation. The embedded
// *mongo.Cursor preserves the upstream API; DecodeWithContext + Decode delegate
// to a strategy impl chosen at construction time.
type Cursor struct {
	*mongo.Cursor
	impl shared.CursorImpl
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
