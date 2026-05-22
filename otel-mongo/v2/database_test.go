package otelmongo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// TestClientStateForDatabaseInheritsGatesV2 — parity with v1.
func TestClientStateForDatabaseInheritsGatesV2(t *testing.T) {
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
	assert.True(t, d.PropagationEnabled)
	assert.Equal(t, "myhost", d.ServerAddr)
	assert.Equal(t, 27018, d.ServerPort)
	assert.Equal(t, c.Tracer, d.Tracer)
	assert.Equal(t, c.DeliverTracer, d.DeliverTracer)
}

func TestDatabaseStateNewCollectionInheritsGatesV2(t *testing.T) {
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
	assert.Equal(t, d.Tracer, coll.Tracer)
	assert.Equal(t, d.DeliverTracer, coll.DeliverTracer)
}

func TestClientStateForDatabasePropagationOffPropagatesV2(t *testing.T) {
	c := &traced.ClientState{PropagationEnabled: false}
	d := c.ForDatabase()
	assert.False(t, d.PropagationEnabled)
	coll := d.NewCollection(nil)
	assert.False(t, coll.PropagationEnabled)
}
