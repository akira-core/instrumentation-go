package otelmongo

import (
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Database wraps *mongo.Database for document-level tracing.
type Database struct {
	*mongo.Database
	serverAddr         string
	serverPort         int
	tracer             trace.Tracer
	propagator         propagation.TextMapPropagator
	propagationEnabled bool
	deliverTracer      trace.Tracer
}

// Collection returns a Collection with document-level trace propagation.
func (d *Database) Collection(name string, opts ...options.Lister[options.CollectionOptions]) *Collection {
	return &Collection{
		Collection:         d.Database.Collection(name, opts...),
		tracer:             d.tracer,
		propagator:         d.propagator,
		propagationEnabled: d.propagationEnabled,
		serverAddr:         d.serverAddr,
		serverPort:         d.serverPort,
		deliverTracer:      d.deliverTracer,
	}
}
