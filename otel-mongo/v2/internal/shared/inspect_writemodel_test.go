package shared

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestInspectWriteModelFields verifies driver field names used by reflection.
func TestInspectWriteModelFields(t *testing.T) {
	ins := mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "x", Value: 1}})
	upd := mongo.NewUpdateOneModel().SetFilter(bson.D{}).SetUpdate(bson.D{{Key: "$set", Value: bson.D{}}})

	assertField(t, ins, "Document")
	assertField(t, upd, "Filter")
	assertField(t, upd, "Update")
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
