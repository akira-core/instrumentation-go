package otelmongo

import (
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// Database wraps *mongo.Database for document-level tracing.
//
// Disabled-mode invariant via nullable pointer: `traced` is nil whenever
// the parent Client was constructed with the mongo tracing gate off.
type Database struct {
	*mongo.Database
	traced *traced.DatabaseState // nil ⇔ disabled (inherited from parent Client)
}

// Collection returns a Collection with document-level trace propagation.
// Constructor-site impl selection (exempt from the no-runtime-branch rule):
// nil parent state ⇒ *direct.Collection, non-nil ⇒ *traced.Collection
// inheriting the parent's tracer/propagator/etc.
func (d *Database) Collection(name string, opts ...options.Lister[options.CollectionOptions]) *Collection {
	raw := d.Database.Collection(name, opts...)
	if d.traced == nil {
		return &Collection{Collection: raw, impl: direct.NewCollection(raw)}
	}
	return &Collection{Collection: raw, impl: d.traced.NewCollection(raw)}
}
