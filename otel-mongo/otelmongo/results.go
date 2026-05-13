package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
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

// changeStreamImpl is the polymorphic core of ChangeStream. Only the
// strategy-relevant methods are listed — Next/Close/Err stay as facade
// passthroughs against the embedded *mongo.ChangeStream.
type changeStreamImpl interface {
	DecodeWithContext(ctx context.Context, val any) (context.Context, error)
	Decode(val any) error
}

var (
	_ changeStreamImpl = (*traced.ChangeStream)(nil)
	_ changeStreamImpl = (*direct.ChangeStream)(nil)
)

// ChangeStream wraps *mongo.ChangeStream with optional trace propagation.
type ChangeStream struct {
	*mongo.ChangeStream
	impl changeStreamImpl
}

// Next advances the change stream to the next change document.
func (cs *ChangeStream) Next(ctx context.Context) bool { return cs.ChangeStream.Next(ctx) }

// Decode decodes the current change document into val.
func (cs *ChangeStream) Decode(val any) error { return cs.impl.Decode(val) }

// DecodeWithContext decodes the current change document into val and returns
// a context enriched with trace context extracted from fullDocument's
// "_oteltrace" field.
func (cs *ChangeStream) DecodeWithContext(ctx context.Context, val any) (context.Context, error) {
	return cs.impl.DecodeWithContext(ctx, val)
}

// Close closes the change stream.
func (cs *ChangeStream) Close(ctx context.Context) error { return cs.ChangeStream.Close(ctx) }

// Err returns the last error.
func (cs *ChangeStream) Err() error { return cs.ChangeStream.Err() }
