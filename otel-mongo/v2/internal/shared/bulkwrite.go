package shared

import (
	"context"
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/propagation"
)

// BuildBulkWriteModelsWithTrace returns a new slice of WriteModels with _oteltrace injected.
func BuildBulkWriteModelsWithTrace(ctx context.Context, models []mongo.WriteModel, prop propagation.TextMapPropagator) ([]mongo.WriteModel, error) {
	out := make([]mongo.WriteModel, 0, len(models))
	for _, m := range models {
		traced, err := buildSingleModelWithTrace(ctx, m, prop)
		if err != nil {
			return nil, err
		}
		out = append(out, traced)
	}
	return out, nil
}

// buildSingleModelWithTrace dispatches one WriteModel to the matching
// per-kind helper. Non-traced kinds (DeleteOne, ReplaceOne, etc.) are
// passed through unchanged.
func buildSingleModelWithTrace(ctx context.Context, m mongo.WriteModel, prop propagation.TextMapPropagator) (mongo.WriteModel, error) {
	switch vm := m.(type) {
	case *mongo.InsertOneModel:
		return buildInsertOneModelWithTrace(ctx, vm, prop)
	case *mongo.UpdateOneModel:
		return buildUpdateOneModelWithTrace(ctx, vm, prop)
	case *mongo.UpdateManyModel:
		return buildUpdateManyModelWithTrace(ctx, vm, prop)
	default:
		return m, nil
	}
}

func buildInsertOneModelWithTrace(ctx context.Context, vm *mongo.InsertOneModel, prop propagation.TextMapPropagator) (mongo.WriteModel, error) {
	doc, ok := getInsertOneModelDocument(vm)
	if !ok {
		return vm, nil
	}
	docWithTrace, err := InjectTraceIntoDocument(ctx, doc, prop)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: bulk insert inject trace: %w", err)
	}
	return mongo.NewInsertOneModel().SetDocument(docWithTrace), nil
}

func buildUpdateOneModelWithTrace(ctx context.Context, vm *mongo.UpdateOneModel, prop propagation.TextMapPropagator) (mongo.WriteModel, error) {
	update, ok := getUpdateModelFilterUpdate(vm)
	if !ok {
		return vm, nil
	}
	updateWithTrace, err := InjectTraceIntoUpdate(ctx, update, prop)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: bulk updateOne inject trace: %w", err)
	}
	newModel := *vm
	newModel.Update = updateWithTrace
	return &newModel, nil
}

func buildUpdateManyModelWithTrace(ctx context.Context, vm *mongo.UpdateManyModel, prop propagation.TextMapPropagator) (mongo.WriteModel, error) {
	update, ok := getUpdateManyModelFilterUpdate(vm)
	if !ok {
		return vm, nil
	}
	updateWithTrace, err := InjectTraceIntoUpdate(ctx, update, prop)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: bulk updateMany inject trace: %w", err)
	}
	newModel := *vm
	newModel.Update = updateWithTrace
	return &newModel, nil
}

func getInsertOneModelDocument(m *mongo.InsertOneModel) (any, bool) {
	if m == nil {
		return nil, false
	}
	v := reflect.ValueOf(m).Elem()
	f := v.FieldByName("Document")
	if !f.IsValid() || !f.CanInterface() {
		return nil, false
	}
	return f.Interface(), true
}

func getUpdateModelFilterUpdate(m *mongo.UpdateOneModel) (update any, ok bool) {
	if m == nil {
		return nil, false
	}
	v := reflect.ValueOf(m).Elem()
	updateF := v.FieldByName("Update")
	if !updateF.IsValid() || !updateF.CanInterface() {
		return nil, false
	}
	return updateF.Interface(), true
}

func getUpdateManyModelFilterUpdate(m *mongo.UpdateManyModel) (update any, ok bool) {
	if m == nil {
		return nil, false
	}
	v := reflect.ValueOf(m).Elem()
	updateF := v.FieldByName("Update")
	if !updateF.IsValid() || !updateF.CanInterface() {
		return nil, false
	}
	return updateF.Interface(), true
}
