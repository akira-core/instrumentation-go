package shared

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel/propagation"
)

// BuildBulkWriteModelsWithTrace returns a new slice of WriteModels with _oteltrace
// injected into InsertOneModel, UpdateOneModel, and UpdateManyModel.
//
// Performance:
//   - Calls traceMetadataFromContext ONCE (propagator.Inject runs once, not
//     once per model). When ctx has no valid span context, returns models
//     unchanged with zero allocations beyond the result slice header.
//   - Reads InsertOneModel.Document / UpdateOneModel.Update / UpdateManyModel.Update
//     via direct type-assertion field access; the previous reflect.ValueOf
//     path is removed.
func BuildBulkWriteModelsWithTrace(ctx context.Context, models []mongo.WriteModel, prop propagation.TextMapPropagator) ([]mongo.WriteModel, error) {
	meta, hasMeta := traceMetadataFromContext(ctx, prop)
	if !hasMeta {
		return models, nil
	}

	out := make([]mongo.WriteModel, 0, len(models))
	for _, m := range models {
		traced, err := buildSingleModelWithTrace(m, meta)
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
func buildSingleModelWithTrace(m mongo.WriteModel, meta TraceMetadata) (mongo.WriteModel, error) {
	switch vm := m.(type) {
	case *mongo.InsertOneModel:
		return buildInsertOneModelWithTrace(vm, meta)
	case *mongo.UpdateOneModel:
		return buildUpdateOneModelWithTrace(vm, meta)
	case *mongo.UpdateManyModel:
		return buildUpdateManyModelWithTrace(vm, meta)
	default:
		return m, nil
	}
}

func buildInsertOneModelWithTrace(vm *mongo.InsertOneModel, meta TraceMetadata) (mongo.WriteModel, error) {
	if vm == nil || vm.Document == nil {
		return vm, nil
	}
	docWithTrace, err := injectMetadataIntoDocument(vm.Document, meta)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: bulk insert inject trace: %w", err)
	}
	return mongo.NewInsertOneModel().SetDocument(docWithTrace), nil
}

func buildUpdateOneModelWithTrace(vm *mongo.UpdateOneModel, meta TraceMetadata) (mongo.WriteModel, error) {
	if vm == nil || vm.Update == nil {
		return vm, nil
	}
	updateWithTrace, err := injectMetadataIntoUpdate(vm.Update, meta)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: bulk updateOne inject trace: %w", err)
	}
	newModel := *vm
	newModel.Update = updateWithTrace
	return &newModel, nil
}

func buildUpdateManyModelWithTrace(vm *mongo.UpdateManyModel, meta TraceMetadata) (mongo.WriteModel, error) {
	if vm == nil || vm.Update == nil {
		return vm, nil
	}
	updateWithTrace, err := injectMetadataIntoUpdate(vm.Update, meta)
	if err != nil {
		return nil, fmt.Errorf("otelmongo: bulk updateMany inject trace: %w", err)
	}
	newModel := *vm
	newModel.Update = updateWithTrace
	return &newModel, nil
}
