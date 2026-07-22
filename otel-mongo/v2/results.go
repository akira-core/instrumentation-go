package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/traced"
)

// InsertOneResult wraps *mongo.InsertOneResult.
type InsertOneResult struct {
	*mongo.InsertOneResult
}

// InsertManyResult wraps *mongo.InsertManyResult.
type InsertManyResult struct {
	*mongo.InsertManyResult
}

// UpdateResult wraps *mongo.UpdateResult.
type UpdateResult struct {
	*mongo.UpdateResult
}

// DeleteResult wraps *mongo.DeleteResult.
type DeleteResult struct {
	*mongo.DeleteResult
}

// BulkWriteResult wraps *mongo.BulkWriteResult.
type BulkWriteResult struct {
	*mongo.BulkWriteResult
}

// Compile-time checks that the impl-package change-stream types satisfy the
// shared polymorphic interface consumed by the facade ChangeStream.
var (
	_ shared.ChangeStreamImpl = (*traced.ChangeStream)(nil)
	_ shared.ChangeStreamImpl = (*direct.ChangeStream)(nil)
)

// ChangeStream wraps *mongo.ChangeStream with optional trace propagation.
type ChangeStream struct {
	*mongo.ChangeStream
	impl shared.ChangeStreamImpl
}

// Next advances the change stream to the next change document.
func (cs *ChangeStream) Next(ctx context.Context) bool { return cs.ChangeStream.Next(ctx) }

// Decode decodes the current change document into val.
func (cs *ChangeStream) Decode(val any) error { return cs.impl.Decode(val) }

// DecodeAndTrace decodes the current change document into val and returns
// a context enriched with trace context extracted from fullDocument's "_oteltrace".
func (cs *ChangeStream) DecodeAndTrace(ctx context.Context, val any) (context.Context, error) {
	return cs.impl.DecodeAndTrace(ctx, val)
}

// Close closes the change stream.
func (cs *ChangeStream) Close(ctx context.Context) error { return cs.ChangeStream.Close(ctx) }

// Err returns the last error.
func (cs *ChangeStream) Err() error { return cs.ChangeStream.Err() }
