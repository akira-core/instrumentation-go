package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/shared"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// Collection wraps *mongo.Collection. Public methods delegate to a polymorphic
// collectionImpl chosen once at construction time — *traced.Collection when
// the tracing gate is on, *direct.Collection (passthrough) when off. The
// facade itself stores no instrumentation state; impls live in
// internal/{direct,traced} so the disabled-mode invariant (no OTel SDK
// reachable from the direct path) is compiler-enforced by package boundary.
type Collection struct {
	*mongo.Collection
	impl collectionImpl
}

// collectionImpl is a facade-local alias of the shared interface. Declared
// in internal/shared so DatabaseImpl.Collection can return it without
// importing the facade — same trick as ClientImpl / DatabaseImpl.
type collectionImpl = shared.CollectionImpl

var (
	_ collectionImpl = (*traced.Collection)(nil)
	_ collectionImpl = (*direct.Collection)(nil)
)

// NewCollection wraps an existing *mongo.Collection with trace propagation.
// Document _oteltrace injection follows the env gates:
// OTEL_INSTRUMENTATION_GO_TRACING_ENABLED **and** OTEL_MONGO_TRACING_ENABLED
// must both be on before OTEL_MONGO_PROPAGATION_ENABLED is consulted. When the
// gate is off the returned wrapper is a passthrough — no spans, no
// _oteltrace, no propagator extract.
func NewCollection(coll *mongo.Collection, tracer trace.Tracer, propagator propagation.TextMapPropagator) *Collection {
	if !mongoTracingEnabled() {
		return &Collection{Collection: coll, impl: direct.NewCollection(coll)}
	}
	return &Collection{
		Collection: coll,
		impl: &traced.Collection{
			Coll:               coll,
			Tracer:             tracer,
			Propagator:         propagator,
			PropagationEnabled: mongoPropagationEnabled(),
		},
	}
}

// InsertOne inserts a document. When tracing is enabled, the call is wrapped
// in a CLIENT span and (with propagation on) the deliver span traceparent is
// injected into the document's "_oteltrace" field.
func (c *Collection) InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*InsertOneResult, error) {
	res, err := c.impl.InsertOne(ctx, document, opts...)
	if err != nil {
		return nil, err
	}
	return &InsertOneResult{res}, nil
}

// InsertMany inserts multiple documents, injecting the deliver span
// traceparent into each "_oteltrace" when propagation is on.
func (c *Collection) InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*InsertManyResult, error) {
	res, err := c.impl.InsertMany(ctx, documents, opts...)
	if err != nil {
		return nil, err
	}
	return &InsertManyResult{res}, nil
}

// Find executes a find command and returns a Cursor.
func (c *Collection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*Cursor, error) {
	raw, cImpl, err := c.impl.Find(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return &Cursor{Cursor: raw, impl: cImpl}, nil
}

// FindOne executes a find command returning at most one document. The span
// (if any) is held in the returned *SingleResult and ended when Decode is called.
func (c *Collection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *SingleResult {
	raw, sImpl := c.impl.FindOne(ctx, filter, opts...)
	return &SingleResult{SingleResult: raw, impl: sImpl}
}

// UpdateOne injects the current trace context into the update and replaces
// the document's _oteltrace (when propagation is on).
func (c *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	res, err := c.impl.UpdateOne(ctx, filter, update, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// UpdateMany injects the current trace context into the update for all matched documents.
func (c *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	res, err := c.impl.UpdateMany(ctx, filter, update, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// ReplaceOne injects the current trace context into the replacement document.
func (c *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...*options.ReplaceOptions) (*UpdateResult, error) {
	res, err := c.impl.ReplaceOne(ctx, filter, replacement, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// DeleteOne deletes one matching document.
func (c *Collection) DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	res, err := c.impl.DeleteOne(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

// DeleteMany deletes all documents matching filter.
func (c *Collection) DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	res, err := c.impl.DeleteMany(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

// CountDocuments counts documents matching filter.
func (c *Collection) CountDocuments(ctx context.Context, filter any, opts ...*options.CountOptions) (int64, error) {
	return c.impl.CountDocuments(ctx, filter, opts...)
}

// Distinct returns distinct values for fieldName.
func (c *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...*options.DistinctOptions) ([]interface{}, error) {
	return c.impl.Distinct(ctx, fieldName, filter, opts...)
}

// Aggregate runs an aggregation pipeline and returns a Cursor.
func (c *Collection) Aggregate(ctx context.Context, pipeline any, opts ...*options.AggregateOptions) (*Cursor, error) {
	raw, cImpl, err := c.impl.Aggregate(ctx, pipeline, opts...)
	if err != nil {
		return nil, err
	}
	return &Cursor{Cursor: raw, impl: cImpl}, nil
}

// UpdateByID updates one document by _id, injecting the current trace into the update.
func (c *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	res, err := c.impl.UpdateByID(ctx, id, update, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// DeleteOneByID deletes one document by _id.
func (c *Collection) DeleteOneByID(ctx context.Context, id any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	return c.DeleteOne(ctx, map[string]any{"_id": id}, opts...)
}

// FindOneByID returns a SingleResult for the document with the given _id.
func (c *Collection) FindOneByID(ctx context.Context, id any, opts ...*options.FindOneOptions) *SingleResult {
	return c.FindOne(ctx, map[string]any{"_id": id}, opts...)
}

// FindByIDs returns a Cursor over documents whose _id is in ids.
func (c *Collection) FindByIDs(ctx context.Context, ids []any, opts ...*options.FindOptions) (*Cursor, error) {
	return c.Find(ctx, map[string]any{"_id": map[string]any{"$in": ids}}, opts...)
}

// BulkWrite runs multiple write operations, injecting trace context into write models.
func (c *Collection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...*options.BulkWriteOptions) (*BulkWriteResult, error) {
	res, err := c.impl.BulkWrite(ctx, models, opts...)
	if err != nil {
		return nil, err
	}
	return &BulkWriteResult{res}, nil
}

// Watch starts a change stream on the collection.
func (c *Collection) Watch(ctx context.Context, pipeline interface{}, opts ...*options.ChangeStreamOptions) (*ChangeStream, error) {
	raw, csImpl, err := c.impl.Watch(ctx, pipeline, opts...)
	if err != nil {
		return nil, err
	}
	return &ChangeStream{ChangeStream: raw, impl: csImpl}, nil
}
