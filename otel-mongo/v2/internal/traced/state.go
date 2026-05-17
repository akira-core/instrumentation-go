package traced

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ClientState owns all OTel SDK state attached to an instrumented Client.
// The facade *Client holds a nullable *ClientState — nil ⇔ disabled mode.
type ClientState struct {
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
	DeliverTracer      trace.Tracer
	MongoTP            *sdktrace.TracerProvider
	ServerAddr         string
	ServerPort         int
}

// ShutdownDeliver shuts down the deliver TracerProvider with a 3-second
// best-effort timeout. Caller-guarded by `if c.traced != nil` on the facade.
func (c *ClientState) ShutdownDeliver(ctx context.Context) {
	if c.MongoTP == nil {
		return
	}
	shutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = c.MongoTP.Shutdown(shutCtx)
}

// ForDatabase returns the DatabaseState inheriting this client's gates.
func (c *ClientState) ForDatabase() *DatabaseState {
	return &DatabaseState{
		Tracer:             c.Tracer,
		Propagator:         c.Propagator,
		PropagationEnabled: c.PropagationEnabled,
		DeliverTracer:      c.DeliverTracer,
		ServerAddr:         c.ServerAddr,
		ServerPort:         c.ServerPort,
	}
}

// DatabaseState carries the gates from Client through Database to the
// Collection impl. The facade *Database holds a nullable *DatabaseState —
// nil ⇔ disabled mode.
type DatabaseState struct {
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
	PropagationEnabled bool
	DeliverTracer      trace.Tracer
	ServerAddr         string
	ServerPort         int
}

// NewCollection builds the traced Collection impl carrying this database's
// gates. Called from the constructor-site branch in facade Database.Collection.
func (d *DatabaseState) NewCollection(raw *mongo.Collection) *Collection {
	return &Collection{
		Coll:               raw,
		Tracer:             d.Tracer,
		Propagator:         d.Propagator,
		PropagationEnabled: d.PropagationEnabled,
		DeliverTracer:      d.DeliverTracer,
		ServerAddr:         d.ServerAddr,
		ServerPort:         d.ServerPort,
	}
}
