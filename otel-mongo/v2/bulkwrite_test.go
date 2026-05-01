package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestBuildBulkWriteModelsWithTrace_InsertOneModel(t *testing.T) {
	enableTracing(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	models := []mongo.WriteModel{
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "a", Value: 1}}),
	}
	out, err := buildBulkWriteModelsWithTrace(ctx, models, otel.GetTextMapPropagator())
	require.NoError(t, err)
	require.Len(t, out, 1)
	ins, ok := out[0].(*mongo.InsertOneModel)
	require.True(t, ok)
	doc, ok := getInsertOneModelDocument(ins)
	require.True(t, ok)
	docD, ok := doc.(bson.D)
	require.True(t, ok)
	hasTrace := false
	for _, e := range docD {
		if e.Key == TraceMetadataKey {
			hasTrace = true
			break
		}
	}
	assert.True(t, hasTrace, "InsertOneModel document should contain _oteltrace")
}

func TestBuildBulkWriteModelsWithTrace_UpdateOneModel(t *testing.T) {
	enableTracing(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	models := []mongo.WriteModel{
		mongo.NewUpdateOneModel().SetFilter(bson.D{{Key: "x", Value: 1}}).SetUpdate(bson.D{{Key: "$set", Value: bson.D{{Key: "y", Value: 2}}}}),
	}
	out, err := buildBulkWriteModelsWithTrace(ctx, models, otel.GetTextMapPropagator())
	require.NoError(t, err)
	require.Len(t, out, 1)
	upd, ok := out[0].(*mongo.UpdateOneModel)
	require.True(t, ok)
	update, ok := getUpdateModelFilterUpdate(upd)
	require.True(t, ok)
	updateD, ok := update.(bson.D)
	require.True(t, ok)
	hasSetTrace := false
	for _, e := range updateD {
		if e.Key == "$set" {
			setDoc, _ := e.Value.(bson.D)
			for _, s := range setDoc {
				if s.Key == TraceMetadataKey {
					hasSetTrace = true
					break
				}
			}
			break
		}
	}
	assert.True(t, hasSetTrace, "UpdateOneModel update $set should contain _oteltrace")
}

func TestBuildBulkWriteModelsWithTrace_UpdateOneModel_PreservesOptions(t *testing.T) {
	enableTracing(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	upsertTrue := true
	hint := bson.D{{Key: "x", Value: 1}}
	arrayFilters := []any{bson.D{{Key: "elem.x", Value: bson.D{{Key: "$gt", Value: 0}}}}}
	orig := mongo.NewUpdateOneModel().
		SetFilter(bson.D{{Key: "x", Value: 1}}).
		SetUpdate(bson.D{{Key: "$set", Value: bson.D{{Key: "y", Value: 2}}}}).
		SetUpsert(upsertTrue).
		SetHint(hint).
		SetArrayFilters(arrayFilters)

	out, err := buildBulkWriteModelsWithTrace(ctx, []mongo.WriteModel{orig}, otel.GetTextMapPropagator())
	require.NoError(t, err)
	require.Len(t, out, 1)

	got, ok := out[0].(*mongo.UpdateOneModel)
	require.True(t, ok)
	require.NotNil(t, got.Upsert, "Upsert must be preserved")
	assert.True(t, *got.Upsert, "Upsert value must be true")
	assert.Equal(t, orig.Hint, got.Hint, "Hint must be preserved")
	assert.Equal(t, orig.ArrayFilters, got.ArrayFilters, "ArrayFilters must be preserved")
	assert.Equal(t, orig.Filter, got.Filter, "Filter must be preserved")
}

func TestBuildBulkWriteModelsWithTrace_UpdateOneModel_SetOnInsert(t *testing.T) {
	enableTracing(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	upsertTrue := true
	orig := mongo.NewUpdateOneModel().
		SetFilter(bson.D{{Key: "u._id", Value: "123"}, {Key: "p._id", Value: "444"}}).
		SetUpdate(bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "u._id", Value: "123"}, {Key: "p._id", Value: "444"}}}}).
		SetUpsert(upsertTrue)

	out, err := buildBulkWriteModelsWithTrace(ctx, []mongo.WriteModel{orig}, otel.GetTextMapPropagator())
	require.NoError(t, err)
	require.Len(t, out, 1)

	got, ok := out[0].(*mongo.UpdateOneModel)
	require.True(t, ok)

	// Upsert must be preserved.
	require.NotNil(t, got.Upsert, "Upsert must be preserved")
	assert.True(t, *got.Upsert)

	// Update must contain _oteltrace in both $setOnInsert and $set.
	updateD, ok := got.Update.(bson.D)
	require.True(t, ok)

	hasTraceInSetOnInsert := false
	hasSet := false
	uDotIDPreserved := false
	pDotIDPreserved := false
	for _, e := range updateD {
		switch e.Key {
		case "$setOnInsert":
			subDoc, _ := e.Value.(bson.D)
			for _, s := range subDoc {
				switch s.Key {
				case TraceMetadataKey:
					hasTraceInSetOnInsert = true
				case "u._id":
					// Dot-notation field name must survive the marshal/unmarshal round-trip
					// as a literal string — bson.D does not expand dots into nested documents.
					uDotIDPreserved = true
				case "p._id":
					pDotIDPreserved = true
				}
			}
		case "$set":
			hasSet = true
		}
	}
	assert.True(t, hasTraceInSetOnInsert, "$setOnInsert must contain _oteltrace so upserted documents carry trace context")
	assert.True(t, hasSet, "$set must be present so existing documents are also annotated")
	assert.True(t, uDotIDPreserved, "dot-notation field name 'u._id' must be preserved verbatim in $setOnInsert after marshal/unmarshal round-trip")
	assert.True(t, pDotIDPreserved, "dot-notation field name 'p._id' must be preserved verbatim in $setOnInsert after marshal/unmarshal round-trip")
}

func TestBuildBulkWriteModelsWithTrace_OtherModelsUnchanged(t *testing.T) {
	ctx := context.Background()
	del := mongo.NewDeleteOneModel().SetFilter(bson.D{{Key: "_id", Value: 1}})
	models := []mongo.WriteModel{del}
	out, err := buildBulkWriteModelsWithTrace(ctx, models, otel.GetTextMapPropagator())
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Same(t, del, out[0])
}
