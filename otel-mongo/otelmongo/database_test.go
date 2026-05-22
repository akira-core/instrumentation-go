package otelmongo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// TestClientStateForDatabaseInheritsGates locks in the inheritance chain
// Client → ClientState.ForDatabase() → DatabaseState. All four gate-bearing
// fields must propagate verbatim so the Collection built downstream observes
// the same effective gates the Client was constructed with.
func TestClientStateForDatabaseInheritsGates(t *testing.T) {
	c := &traced.ClientState{
		Tracer:             otel.Tracer("test"),
		Propagator:         propagation.TraceContext{},
		PropagationEnabled: true,
		DeliverTracer:      otel.Tracer("deliver"),
		ServerAddr:         "myhost",
		ServerPort:         27018,
	}
	d := c.ForDatabase()
	require.NotNil(t, d)
	assert.True(t, d.PropagationEnabled, "PropagationEnabled must propagate")
	assert.Equal(t, "myhost", d.ServerAddr)
	assert.Equal(t, 27018, d.ServerPort)
	// trace.Tracer is an interface; assert.Same requires pointers, so we
	// fall back to assert.Equal which compares the interface values.
	assert.Equal(t, c.Tracer, d.Tracer, "Tracer interface value must propagate verbatim")
	assert.Equal(t, c.DeliverTracer, d.DeliverTracer, "DeliverTracer interface value must propagate verbatim")
}

// TestDatabaseStateNewCollectionInheritsGates locks in the second leg of the
// chain: DatabaseState.NewCollection() → traced.Collection must carry the
// same gates forward unchanged.
func TestDatabaseStateNewCollectionInheritsGates(t *testing.T) {
	d := &traced.DatabaseState{
		Tracer:             otel.Tracer("test"),
		Propagator:         propagation.TraceContext{},
		PropagationEnabled: true,
		DeliverTracer:      otel.Tracer("deliver"),
		ServerAddr:         "myhost",
		ServerPort:         27018,
	}
	coll := d.NewCollection(nil)
	require.NotNil(t, coll)
	assert.True(t, coll.PropagationEnabled)
	assert.Equal(t, "myhost", coll.ServerAddr)
	assert.Equal(t, 27018, coll.ServerPort)
	assert.Equal(t, d.Tracer, coll.Tracer, "Tracer interface value must propagate verbatim")
	assert.Equal(t, d.DeliverTracer, coll.DeliverTracer, "DeliverTracer interface value must propagate verbatim")
}

// TestClientStateForDatabasePropagationOffPropagates locks in that the disabled
// state propagates: when the parent client decided propagation is off, the
// child Database / Collection must inherit that decision and NOT re-resolve
// it from env vars (which could have flipped after Connect).
func TestClientStateForDatabasePropagationOffPropagates(t *testing.T) {
	c := &traced.ClientState{PropagationEnabled: false}
	d := c.ForDatabase()
	assert.False(t, d.PropagationEnabled)
	coll := d.NewCollection(nil)
	assert.False(t, coll.PropagationEnabled, "Collection must inherit PropagationEnabled=false unchanged")
}
