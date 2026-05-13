package otelmongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracedCollection is the fully-instrumented collectionImpl: wraps every
// CRUD method with a CLIENT span, a deliver CONSUMER span, and (when
// propagationEnabled) _oteltrace document injection.
type tracedCollection struct {
	coll               *mongo.Collection
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	deliverTracer      trace.Tracer
	serverAddr         string
	serverPort         int
}

func (t *tracedCollection) tracingOn() bool           { return true }
func (t *tracedCollection) propagationOn() bool       { return t.propagationEnabled }
func (t *tracedCollection) tracerProbe() trace.Tracer { return t.tracer }

func (t *tracedCollection) dbAndColl() (dbName, collName string) {
	collName = t.coll.Name()
	if t.coll.Database() != nil {
		dbName = t.coll.Database().Name()
	}
	return dbName, collName
}

// startDeliverSpan creates a synthetic CONSUMER span representing MongoDB broker delivery.
// The returned context carries the deliver span, suitable for injecting into documents so
// change stream consumers link to it. The caller must End the returned span after the
// MongoDB operation completes. When deliverTracer is nil, returns a no-op span safe to End.
func (t *tracedCollection) startDeliverSpan(ctx context.Context, dbName, collName string) (context.Context, trace.Span) {
	if t.deliverTracer == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	attrs := []attribute.KeyValue{
		attribute.String(keyDBSystemName, dbSystemMongoDB),
		attribute.String(keyDBCollection, collName),
	}
	if dbName != "" {
		attrs = append(attrs, attribute.String(keyDBNamespace, dbName))
	}
	if t.serverAddr != "" {
		attrs = append(attrs, attribute.String(keyServerAddress, t.serverAddr))
		if t.serverPort > 0 && t.serverPort != 27017 {
			attrs = append(attrs, attribute.Int(keyServerPort, t.serverPort))
		}
	}
	deliverCtx, span := t.deliverTracer.Start(ctx,
		dbName+"."+collName+" deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	)
	return deliverCtx, span
}

func (t *tracedCollection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (*InsertOneResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "insert", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docToInsert := document
	if t.propagationEnabled {
		docWithTrace, err := injectTraceIntoDocument(injectCtx, document, t.propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		docToInsert = docWithTrace
	}
	res, err := t.coll.InsertOne(injectCtx, docToInsert, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &InsertOneResult{res}, nil
}

func (t *tracedCollection) InsertMany(ctx context.Context, documents []any, opts ...options.Lister[options.InsertManyOptions]) (*InsertManyResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "insert", len(documents), t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docsToInsert := documents
	if t.propagationEnabled {
		docsWithTrace := make([]any, 0, len(documents))
		for _, doc := range documents {
			d, err := injectTraceIntoDocument(injectCtx, doc, t.propagator)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
			}
			docsWithTrace = append(docsWithTrace, d)
		}
		docsToInsert = docsWithTrace
	}
	res, err := t.coll.InsertMany(injectCtx, docsToInsert, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &InsertManyResult{res}, nil
}

func (t *tracedCollection) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*Cursor, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "find", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := t.coll.Find(ctx, filter, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &Cursor{
		Cursor:             cursor,
		parentCtx:          ctx,
		tracer:             t.tracer,
		propagator:         t.propagator,
		tracingEnabled:     true,
		propagationEnabled: t.propagationEnabled,
	}, nil
}

func (t *tracedCollection) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) *SingleResult {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "find", 0, t.serverAddr, t.serverPort)...),
	)
	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	sr := t.coll.FindOne(ctx, filter, opts...)
	deliverSpan.End()
	return &SingleResult{
		SingleResult:       sr,
		span:               span,
		ctx:                ctx,
		propagator:         t.propagator,
		tracingEnabled:     true,
		propagationEnabled: t.propagationEnabled,
	}
}

