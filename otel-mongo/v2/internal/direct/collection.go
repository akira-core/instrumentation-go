package direct

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
)

// Collection is the passthrough collectionImpl used when the tracing gate is
// off. No spans, no _oteltrace inject, no propagator extract — calls the
// upstream driver directly and returns disabled-strategy impls so downstream
// Cursor / SingleResult / ChangeStream are also passthrough.
type Collection struct {
	Coll *mongo.Collection
}

// NewCollection constructs a passthrough Collection.
func NewCollection(coll *mongo.Collection) *Collection {
	return &Collection{Coll: coll}
}

// InsertOne inserts a single document; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*mongo.InsertOneResult, error) {
	return d.Coll.InsertOne(ctx, document, opts...)
}

// InsertMany inserts multiple documents; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*mongo.InsertManyResult, error) {
	return d.Coll.InsertMany(ctx, documents, opts...)
}

// Find executes a find command and returns the cursor + impl; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*mongo.Cursor, shared.CursorImpl, error) {
	cursor, err := d.Coll.Find(ctx, filter, opts...)
	if err != nil {
		return nil, nil, err
	}
	return cursor, NewCursor(cursor), nil
}

// FindOne executes a find command returning at most one document; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) (*mongo.SingleResult, shared.SingleResultImpl) {
	sr := d.Coll.FindOne(ctx, filter, opts...)
	return sr, NewSingleResult(sr, ctx)
}

// UpdateOne updates one matching document; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error) {
	return d.Coll.UpdateOne(ctx, filter, update, opts...)
}

// UpdateMany updates all matching documents; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateManyOptions]) (*mongo.UpdateResult, error) {
	return d.Coll.UpdateMany(ctx, filter, update, opts...)
}

// ReplaceOne replaces one matching document; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*mongo.UpdateResult, error) {
	return d.Coll.ReplaceOne(ctx, filter, replacement, opts...)
}

// DeleteOne deletes one matching document; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*mongo.DeleteResult, error) {
	return d.Coll.DeleteOne(ctx, filter, opts...)
}

// DeleteMany deletes all matching documents; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*mongo.DeleteResult, error) {
	return d.Coll.DeleteMany(ctx, filter, opts...)
}

// CountDocuments counts documents matching the filter; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	return d.Coll.CountDocuments(ctx, filter, opts...)
}

// Distinct returns distinct values for the field; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult {
	return d.Coll.Distinct(ctx, fieldName, filter, opts...)
}

// Aggregate runs an aggregation pipeline and returns the cursor + impl; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*mongo.Cursor, shared.CursorImpl, error) {
	cursor, err := d.Coll.Aggregate(ctx, pipeline, opts...)
	if err != nil {
		return nil, nil, err
	}
	return cursor, NewCursor(cursor), nil
}

// UpdateByID updates one document by _id; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error) {
	return d.Coll.UpdateByID(ctx, id, update, opts...)
}

// BulkWrite runs multiple write operations; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*mongo.BulkWriteResult, error) {
	return d.Coll.BulkWrite(ctx, models, opts...)
}

// Watch starts a change stream on the collection; passes through to *mongo.Collection (no spans, no propagation).
func (d *Collection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*mongo.ChangeStream, shared.ChangeStreamImpl, error) {
	cs, err := d.Coll.Watch(ctx, pipeline, opts...)
	if err != nil {
		return nil, nil, err
	}
	return cs, NewChangeStream(cs), nil
}
