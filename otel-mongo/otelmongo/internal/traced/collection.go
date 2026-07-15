package traced

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-mongo/otelmongo/internal/shared"
)

// Collection is the fully-instrumented collectionImpl: wraps every CRUD method
// with a CLIENT span and (when PropagationEnabled) _oteltrace document
// injection. Constructed by the facade NewCollection / newCollectionForDatabase
// only when the tracing gate is on.
//
// Fields are exported so tests in the facade package (and elsewhere within
// this module) can build Collection literals for unit testing without going
// through a constructor.
type Collection struct {
	Coll               *mongo.Collection
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
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

// setCapturedServerAttrs overwrites the span's server.address/server.port with the
// per-command captured value, falling back to the static t.ServerAddr/ServerPort
// when nothing was captured for this call. See internal/shared/monitor.go.
//
// CRUD methods register this as `defer t.setCapturedServerAttrs(span, capture)`
// immediately after WithAddrCapture — i.e. after the `defer span.End()` above it,
// so LIFO ordering runs it before the span ends. Deferring (rather than an explicit
// post-call statement) guarantees the fallback is emitted on *every* return path,
// including early returns when _oteltrace injection / BSON encoding fails before the
// driver call — otherwise those failure spans would omit server.* entirely, violating
// the "Fallback to static URI-derived address" spec. FindOne is the lone exception:
// it hands the still-open span to its SingleResult and calls this explicitly instead.
func (t *Collection) setCapturedServerAttrs(span trace.Span, capture *shared.AddrCapture) {
	if addr, port := capture.Resolve(t.ServerAddr, t.ServerPort); addr != "" {
		span.SetAttributes(shared.ServerAttributes(addr, port)...)
	}
}

// changeStreamReaderAttrs builds the attribute set for the ChangeStream reader's
// getMore spans: db.* plus the static server.* snapshot. Those spans are
// out of scope for per-command capture (design non-goal), so — unlike CRUD sites,
// which emit server.* once post-call from the captured value — they keep the
// Connect-time static t.ServerAddr/ServerPort. Since DBAttributes no longer emits
// server.*, ServerAttributes must be appended here or the reader spans would carry
// no server.address at all.
func (t *Collection) changeStreamReaderAttrs(dbName, collName string) []attribute.KeyValue {
	attrs := shared.DBAttributes(dbName, collName, "aggregate", 0)
	return append(attrs, shared.ServerAttributes(t.ServerAddr, t.ServerPort)...)
}

// InsertOne inserts a single document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) InsertOne(ctx context.Context, document any, opts ...*options.InsertOneOptions) (*mongo.InsertOneResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "insert", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	docToInsert := document
	if t.PropagationEnabled {
		docWithTrace, err := shared.InjectTraceIntoDocument(ctx, document, t.Propagator)
		if err != nil {
			err = fmt.Errorf("otelmongo: inject trace: %w", err)
			shared.RecordSpanError(span, err)
			return nil, err
		}
		docToInsert = docWithTrace
	}
	res, err := t.Coll.InsertOne(ctx, docToInsert, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// InsertMany inserts multiple documents; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("insert", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "insert", len(documents))...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	docsToInsert := documents
	if t.PropagationEnabled {
		docsWithTrace := make([]any, 0, len(documents))
		for _, doc := range documents {
			d, err := shared.InjectTraceIntoDocument(ctx, doc, t.Propagator)
			if err != nil {
				err = fmt.Errorf("otelmongo: inject trace: %w", err)
				shared.RecordSpanError(span, err)
				return nil, err
			}
			docsWithTrace = append(docsWithTrace, d)
		}
		docsToInsert = docsWithTrace
	}
	res, err := t.Coll.InsertMany(ctx, docsToInsert, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Find executes a find command and returns the cursor + impl; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, shared.CursorImpl, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "find", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	cursor, err := t.Coll.Find(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, nil, err
	}
	return cursor, NewCursor(cursor, t.Tracer, t.Propagator, t.PropagationEnabled), nil
}

// FindOne executes a find command returning at most one document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) (*mongo.SingleResult, shared.SingleResultImpl) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("find", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "find", 0)...),
	)
	ctx, capture := shared.WithAddrCapture(ctx)
	sr := t.Coll.FindOne(ctx, filter, opts...)
	t.setCapturedServerAttrs(span, capture)
	return sr, NewSingleResult(sr, span, ctx, t.Propagator, t.PropagationEnabled)
}

// UpdateOne updates one matching document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) UpdateOne(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	return t.runUpdate(ctx, "UpdateOne", filter, update, opts)
}

// UpdateMany updates all matching documents; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) UpdateMany(ctx context.Context, filter any, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	return t.runUpdate(ctx, "UpdateMany", filter, update, opts)
}

