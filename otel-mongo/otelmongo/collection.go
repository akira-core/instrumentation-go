package otelmongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Collection wraps *mongo.Collection and overrides CRUD methods to inject and
// extract OpenTelemetry trace contexts via the "_oteltrace" document field.
type Collection struct {
	*mongo.Collection
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	tracingEnabled     bool // when false, wrapper CLIENT spans are skipped entirely
	propagationEnabled bool
	serverAddr         string
	serverPort         int
	deliverTracer      trace.Tracer // nil when disabled
}

// NewCollection wraps an existing *mongo.Collection with trace propagation.
// Tracer and propagator are required; use WithTracerProvider/WithPropagators via Connect
// for the standard init path. Document _oteltrace injection follows the same env gates as
// Connect (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED and OTEL_MONGO_PROPAGATION_ENABLED); there is
// no per-wrapper option—use ConnectWithOptions(..., WithTracePropagationEnabled(...)) for that.
// When the global+module tracing gate is off, the supplied tracer is replaced with a noop
// tracer so wrapper CLIENT spans are suppressed — symmetric with Connect.
func NewCollection(coll *mongo.Collection, tracer trace.Tracer, propagator propagation.TextMapPropagator) *Collection {
	tracingOn := mongoTracingEnabled()
	if !tracingOn {
		tracer = noop.NewTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
	}
	return &Collection{
		Collection:         coll,
		tracer:             tracer,
		propagator:         propagator,
		tracingEnabled:     tracingOn,
		propagationEnabled: mongoPropagationEnabled(),
	}
}

func (c *Collection) dbAndColl() (dbName, collName string) {
	collName = c.Name()
	if c.Database() != nil {
		dbName = c.Database().Name()
	}
	return dbName, collName
}

