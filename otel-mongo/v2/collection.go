package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Collection wraps *mongo.Collection. Public methods delegate to a polymorphic
// collectionImpl chosen once at construction time — tracedCollection when the
// tracing gate is on, directCollection (passthrough) when off. The facade
// itself stores no instrumentation state.
type Collection struct {
	*mongo.Collection
	impl collectionImpl
}

// collectionImpl is the polymorphic core of Collection. Two implementations
// exist (tracedCollection / directCollection). Selection happens at
// construction so per-method gates are unnecessary.
type collectionImpl interface {
	InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*InsertOneResult, error)
	InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*InsertManyResult, error)
	Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*Cursor, error)
	FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) *SingleResult
	UpdateOne(ctx context.Context, filter, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error)
	UpdateMany(ctx context.Context, filter, update any, opts ...options.Lister[options.UpdateManyOptions]) (*UpdateResult, error)
	ReplaceOne(ctx context.Context, filter, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*UpdateResult, error)
	DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*DeleteResult, error)
	DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*DeleteResult, error)
	CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error)
	Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult
	Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error)
	UpdateByID(ctx context.Context, id, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error)
	BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*BulkWriteResult, error)
	Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error)
}

// NewCollection wraps an existing *mongo.Collection with trace propagation.
// Document _oteltrace injection follows the env gates:
// OTEL_INSTRUMENTATION_GO_TRACING_ENABLED **and** OTEL_MONGO_TRACING_ENABLED
// must both be on before OTEL_MONGO_PROPAGATION_ENABLED is consulted. When the
// gate is off the returned wrapper is a passthrough — no spans, no
// _oteltrace, no propagator extract.
func NewCollection(coll *mongo.Collection, tracer trace.Tracer, propagator propagation.TextMapPropagator) *Collection {
	if !mongoTracingEnabled() {
		return &Collection{Collection: coll, impl: &directCollection{coll: coll}}
	}
	return &Collection{
		Collection: coll,
		impl: &tracedCollection{
			coll:               coll,
			tracer:             tracer,
			propagator:         propagator,
			propagationEnabled: mongoPropagationEnabled(),
		},
	}
}

// newCollectionForDatabase builds the collectionImpl that Database.Collection
// hands to its Collection facade. Uses the Database's cached gates so a single
// Connect-time decision flows through.
func newCollectionForDatabase(d *Database, raw *mongo.Collection) *Collection {
	if !d.tracingEnabled {
		return &Collection{Collection: raw, impl: &directCollection{coll: raw}}
	}
	return &Collection{
		Collection: raw,
		impl: &tracedCollection{
			coll:               raw,
			tracer:             d.tracer,
			propagator:         d.propagator,
			propagationEnabled: d.propagationEnabled,
			deliverTracer:      d.deliverTracer,
			serverAddr:         d.serverAddr,
			serverPort:         d.serverPort,
		},
	}
}

// InsertOne inserts a document.
func (c *Collection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*InsertOneResult, error) {
	return c.impl.InsertOne(ctx, document, opts...)
}

// InsertMany inserts multiple documents.
func (c *Collection) InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*InsertManyResult, error) {
	return c.impl.InsertMany(ctx, documents, opts...)
}

// Find executes a find command and returns a Cursor.
func (c *Collection) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*Cursor, error) {
	return c.impl.Find(ctx, filter, opts...)
}

// FindOne executes a find command returning at most one document.
func (c *Collection) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) *SingleResult {
	return c.impl.FindOne(ctx, filter, opts...)
}

// UpdateOne updates one matching document.
func (c *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	return c.impl.UpdateOne(ctx, filter, update, opts...)
}

// UpdateMany updates all matching documents.
func (c *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateManyOptions]) (*UpdateResult, error) {
	return c.impl.UpdateMany(ctx, filter, update, opts...)
}

// ReplaceOne replaces one matching document.
func (c *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*UpdateResult, error) {
	return c.impl.ReplaceOne(ctx, filter, replacement, opts...)
}

// DeleteOne deletes one matching document.
func (c *Collection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*DeleteResult, error) {
	return c.impl.DeleteOne(ctx, filter, opts...)
}

// DeleteMany deletes all documents matching filter.
func (c *Collection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*DeleteResult, error) {
	return c.impl.DeleteMany(ctx, filter, opts...)
}

// CountDocuments counts documents matching filter.
func (c *Collection) CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	return c.impl.CountDocuments(ctx, filter, opts...)
}

// Distinct returns distinct values for fieldName.
func (c *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult {
	return c.impl.Distinct(ctx, fieldName, filter, opts...)
}

// Aggregate runs an aggregation pipeline and returns a Cursor.
func (c *Collection) Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error) {
	return c.impl.Aggregate(ctx, pipeline, opts...)
}

// UpdateByID updates one document by _id.
func (c *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	return c.impl.UpdateByID(ctx, id, update, opts...)
}

// DeleteOneByID deletes one document by _id.
func (c *Collection) DeleteOneByID(ctx context.Context, id any, opts ...options.Lister[options.DeleteOneOptions]) (*DeleteResult, error) {
	return c.DeleteOne(ctx, map[string]any{"_id": id}, opts...)
}

// FindOneByID returns a SingleResult for the document with the given _id.
func (c *Collection) FindOneByID(ctx context.Context, id any, opts ...options.Lister[options.FindOneOptions]) *SingleResult {
	return c.FindOne(ctx, map[string]any{"_id": id}, opts...)
}

// FindByIDs returns a Cursor over documents whose _id is in ids.
func (c *Collection) FindByIDs(ctx context.Context, ids []any, opts ...options.Lister[options.FindOptions]) (*Cursor, error) {
	return c.Find(ctx, map[string]any{"_id": map[string]any{"$in": ids}}, opts...)
}

// BulkWrite runs multiple write operations.
func (c *Collection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*BulkWriteResult, error) {
	return c.impl.BulkWrite(ctx, models, opts...)
}

// Watch starts a change stream on the collection.
func (c *Collection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	return c.impl.Watch(ctx, pipeline, opts...)
}