func (t *Collection) runUpdate(ctx context.Context, op string, filter, update any, opts []*options.UpdateOptions) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	updateWithTrace := update
	if t.PropagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(ctx, update, t.Propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	var (
		res *mongo.UpdateResult
		err error
	)
	switch op {
	case "UpdateOne":
		res, err = t.Coll.UpdateOne(ctx, filter, updateWithTrace, opts...)
	case "UpdateMany":
		res, err = t.Coll.UpdateMany(ctx, filter, updateWithTrace, opts...)
	}
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ReplaceOne replaces one matching document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) ReplaceOne(ctx context.Context, filter any, replacement any, opts ...*options.ReplaceOptions) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	replacementToUse := replacement
	if t.PropagationEnabled {
		replacementWithTrace, err := shared.InjectTraceIntoDocument(ctx, replacement, t.Propagator)
		if err != nil {
			err = fmt.Errorf("otelmongo: inject trace: %w", err)
			shared.RecordSpanError(span, err)
			return nil, err
		}
		replacementToUse = replacementWithTrace
	}
	res, err := t.Coll.ReplaceOne(ctx, filter, replacementToUse, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// DeleteOne deletes one matching document; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	return t.runDelete(ctx, "DeleteOne", filter, opts)
}

// DeleteMany deletes all matching documents; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) DeleteMany(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	return t.runDelete(ctx, "DeleteMany", filter, opts)
}

func (t *Collection) runDelete(ctx context.Context, op string, filter any, opts []*options.DeleteOptions) (*mongo.DeleteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("delete", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "delete", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	var (
		res *mongo.DeleteResult
		err error
	)
	switch op {
	case "DeleteOne":
		res, err = t.Coll.DeleteOne(ctx, filter, opts...)
	case "DeleteMany":
		res, err = t.Coll.DeleteMany(ctx, filter, opts...)
	}
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// CountDocuments counts documents matching the filter; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) CountDocuments(ctx context.Context, filter any, opts ...*options.CountOptions) (int64, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	n, err := t.Coll.CountDocuments(ctx, filter, opts...)
	shared.RecordSpanError(span, err)
	return n, err
}

// Distinct returns distinct values for the field; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Distinct(ctx context.Context, fieldName string, filter any, opts ...*options.DistinctOptions) ([]interface{}, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("distinct", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "distinct", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	vals, err := t.Coll.Distinct(ctx, fieldName, filter, opts...)
	shared.RecordSpanError(span, err)
	return vals, err
}

// Aggregate runs an aggregation pipeline and returns the cursor + impl; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Aggregate(ctx context.Context, pipeline any, opts ...*options.AggregateOptions) (*mongo.Cursor, shared.CursorImpl, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("aggregate", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	cursor, err := t.Coll.Aggregate(ctx, pipeline, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, nil, err
	}
	return cursor, NewCursor(cursor, t.Tracer, t.Propagator, t.PropagationEnabled), nil
}

// UpdateByID updates one document by _id; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) UpdateByID(ctx context.Context, id any, update any, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("update", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "update", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	updateWithTrace := update
	if t.PropagationEnabled {
		var err error
		updateWithTrace, err = shared.InjectTraceIntoUpdate(ctx, update, t.Propagator)
		if err != nil {
			span.AddEvent("otelmongo.trace_inject_failed",
				trace.WithAttributes(attribute.String("error.message", err.Error())))
			updateWithTrace = update
		}
	}
	res, err := t.Coll.UpdateByID(ctx, id, updateWithTrace, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// BulkWrite runs multiple write operations; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...*options.BulkWriteOptions) (*mongo.BulkWriteResult, error) {
	dbName, collName := t.dbAndColl()
	ctx, span := t.Tracer.Start(ctx, shared.DBSpanName("bulkWrite", collName),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "bulkWrite", len(models))...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	modelsToWrite := models
	if t.PropagationEnabled {
		injected, err := shared.BuildBulkWriteModelsWithTrace(ctx, models, t.Propagator)
		if err != nil {
			shared.RecordSpanError(span, err)
			return nil, err
		}
		modelsToWrite = injected
	}
	res, err := t.Coll.BulkWrite(ctx, modelsToWrite, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Watch starts a change stream on the collection; wraps *mongo.Collection with a CLIENT span (and propagation when enabled).
func (t *Collection) Watch(ctx context.Context, pipeline interface{}, opts ...*options.ChangeStreamOptions) (*mongo.ChangeStream, shared.ChangeStreamImpl, error) {
	dbName, collName := t.dbAndColl()
	spanName := shared.DBSpanName("aggregate", collName)
	ctx, span := t.Tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shared.DBAttributes(dbName, collName, "aggregate", 0)...),
	)
	defer span.End()
	ctx, capture := shared.WithAddrCapture(ctx)
	defer t.setCapturedServerAttrs(span, capture)

	cs, err := t.Coll.Watch(ctx, pipeline, opts...)
	shared.RecordSpanError(span, err)
	if err != nil {
		return nil, nil, err
	}
	// baseSpanOpts seeds the ChangeStream reader's later getMore spans, which are
	// out of scope for per-command address capture (design non-goal) — they keep
	// the static t.ServerAddr/ServerPort snapshot, not this Watch call's captured value.
	baseSpanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(t.changeStreamReaderAttrs(dbName, collName)...),
	}
	return cs, NewChangeStream(cs, ChangeStreamConfig{
		Tracer:             t.Tracer,
		Propagator:         t.Propagator,
		PropagationEnabled: t.PropagationEnabled,
		SpanName:           spanName,
		BaseSpanOpts:       baseSpanOpts,
	}), nil
}