// startDeliverSpan creates a synthetic CONSUMER span representing MongoDB broker delivery.
// The returned context carries the deliver span, suitable for injecting into documents so
// change stream consumers link to it. The caller must End the returned span after the
// MongoDB operation completes. When deliverTracer is nil, returns a no-op span safe to End.
func (c *Collection) startDeliverSpan(ctx context.Context, dbName, collName string) (context.Context, trace.Span) {
	if c.deliverTracer == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	attrs := []attribute.KeyValue{
		attribute.String(keyDBSystemName, dbSystemMongoDB),
		attribute.String(keyDBCollection, collName),
	}
	if dbName != "" {
		attrs = append(attrs, attribute.String(keyDBNamespace, dbName))
	}
	if c.serverAddr != "" {
		attrs = append(attrs, attribute.String(keyServerAddress, c.serverAddr))
		if c.serverPort > 0 && c.serverPort != 27017 {
			attrs = append(attrs, attribute.Int(keyServerPort, c.serverPort))
		}
	}
	deliverCtx, span := c.deliverTracer.Start(ctx,
		dbName+"."+collName+" deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	)
	return deliverCtx, span
}

// InsertOne inserts a document, injecting the deliver span traceparent into "_oteltrace".
func (c *Collection) InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*InsertOneResult, error) {
	if !c.tracingEnabled {
		docToInsert := document
		if c.propagationEnabled {
			d, err := injectTraceIntoDocument(ctx, document, c.propagator)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
			}
			docToInsert = d
		}
		res, err := c.Collection.InsertOne(ctx, docToInsert, opts...)
		if err != nil {
			return nil, err
		}
		return &InsertOneResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "insert", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docToInsert := document
	if c.propagationEnabled {
		docWithTrace, err := injectTraceIntoDocument(injectCtx, document, c.propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		docToInsert = docWithTrace
	}
	res, err := c.Collection.InsertOne(injectCtx, docToInsert, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &InsertOneResult{res}, nil
}

// InsertMany inserts documents, injecting the deliver span traceparent into each "_oteltrace".
func (c *Collection) InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*InsertManyResult, error) {
	if !c.tracingEnabled {
		docsToInsert := documents
		if c.propagationEnabled {
			docsWithTrace := make([]any, 0, len(documents))
			for _, doc := range documents {
				d, err := injectTraceIntoDocument(ctx, doc, c.propagator)
				if err != nil {
					return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
				}
				docsWithTrace = append(docsWithTrace, d)
			}
			docsToInsert = docsWithTrace
		}
		res, err := c.Collection.InsertMany(ctx, docsToInsert, opts...)
		if err != nil {
			return nil, err
		}
		return &InsertManyResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "insert", len(documents), c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	docsToInsert := documents
	if c.propagationEnabled {
		docsWithTrace := make([]any, 0, len(documents))
		for _, doc := range documents {
			d, err := injectTraceIntoDocument(injectCtx, doc, c.propagator)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
			}
			docsWithTrace = append(docsWithTrace, d)
		}
		docsToInsert = docsWithTrace
	}
	res, err := c.Collection.InsertMany(injectCtx, docsToInsert, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &InsertManyResult{res}, nil
}

// Find executes a find command and returns a Cursor.
func (c *Collection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*Cursor, error) {
	if !c.tracingEnabled {
		cursor, err := c.Collection.Find(ctx, filter, opts...)
		if err != nil {
			return nil, err
		}
		return &Cursor{Cursor: cursor, parentCtx: ctx, tracer: c.tracer, propagator: c.propagator, tracingEnabled: false, propagationEnabled: c.propagationEnabled}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "find", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := c.Collection.Find(ctx, filter, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &Cursor{Cursor: cursor, parentCtx: ctx, tracer: c.tracer, propagator: c.propagator, propagationEnabled: c.propagationEnabled}, nil
}

// FindOne executes a find command returning at most one document.
// The span is held in the returned *SingleResult and ended when Decode is called.
func (c *Collection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *SingleResult {
	if !c.tracingEnabled {
		sr := c.Collection.FindOne(ctx, filter, opts...)
		return &SingleResult{SingleResult: sr, ctx: ctx, propagator: c.propagator, tracingEnabled: false, propagationEnabled: c.propagationEnabled}
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "find", 0, c.serverAddr, c.serverPort)...),
	)
	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	sr := c.Collection.FindOne(ctx, filter, opts...)
	deliverSpan.End()
	return &SingleResult{SingleResult: sr, span: span, ctx: ctx, propagator: c.propagator, tracingEnabled: true, propagationEnabled: c.propagationEnabled}
}

// UpdateOne injects the current trace context into the update and replaces the document's _oteltrace.
func (c *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	if !c.tracingEnabled {
		updateWithTrace := update
		if c.propagationEnabled {
			if u, err := injectTraceIntoUpdate(ctx, update, c.propagator); err == nil {
				updateWithTrace = u
			}
		}
		res, err := c.Collection.UpdateOne(ctx, filter, updateWithTrace, opts...)
		if err != nil {
			return nil, err
		}
		return &UpdateResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if c.propagationEnabled {
		var err error
		updateWithTrace, err = injectTraceIntoUpdate(injectCtx, update, c.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := c.Collection.UpdateOne(injectCtx, filter, updateWithTrace, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// UpdateMany injects the current trace context into the update for all matched documents.
func (c *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	if !c.tracingEnabled {
		updateWithTrace := update
		if c.propagationEnabled {
			if u, err := injectTraceIntoUpdate(ctx, update, c.propagator); err == nil {
				updateWithTrace = u
			}
		}
		res, err := c.Collection.UpdateMany(ctx, filter, updateWithTrace, opts...)
		if err != nil {
			return nil, err
		}
		return &UpdateResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if c.propagationEnabled {
		var err error
		updateWithTrace, err = injectTraceIntoUpdate(injectCtx, update, c.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := c.Collection.UpdateMany(injectCtx, filter, updateWithTrace, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// ReplaceOne injects the current trace context into the replacement document.
func (c *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...*options.ReplaceOptions) (*UpdateResult, error) {
	if !c.tracingEnabled {
		replacementToUse := replacement
		if c.propagationEnabled {
			r, err := injectTraceIntoDocument(ctx, replacement, c.propagator)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
			}
			replacementToUse = r
		}
		res, err := c.Collection.ReplaceOne(ctx, filter, replacementToUse, opts...)
		if err != nil {
			return nil, err
		}
		return &UpdateResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	replacementToUse := replacement
	if c.propagationEnabled {
		replacementWithTrace, err := injectTraceIntoDocument(injectCtx, replacement, c.propagator)
		if err != nil {
			return nil, fmt.Errorf("otelmongo: inject trace: %w", err)
		}
		replacementToUse = replacementWithTrace
	}
	res, err := c.Collection.ReplaceOne(injectCtx, filter, replacementToUse, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &UpdateResult{res}, nil
}

// DeleteOne deletes one matching document.
func (c *Collection) DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	if !c.tracingEnabled {
		res, err := c.Collection.DeleteOne(ctx, filter, opts...)
		if err != nil {
			return nil, err
		}
		return &DeleteResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "delete", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := c.Collection.DeleteOne(ctx, filter, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

// DeleteMany deletes all documents matching filter.
func (c *Collection) DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*DeleteResult, error) {
	if !c.tracingEnabled {
		res, err := c.Collection.DeleteMany(ctx, filter, opts...)
		if err != nil {
			return nil, err
		}
		return &DeleteResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "delete", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	res, err := c.Collection.DeleteMany(ctx, filter, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{res}, nil
}

// CountDocuments counts documents matching filter.
func (c *Collection) CountDocuments(ctx context.Context, filter any, opts ...*options.CountOptions) (int64, error) {
	if !c.tracingEnabled {
		return c.Collection.CountDocuments(ctx, filter, opts...)
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	n, err := c.Collection.CountDocuments(ctx, filter, opts...)
	recordSpanError(span, err)
	return n, err
}

// Distinct returns distinct values for fieldName.
func (c *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...*options.DistinctOptions) ([]interface{}, error) {
	if !c.tracingEnabled {
		return c.Collection.Distinct(ctx, fieldName, filter, opts...)
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("distinct", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "distinct", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	vals, err := c.Collection.Distinct(ctx, fieldName, filter, opts...)
	recordSpanError(span, err)
	return vals, err
}

// Aggregate runs an aggregation pipeline and returns a Cursor.
func (c *Collection) Aggregate(ctx context.Context, pipeline any, opts ...*options.AggregateOptions) (*Cursor, error) {
	if !c.tracingEnabled {
		cursor, err := c.Collection.Aggregate(ctx, pipeline, opts...)
		if err != nil {
			return nil, err
		}
		return &Cursor{Cursor: cursor, parentCtx: ctx, tracer: c.tracer, propagator: c.propagator, tracingEnabled: false, propagationEnabled: c.propagationEnabled}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	_, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()

	cursor, err := c.Collection.Aggregate(ctx, pipeline, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &Cursor{Cursor: cursor, parentCtx: ctx, tracer: c.tracer, propagator: c.propagator, propagationEnabled: c.propagationEnabled}, nil
}

// UpdateByID updates one document by _id, injecting the current trace into the update.
func (c *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...*options.UpdateOptions) (*UpdateResult, error) {
	if !c.tracingEnabled {
		updateWithTrace := update
		if c.propagationEnabled {
			if u, err := injectTraceIntoUpdate(ctx, update, c.propagator); err == nil {
				updateWithTrace = u
			}
		}
		res, err := c.Collection.UpdateByID(ctx, id, updateWithTrace, opts...)
		if err != nil {
			return nil, err
		}
		return &UpdateResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "update", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	updateWithTrace := update
	if c.propagationEnabled {
		var err error
		updateWithTrace, err = injectTraceIntoUpdate(injectCtx, update, c.propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := c.Collection.UpdateByID(injectCtx, id, updateWithTrace, opts...)
	recordSpanError(span, err)
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
	if !c.tracingEnabled {
		modelsToWrite := models
		if c.propagationEnabled {
			injected, err := buildBulkWriteModelsWithTrace(ctx, models, c.propagator)
			if err != nil {
				return nil, err
			}
			modelsToWrite = injected
		}
		res, err := c.Collection.BulkWrite(ctx, modelsToWrite, opts...)
		if err != nil {
			return nil, err
		}
		return &BulkWriteResult{res}, nil
	}
	dbName, collName := c.dbAndColl()
	ctx, span := c.tracer.Start(ctx, dbSpanName("bulkWrite", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "bulkWrite", len(models), c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	injectCtx, deliverSpan := c.startDeliverSpan(ctx, dbName, collName)
	defer deliverSpan.End()
	modelsToWrite := models
	if c.propagationEnabled {
		injected, err := buildBulkWriteModelsWithTrace(injectCtx, models, c.propagator)
		if err != nil {
			recordSpanError(span, err)
			return nil, err
		}
		modelsToWrite = injected
	}
	res, err := c.Collection.BulkWrite(injectCtx, modelsToWrite, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return &BulkWriteResult{res}, nil
}

// Watch starts a change stream on the collection.
func (c *Collection) Watch(ctx context.Context, pipeline interface{}, opts ...*options.ChangeStreamOptions) (*ChangeStream, error) {
	if !c.tracingEnabled {
		cs, err := c.Collection.Watch(ctx, pipeline, opts...)
		if err != nil {
			return nil, err
		}
		return &ChangeStream{
			ChangeStream:       cs,
			tracer:             c.tracer,
			propagator:         c.propagator,
			tracingEnabled:     false,
			propagationEnabled: c.propagationEnabled,
		}, nil
	}
	dbName, collName := c.dbAndColl()
	spanName := dbSpanName("aggregate", collName)
	ctx, span := c.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, c.serverAddr, c.serverPort)...),
	)
	defer span.End()

	cs, err := c.Collection.Watch(ctx, pipeline, opts...)
	recordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	baseSpanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(dbAttributes(dbName, collName, "aggregate", 0, c.serverAddr, c.serverPort)...),
	}
	deliverAttrs := []attribute.KeyValue{
		attribute.String(keyDBSystemName, dbSystemMongoDB),
		attribute.String(keyDBCollection, collName),
	}
	if dbName != "" {
		deliverAttrs = append(deliverAttrs, attribute.String(keyDBNamespace, dbName))
	}
	if c.serverAddr != "" {
		deliverAttrs = append(deliverAttrs, attribute.String(keyServerAddress, c.serverAddr))
		if c.serverPort > 0 && c.serverPort != 27017 {
			deliverAttrs = append(deliverAttrs, attribute.Int(keyServerPort, c.serverPort))
		}
	}
	return &ChangeStream{
		ChangeStream:       cs,
		tracer:             c.tracer,
		propagator:         c.propagator,
		tracingEnabled:     true,
		propagationEnabled: c.propagationEnabled,
		spanName:           spanName,
		baseSpanOpts:       baseSpanOpts,
		deliverTracer:      c.deliverTracer,
		deliverSpanName:    dbName + "." + collName + " deliver",
		deliverAttrs:       deliverAttrs,
	}, nil
}
