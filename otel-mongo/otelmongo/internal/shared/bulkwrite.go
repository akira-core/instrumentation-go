package shared

import (
	"context"
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel/propagation"
)

// BuildBulkWriteModelsWithTrace returns a new slice of WriteModels with _oteltrace
// injected into InsertOneModel, UpdateOneModel, and UpdateManyModel.
func BuildBulkWriteModelsWithTrace(ctx context.Context, models []mongo.WriteModel, prop propagation.TextMapPropagator) ([]mongo.WriteModel, error) {
	out := make([]mongo.WriteModel, 0, len(models))
	for _, m := range models {
		switch vm := m.(type) {
		case *mongo.InsertOneModel:
			doc, ok := getInsertOneModelDocument(vm)
			if !ok {
				out = append(out, m)
				continue
			}
			docWithTrace, err := InjectTraceIntoDocument(ctx, doc, prop)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: bulk insert inject trace: %w", err)
			}
			out = append(out, mongo.NewInsertOneModel().SetDocument(docWithTrace))
		case *mongo.UpdateOneModel:
			update, ok := getUpdateOneModelFilterUpdate(vm)
			if !ok {
				out = append(out, m)
				continue
			}
			updateWithTrace, err := InjectTraceIntoUpdate(ctx, update, prop)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: bulk updateOne inject trace: %w", err)
			}
			newModel := *vm
			newModel.Update = updateWithTrace
			out = append(out, &newModel)
		case *mongo.UpdateManyModel:
			manyUpdate, ok := getUpdateManyModelFilterUpdate(vm)
			if !ok {
				out = append(out, m)
				continue
			}
			updateWithTrace, err := InjectTraceIntoUpdate(ctx, manyUpdate, prop)
			if err != nil {
				return nil, fmt.Errorf("otelmongo: bulk updateMany inject trace: %w", err)
			}
			newModel := *vm
			newModel.Update = updateWithTrace
			out = append(out, &newModel)
		default:
			out = append(out, m)
		}
	}
	return out, nil
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

func getUpdateOneModelFilterUpdate(m *mongo.UpdateOneModel) (update any, ok bool) {
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
