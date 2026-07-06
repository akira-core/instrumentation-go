package shared

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
)

// CursorImpl is the polymorphic core of the facade Cursor. Satisfied by
// internal/direct.Cursor and internal/traced.Cursor. Declared here so the
// facade collectionImpl interface can name it as a return type without
// importing either path.
type CursorImpl interface {
	DecodeWithContext(ctx context.Context, val any) (context.Context, error)
	Decode(val any) error
}

// SingleResultImpl is the polymorphic core of the facade SingleResult.
type SingleResultImpl interface {
	Decode(v any) error
	TraceContext() context.Context
	Raw() (bson.Raw, error)
}

// ChangeStreamImpl is the polymorphic core of the facade ChangeStream.
type ChangeStreamImpl interface {
	DecodeWithContext(ctx context.Context, val any) (context.Context, error)
	Decode(val any) error
}
