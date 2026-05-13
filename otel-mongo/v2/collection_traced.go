package otelmongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/shared"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// tracedCollection is the fully-instrumented collectionImpl.
type tracedCollection struct {
	coll               *mongo.Collection
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	deliverTracer      trace.Tracer
	serverAddr         string
	serverPort         int
}

func (t *tracedCollection) dbAndColl() (dbName, collName string) {
	collName = t.coll.Name()
	if t.coll.Database() != nil {
		dbName = t.coll.Database().Name()
	}
	return dbName, collName
}

// startDeliverSpan creates a synthetic CONSUMER span representing MongoDB broker delivery.
func (t *tracedCollection) startDeliverSpan(ctx context.Context, dbName, collName string) (context.Context, trace.Span) {
	if t.deliverTracer == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	deliverCtx, span := t.deliverTracer.Start(ctx,
		dbName+"."+collName+" deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(shared.DeliverAttributes(dbName, collName, t.serverAddr, t.serverPort)...),
	)
	return deliverCtx, span
}

func (t *tracedCollection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*InsertOneResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "insert", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docToInsert := document
	if t.propagationEnabled {
		docWithTrace, err := shared.InjectTraceIntoDocument(injectCtx, document, t.propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		docToInsert = docWithTrace
	}
	res, err := t.coll.InsertOne(injectCtx, docToInsert, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &InsertOneResult{res}, nil
}

func (t *tracedCollection) InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*InsertManyResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "insert", len(documents), t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docsToInsert := documents
	if t.propagationEnabled {
		docsWithTrace := make([]any, 0, len(documents))
		for _, doc := range documents {
			d, err := shared.InjectTraceIntoDocument(injectCtx, doc, t.propagator)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
			}
			docsWithTrace = append(docsWithTrace, d)
		}
		docsToInsert = docsWithTrace
	}
	res, err := t.coll.InsertMany(injectCtx, docsToInsert, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &InsertManyResult{res}, nil
}

func (t *tracedCollection) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*Cursor, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "find", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := t.coll.Find(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &Cursor{
		Cursor: cursor,
		impl:   traced.NewCursor(cursor, t.tracer, t.propagator, t.propagationEnabled),
	}, nil
}

func (t *tracedCollection) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) *SingleResult {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "find", 0, t.serverAddr, t.serverPort)...),
	)
	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	sr := t.coll.FindOne(ctx, filter, opts...)
	deliverSpan.End()
	return &SingleResult{
		SingleResult: sr,
		impl:         traced.NewSingleResult(sr, span, ctx, t.propagator, t.propagationEnabled),
	}
}

func (t *tracedCollection) UpdateOne(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.propagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(injectCtx, update, t.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.coll.UpdateOne(injectCtx, filter, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) UpdateMany(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateManyOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.propagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(injectCtx, update, t.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.coll.UpdateMany(injectCtx, filter, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	replacementToUse := replacement
	if t.propagationEnabled {
		replacementWithTrace, err := shared.InjectTraceIntoDocument(injectCtx, replacement, t.propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		replacementToUse = replacementWithTrace
	}
	res, err := t.coll.ReplaceOne(injectCtx, filter, replacementToUse, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "delete", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := t.coll.DeleteOne(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

func (t *tracedCollection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "delete", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := t.coll.DeleteMany(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

func (t *tracedCollection) CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	n, err := t.coll.CountDocuments(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	return n, err
}

func (t *tracedCollection) Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("distinct", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "distinct", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	result := t.coll.Distinct(ctx, fieldName, filter, opts...)
	deliverSpan.End()
	return result
}

func (t *tracedCollection) Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := t.coll.Aggregate(ctx, pipeline, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &Cursor{
		Cursor: cursor,
		impl:   traced.NewCursor(cursor, t.tracer, t.propagator, t.propagationEnabled),
	}, nil
}

func (t *tracedCollection) UpdateByID(ctx context.Context, id any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.propagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(injectCtx, update, t.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.coll.UpdateByID(injectCtx, id, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*BulkWriteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, shared.DBSpanName("bulkWrite", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "bulkWrite", len(models), t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	modelsToWrite := models
	if t.propagationEnabled {
		injected, err := shared.BuildBulkWriteModelsWithTrace(injectCtx, models, t.propagator)
		if err != nil {
			shared.RecordSpanError(span, err)
			return nil, err
		}
		modelsToWrite = injected
	}
	res, err := t.coll.BulkWrite(injectCtx, modelsToWrite, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &BulkWriteResult{res}, nil
}

func (t *tracedCollection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	dbName, collName := t.dbAndColl()
	spanName := shared.DBSpanName("aggregate", collName)
	ctx, span := t.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	cs, err := t.coll.Watch(ctx, pipeline, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	baseSpanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	}
	deliverAttrs := shared.DeliverAttributes(dbName, collName, t.serverAddr, t.serverPort)
	return &ChangeStream{
		ChangeStream: cs,
		impl: traced.NewChangeStream(cs, traced.ChangeStreamConfig{
			Tracer:             t.tracer,
			Propagator:         t.propagator,
			PropagationEnabled: t.propagationEnabled,
			SpanName:           spanName,
			BaseSpanOpts:       baseSpanOpts,
			DeliverTracer:      t.deliverTracer,
			DeliverSpanName:    dbName + "." + collName + " deliver",
			DeliverAttrs:       deliverAttrs,
		}),
	}, nil
}
