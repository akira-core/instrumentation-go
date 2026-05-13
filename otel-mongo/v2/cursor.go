package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// cursorImpl is the polymorphic core of Cursor.
type cursorImpl interface {
	DecodeWithContext(ctx context.Context, val any) (context.Context, error)
	Decode(val any) error
}

var (
	_ cursorImpl = (*traced.Cursor)(nil)
	_ cursorImpl = (*direct.Cursor)(nil)
)

// Cursor wraps *mongo.Cursor with optional trace propagation.
type Cursor struct {
	*mongo.Cursor
	impl cursorImpl
}

// DecodeWithContext decodes the current document into val and returns a
// context enriched with the trace context extracted from the document's
// "_oteltrace" field.
func (c *Cursor) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	return c.impl.DecodeWithContext(ctx, val)
}

// Decode decodes the current document into val.
func (c *Cursor) Decode(val any) error { return c.impl.Decode(val) }

// singleResultImpl is the polymorphic core of SingleResult.
type singleResultImpl interface {
	Decode(v any) error
	TraceContext() context.Context
	Raw() (bson.Raw, error)
}

var (
	_ singleResultImpl = (*traced.SingleResult)(nil)
	_ singleResultImpl = (*direct.SingleResult)(nil)
)

// SingleResult wraps *mongo.SingleResult with optional trace propagation.
type SingleResult struct {
	*mongo.SingleResult
	impl singleResultImpl
}

// Decode decodes the document.
func (r *SingleResult) Decode(v any) error { return r.impl.Decode(v) }

// TraceContext returns a context enriched with the trace context stored in
// the fetched document's "_oteltrace" field.
func (r *SingleResult) TraceContext() context.Context { return r.impl.TraceContext() }

// Raw returns the raw BSON document.
func (r *SingleResult) Raw() (bson.Raw, error) { return r.impl.Raw() }
