package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/traced"
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

// Compile-time impl assertions.
var (
	_ shared.SingleResultImpl = (*traced.SingleResult)(nil)
	_ shared.SingleResultImpl = (*direct.SingleResult)(nil)
	_ shared.ChangeStreamImpl = (*traced.ChangeStream)(nil)
	_ shared.ChangeStreamImpl = (*direct.ChangeStream)(nil)
)

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
// a context enriched with trace context extracted from fullDocument's
// "_oteltrace" field.
func (cs *ChangeStream) DecodeAndTrace(ctx context.Context, val any) (context.Context, error) {
	return cs.impl.DecodeAndTrace(ctx, val)
}

// Close closes the change stream.
func (cs *ChangeStream) Close(ctx context.Context) error { return cs.ChangeStream.Close(ctx) }

// Err returns the last error.
func (cs *ChangeStream) Err() error { return cs.ChangeStream.Err() }
