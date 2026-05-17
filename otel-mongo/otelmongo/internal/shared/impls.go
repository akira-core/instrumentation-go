package shared

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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

// CollectionImpl is the polymorphic core of the facade Collection. Satisfied
// by internal/direct.Collection and internal/traced.Collection. Methods
// return raw driver types plus the polymorphic Cursor/SingleResult/ChangeStream
// impls so the impl packages never need to import the facade.
type CollectionImpl interface {
	InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*mongo.InsertOneResult, error)
	InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error)
	Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, CursorImpl, error)
	FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) (*mongo.SingleResult, SingleResultImpl)
	UpdateOne(ctx context.Context, filter, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	UpdateMany(ctx context.Context, filter, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	ReplaceOne(ctx context.Context, filter, replacement any, opts ...*options.ReplaceOptions) (*mongo.UpdateResult, error)
	DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error)
	DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error)
	CountDocuments(ctx context.Context, filter any, opts ...*options.CountOptions) (int64, error)
	Distinct(ctx context.Context, fieldName string, filter any, opts ...*options.DistinctOptions) ([]interface{}, error)
	Aggregate(ctx context.Context, pipeline any, opts ...*options.AggregateOptions) (*mongo.Cursor, CursorImpl, error)
	UpdateByID(ctx context.Context, id, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...*options.BulkWriteOptions) (*mongo.BulkWriteResult, error)
	Watch(ctx context.Context, pipeline interface{}, opts ...*options.ChangeStreamOptions) (*mongo.ChangeStream, ChangeStreamImpl, error)
}
