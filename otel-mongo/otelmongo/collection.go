package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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
	InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*InsertOneResult, error)
	InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*InsertManyResult, error)
	Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*Cursor, error)
	FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *SingleResult
	UpdateOne(ctx context.Context, filter, update any, opts ...*options.UpdateOptions) (*UpdateResult, error)
	UpdateMany(ctx context.Context, filter, update any, opts ...*options.UpdateOptions) (*UpdateResult, error)
	ReplaceOne(ctx context.Context, filter, replacement any, opts ...*options.ReplaceOptions) (*UpdateResult, error)
	DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error)
	DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error)
	CountDocuments(ctx context.Context, filter any, opts ...*options.CountOptions) (int64, error)
	Distinct(ctx context.Context, fieldName string, filter any, opts ...*options.DistinctOptions) ([]interface{}, error)
	Aggregate(ctx context.Context, pipeline any, opts ...*options.AggregateOptions) (*Cursor, error)
	UpdateByID(ctx context.Context, id, update any, opts ...*options.UpdateOptions) (*UpdateResult, error)
	BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...*options.BulkWriteOptions) (*BulkWriteResult, error)
	Watch(ctx context.Context, pipeline interface{}, opts ...*options.ChangeStreamOptions) (*ChangeStream, error)

	tracingOn() bool
	propagationOn() bool
	tracerProbe() trace.Tracer
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
// hands to its Collection facade. Uses the Database's cached gates instead of
// re-reading the env so a single Connect-time decision flows through.
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

// tracingOn reports whether this Collection uses the full instrumentation
// path. Internal helper; mainly useful in tests.
func (c *Collection) tracingOn() bool { return c.impl.tracingOn() }

// propagationOn reports whether this Collection injects _oteltrace metadata
// on writes. Internal helper for tests.
func (c *Collection) propagationOn() bool { return c.impl.propagationOn() }

// tracerProbe exposes the underlying tracer chosen by the impl. Internal
// helper for tests that verify the kill-switch swapped a real tracer for a
// noop tracer.
func (c *Collection) tracerProbe() trace.Tracer { return c.impl.tracerProbe() }

// InsertOne inserts a document. When tracing is enabled, the call is wrapped
// in a CLIENT span and (with propagation on) the deliver span traceparent is
// injected into the document's "_oteltrace" field.
func (c *Collection) InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*InsertOneResult, error) {
	return c.impl.InsertOne(ctx, document, opts...)
}

// InsertMany inserts multiple documents, injecting the deliver span
// traceparent into each "_oteltrace" when propagation is on.
func (c *Collection) InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*InsertManyResult, error) {
	return c.impl.InsertMany(ctx, documents, opts...)
}

// Find executes a find command and returns a Cursor.
func (c *Collection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*Cursor, error) {
	return c.impl.Find(ctx, filter, opts...)
}

// FindOne executes a find command returning at most one document. The span
// (if any) is held in the returned *SingleResult and ended when Decode is called.
func (c *Collection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *SingleResult {
	return c.impl.FindOne(ctx, filter, opts...)
}

// UpdateOne injects the current trace context into the update and replaces
// the document's _oteltrace (when propagation is on).
func (c *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	return c.impl.UpdateOne(ctx, filter, update, opts...)
}

// UpdateMany injects the current trace context into the update for all matched documents.
func (c *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	return c.impl.UpdateMany(ctx, filter, update, opts...)
}

// ReplaceOne injects the current trace context into the replacement document.
func (c *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...*options.ReplaceOptions) (*UpdateResult, error) {
	return c.impl.ReplaceOne(ctx, filter, replacement, opts...)
}

// DeleteOne deletes one matching document.
func (c *Collection) DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	return c.impl.DeleteOne(ctx, filter, opts...)
}

// DeleteMany deletes all documents matching filter.
func (c *Collection) DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	return c.impl.DeleteMany(ctx, filter, opts...)
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
	return c.impl.Aggregate(ctx, pipeline, opts...)
}

// UpdateByID updates one document by _id, injecting the current trace into the update.
func (c *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	return c.impl.UpdateByID(ctx, id, update, opts...)
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
	return c.impl.BulkWrite(ctx, models, opts...)
}

// Watch starts a change stream on the collection.
func (c *Collection) Watch(ctx context.Context, pipeline interface{}, opts ...*options.ChangeStreamOptions) (*ChangeStream, error) {
	return c.impl.Watch(ctx, pipeline, opts...)
}
