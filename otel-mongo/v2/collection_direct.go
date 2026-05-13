package otelmongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// directCollection is the passthrough collectionImpl used when the tracing
// gate is off. No spans, no _oteltrace inject, no propagator extract — calls
// the upstream driver directly and wraps result types with disabled-strategy
// impls so downstream Cursor/SingleResult/ChangeStream are also passthrough.
type directCollection struct {
	coll *mongo.Collection
}

func (d *directCollection) tracingOn() bool     { return false }
func (d *directCollection) propagationOn() bool { return false }

// tracerProbe returns a noop tracer so legacy test paths that capture
// the tracer and call .Start on it see zero recorded spans.
func (d *directCollection) tracerProbe() trace.Tracer {
	return noop.NewTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
}

func (d *directCollection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*InsertOneResult, error) {
	res, err := d.coll.InsertOne(ctx, document, opts...)
	if err != nil {
		return nil, err
	}
	return &InsertOneResult{res}, nil
}

func (d *directCollection) InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*InsertManyResult, error) {
	res, err := d.coll.InsertMany(ctx, documents, opts...)
	if err != nil {
		return nil, err
	}
	return &InsertManyResult{res}, nil
}

func (d *directCollection) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*Cursor, error) {
	cursor, err := d.coll.Find(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return &Cursor{Cursor: cursor}, nil
}

func (d *directCollection) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) *SingleResult {
	sr := d.coll.FindOne(ctx, filter, opts...)
	return &SingleResult{SingleResult: sr, ctx: ctx}
}

func (d *directCollection) UpdateOne(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	res, err := d.coll.UpdateOne(ctx, filter, update, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (d *directCollection) UpdateMany(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateManyOptions]) (*UpdateResult, error) {
	res, err := d.coll.UpdateMany(ctx, filter, update, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (d *directCollection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*UpdateResult, error) {
	res, err := d.coll.ReplaceOne(ctx, filter, replacement, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (d *directCollection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*DeleteResult, error) {
	res, err := d.coll.DeleteOne(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

func (d *directCollection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*DeleteResult, error) {
	res, err := d.coll.DeleteMany(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

func (d *directCollection) CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	return d.coll.CountDocuments(ctx, filter, opts...)
}

func (d *directCollection) Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult {
	return d.coll.Distinct(ctx, fieldName, filter, opts...)
}

func (d *directCollection) Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error) {
	cursor, err := d.coll.Aggregate(ctx, pipeline, opts...)
	if err != nil {
		return nil, err
	}
	return &Cursor{Cursor: cursor}, nil
}

func (d *directCollection) UpdateByID(ctx context.Context, id any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	res, err := d.coll.UpdateByID(ctx, id, update, opts...)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (d *directCollection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*BulkWriteResult, error) {
	res, err := d.coll.BulkWrite(ctx, models, opts...)
	if err != nil {
		return nil, err
	}
	return &BulkWriteResult{res}, nil
}

func (d *directCollection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	cs, err := d.coll.Watch(ctx, pipeline, opts...)
	if err != nil {
		return nil, err
	}
	return &ChangeStream{ChangeStream: cs}, nil
}
