package traced

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/shared"
)

// Collection is the fully-instrumented collectionImpl: wraps every CRUD method
// with a CLIENT span, a deliver CONSUMER span, and (when PropagationEnabled)
// _oteltrace document injection. Constructed by the facade NewCollection /
// newCollectionForDatabase only when the tracing gate is on.
//
// Fields are exported so tests in the facade package (and elsewhere within
// this module) can build Collection literals for unit testing without going
// through a constructor.
type Collection struct {
	Coll               *mongo.Collection
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
	DeliverTracer      trace.Tracer
	ServerAddr         string
	ServerPort         int
}

func (t *Collection) dbAndColl() (dbName, collName string) {
	collName = t.Coll.Name()
	if t.Coll.Database() != nil {
		dbName = t.Coll.Database().Name()
	}
	return dbName, collName
}

// StartDeliverSpan creates a synthetic CONSUMER span representing MongoDB broker delivery.
// The returned context carries the deliver span, suitable for injecting into documents so
// change stream consumers link to it. When DeliverTracer is nil, returns a no-op span safe to End.
func (t *Collection) StartDeliverSpan(ctx context.Context, dbName, collName string) (context.Context, trace.Span) {
	if t.DeliverTracer == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	deliverCtx, span := t.DeliverTracer.Start(ctx,
		dbName+"."+collName+" deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(shared.DeliverAttributes(dbName, collName, t.ServerAddr, t.ServerPort)...),
	)
	return deliverCtx, span
}

// InsertOne inserts a single document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*mongo.InsertOneResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "insert", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docToInsert := document
	if t.PropagationEnabled {
		docWithTrace, err := shared.InjectTraceIntoDocument(injectCtx, document, t.Propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		docToInsert = docWithTrace
	}
	res, err := t.Coll.InsertOne(injectCtx, docToInsert, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// InsertMany inserts multiple documents; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*mongo.InsertManyResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "insert", len(documents), t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docsToInsert := documents
	if t.PropagationEnabled {
		docsWithTrace := make([]any, 0, len(documents))
		for _, doc := range documents {
			d, err := shared.InjectTraceIntoDocument(injectCtx, doc, t.Propagator)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
			}
			docsWithTrace = append(docsWithTrace, d)
		}
		docsToInsert = docsWithTrace
	}
	res, err := t.Coll.InsertMany(injectCtx, docsToInsert, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Find executes a find command and returns the cursor + impl; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*mongo.Cursor, shared.CursorImpl, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "find", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := t.Coll.Find(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, nil, err
	}
	return cursor, NewCursor(cursor, t.Tracer, t.Propagator, t.PropagationEnabled), nil
}

// FindOne executes a find command returning at most one document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) (*mongo.SingleResult, shared.SingleResultImpl) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "find", 0, t.ServerAddr, t.ServerPort)...),
	)
	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	sr := t.Coll.FindOne(ctx, filter, opts...)
	deliverSpan.End()
	return sr, NewSingleResult(sr, span, ctx, t.Propagator, t.PropagationEnabled)
}

// UpdateOne updates one matching document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.PropagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(injectCtx, update, t.Propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.Coll.UpdateOne(injectCtx, filter, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// UpdateMany updates all matching documents; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateManyOptions]) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.PropagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(injectCtx, update, t.Propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.Coll.UpdateMany(injectCtx, filter, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ReplaceOne replaces one matching document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	replacementToUse := replacement
	if t.PropagationEnabled {
		replacementWithTrace, err := shared.InjectTraceIntoDocument(injectCtx, replacement, t.Propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		replacementToUse = replacementWithTrace
	}
	res, err := t.Coll.ReplaceOne(injectCtx, filter, replacementToUse, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// DeleteOne deletes one matching document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*mongo.DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "delete", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := t.Coll.DeleteOne(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// DeleteMany deletes all matching documents; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*mongo.DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "delete", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := t.Coll.DeleteMany(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// CountDocuments counts documents matching the filter; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	n, err := t.Coll.CountDocuments(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	return n, err
}

// Distinct returns distinct values for the field; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("distinct", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "distinct", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	result := t.Coll.Distinct(ctx, fieldName, filter, opts...)
	deliverSpan.End()
	return result
}

// Aggregate runs an aggregation pipeline and returns the cursor + impl; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*mongo.Cursor, shared.CursorImpl, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	_, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := t.Coll.Aggregate(ctx, pipeline, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, nil, err
	}
	return cursor, NewCursor(cursor, t.Tracer, t.Propagator, t.PropagationEnabled), nil
}

// UpdateByID updates one document by _id; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.PropagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(injectCtx, update, t.Propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.Coll.UpdateByID(injectCtx, id, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// BulkWrite runs multiple write operations; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*mongo.BulkWriteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("bulkWrite", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "bulkWrite", len(models), t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.StartDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	modelsToWrite := models
	if t.PropagationEnabled {
		injected, err := shared.BuildBulkWriteModelsWithTrace(injectCtx, models, t.Propagator)
		if err != nil {
			shared.RecordSpanError(span, err)
			return nil, err
		}
		modelsToWrite = injected
	}
	res, err := t.Coll.BulkWrite(injectCtx, modelsToWrite, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Watch starts a change stream on the collection; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*mongo.ChangeStream, shared.ChangeStreamImpl, error) {
	dbName, collName := t.dbAndColl()
	spanName := shared.DBSpanName("aggregate", collName)
	ctx, span := t.Tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.ServerAddr, t.ServerPort)...),
	)
	defer span.End()

	cs, err := t.Coll.Watch(ctx, pipeline, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, nil, err
	}
	baseSpanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.ServerAddr, t.ServerPort)...),
	}
	deliverAttrs := shared.DeliverAttributes(dbName, collName, t.ServerAddr, t.ServerPort)
	return cs, NewChangeStream(cs, ChangeStreamConfig{
		Tracer:             t.Tracer,
		Propagator:         t.Propagator,
		PropagationEnabled: t.PropagationEnabled,
		SpanName:           spanName,
		BaseSpanOpts:       baseSpanOpts,
		DeliverTracer:      t.DeliverTracer,
		DeliverSpanName:    dbName + "." + collName + " deliver",
		DeliverAttrs:       deliverAttrs,
	}), nil
}