func (t *tracedCollection) UpdateOne(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.propagationEnabled {
		var err error
		updateWithTrace, err = injectTraceIntoUpdate(injectCtx, update, t.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.coll.UpdateOne(injectCtx, filter, updateWithTrace, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) UpdateMany(ctx context.Context, filter any, update any, opts ...options.Lister[options.UpdateManyOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.propagationEnabled {
		var err error
		updateWithTrace, err = injectTraceIntoUpdate(injectCtx, update, t.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.coll.UpdateMany(injectCtx, filter, updateWithTrace, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	replacementToUse := replacement
	if t.propagationEnabled {
		replacementWithTrace, err := injectTraceIntoDocument(injectCtx, replacement, t.propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		replacementToUse = replacementWithTrace
	}
	res, err := t.coll.ReplaceOne(injectCtx, filter, replacementToUse, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "delete", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := t.coll.DeleteOne(ctx, filter, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

func (t *tracedCollection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (*DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "delete", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := t.coll.DeleteMany(ctx, filter, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

func (t *tracedCollection) CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	n, err := t.coll.CountDocuments(ctx, filter, opts...)
	recordSpanError(span, err)
	return n, err
}

func (t *tracedCollection) Distinct(ctx context.Context, fieldName string, filter any, opts ...options.Lister[options.DistinctOptions]) *mongo.DistinctResult {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("distinct", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "distinct", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	result := t.coll.Distinct(ctx, fieldName, filter, opts...)
	deliverSpan.End()
	return result
}

func (t *tracedCollection) Aggregate(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := t.coll.Aggregate(ctx, pipeline, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &Cursor{
		Cursor:             cursor,
		parentCtx:          ctx,
		tracer:             t.tracer,
		propagator:         t.propagator,
		tracingEnabled:     true,
		propagationEnabled: t.propagationEnabled,
	}, nil
}

func (t *tracedCollection) UpdateByID(ctx context.Context, id any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if t.propagationEnabled {
		var err error
		updateWithTrace, err = injectTraceIntoUpdate(injectCtx, update, t.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.coll.UpdateByID(injectCtx, id, updateWithTrace, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

func (t *tracedCollection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*BulkWriteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.tracer.Start(ctx, dbSpanName("bulkWrite", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "bulkWrite", len(models), t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := t.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	modelsToWrite := models
	if t.propagationEnabled {
		injected, err := buildBulkWriteModelsWithTrace(injectCtx, models, t.propagator)
		if err != nil {
			recordSpanError(span, err)
			return nil, err
		}
		modelsToWrite = injected
	}
	res, err := t.coll.BulkWrite(injectCtx, modelsToWrite, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &BulkWriteResult{res}, nil
}

func (t *tracedCollection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	dbName, collName := t.dbAndColl()
	spanName := dbSpanName("aggregate", collName)
	ctx, span := t.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	)
	defer span.End()

	cs, err := t.coll.Watch(ctx, pipeline, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	baseSpanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, t.serverAddr, t.serverPort)...),
	}
	deliverAttrs := []attribute.KeyValue{
		attribute.String(keyDBSystemName, dbSystemMongoDB),
		attribute.String(keyDBCollection, collName),
	}
	if dbName != "" {
		deliverAttrs = append(deliverAttrs, attribute.String(keyDBNamespace, dbName))
	}
	if t.serverAddr != "" {
		deliverAttrs = append(deliverAttrs, attribute.String(keyServerAddress, t.serverAddr))
		if t.serverPort > 0 && t.serverPort != 27017 {
			deliverAttrs = append(deliverAttrs, attribute.Int(keyServerPort, t.serverPort))
		}
	}
	return &ChangeStream{
		ChangeStream:       cs,
		tracer:             t.tracer,
		propagator:         t.propagator,
		tracingEnabled:     true,
		propagationEnabled: t.propagationEnabled,
		spanName:           spanName,
		baseSpanOpts:       baseSpanOpts,
		deliverTracer:      t.deliverTracer,
		deliverSpanName:    dbName + "." + collName + " deliver",
		deliverAttrs:       deliverAttrs,
	}, nil
}
