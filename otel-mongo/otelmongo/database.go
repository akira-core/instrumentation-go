package otelmongo

import (
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Database wraps *mongo.Database for document-level tracing. The fields here
// hold the Client's resolved gates so Collection() can pick the right
// collectionImpl without re-reading env.
type Database struct {
	*mongo.Database
	serverAddr string
	serverPort int
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator

	// Inherited verbatim from the Client so a Database and its Collections
	// resolve exactly as the Client that produced them.
	gate gateState
}

// effectiveTracing reports whether THIS call should be instrumented.
func (d *Database) effectiveTracing() bool { return d.gate.effectiveTracing() }

// effectivePropagation reports whether THIS call should inject or extract
// _oteltrace. See gateState for static-client and R5 rules.
func (d *Database) effectivePropagation() bool { return d.gate.effectivePropagation() }

// Collection returns a Collection with document-level trace propagation.
func (d *Database) Collection(name string, opts ...*options.CollectionOptions) *Collection {
	return newCollectionForDatabase(d, d.Database.Collection(name, opts...))
}
