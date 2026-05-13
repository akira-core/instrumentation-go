package shared

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// TestInspectWriteModelFields verifies that the field names used by reflection-based
// bulk write trace injection still exist on the driver's WriteModel types.
func TestInspectWriteModelFields(t *testing.T) {
	ins := mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "x", Value: 1}})
	updOne := mongo.NewUpdateOneModel().SetFilter(bson.D{}).SetUpdate(bson.D{{Key: "$set", Value: bson.D{}}})
	updMany := mongo.NewUpdateManyModel().SetFilter(bson.D{}).SetUpdate(bson.D{{Key: "$set", Value: bson.D{}}})

	assertField(t, ins, "Document")
	assertField(t, updOne, "Filter")
	assertField(t, updOne, "Update")
	assertField(t, updMany, "Filter")
	assertField(t, updMany, "Update")
}

func assertField(t *testing.T, model any, fieldName string) {
	t.Helper()
	v := reflect.ValueOf(model).Elem()
	f := v.FieldByName(fieldName)
	assert.True(t, f.IsValid(), "%T: field %q not found", model, fieldName)
	if f.IsValid() {
		assert.True(t, f.CanInterface(), "%T: field %q is unexported", model, fieldName)
	}
}
