package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/traced"
)

// Compile-time checks that the impl-package types satisfy the shared
// polymorphic interfaces consumed by the facade Cursor / SingleResult /
// ChangeStream.
var (
	_ shared.CursorImpl       = (*traced.Cursor)(nil)
	_ shared.CursorImpl       = (*direct.Cursor)(nil)
	_ shared.SingleResultImpl = (*traced.SingleResult)(nil)
	_ shared.SingleResultImpl = (*direct.SingleResult)(nil)
)

// Cursor wraps *mongo.Cursor with optional trace propagation.
type Cursor struct {
	*mongo.Cursor
	impl shared.CursorImpl
}

// DecodeWithContext decodes the current document into val and returns a
// context enriched with the trace context extracted from the document's
// "_oteltrace" field.
func (c *Cursor) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	return c.impl.DecodeWithContext(ctx, val)
}

// Decode decodes the current document into val.
func (c *Cursor) Decode(val any) error { return c.impl.Decode(val) }

// SingleResult wraps *mongo.SingleResult with optional trace propagation.
type SingleResult struct {
	*mongo.SingleResult
	impl shared.SingleResultImpl
}

// Decode decodes the document.
func (r *SingleResult) Decode(v any) error { return r.impl.Decode(v) }

// TraceContext returns a context enriched with the trace context stored in
// the fetched document's "_oteltrace" field.
func (r *SingleResult) TraceContext() context.Context { return r.impl.TraceContext() }

// Raw returns the raw BSON document.
func (r *SingleResult) Raw() (bson.Raw, error) { return r.impl.Raw() }
