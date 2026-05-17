package otelmongo

import (
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/direct"
	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// Database wraps *mongo.Database for document-level tracing.
//
// Disabled-mode invariant via nullable pointer: `traced` is nil whenever
// the parent Client was constructed with the mongo tracing gate off. A nil
// *traced.DatabaseState carries no OTel SDK state, so Collection() cannot
// reach any SDK code path on the disabled branch — direct.NewCollection
// returns the passthrough impl which has zero `otel/sdk` imports.
type Database struct {
	*mongo.Database
	traced *traced.DatabaseState // nil ⇔ disabled (inherited from parent Client)
}

// Collection returns a Collection with document-level trace propagation.
// Constructor-site impl selection (exempt from the no-runtime-branch rule
// per `instrumentation-feature-flags` Scenario "Constructor-site impl
// selection is exempt"): nil parent state ⇒ *direct.Collection, non-nil ⇒
// *traced.Collection inheriting the parent's tracer/propagator/etc.
func (d *Database) Collection(name string, opts ...*options.CollectionOptions) *Collection {
	raw := d.Database.Collection(name, opts...)
	if d.traced == nil {
		return &Collection{Collection: raw, impl: direct.NewCollection(raw)}
	}
	return &Collection{Collection: raw, impl: d.traced.NewCollection(raw)}
}
