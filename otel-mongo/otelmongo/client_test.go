package otelmongo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestMongoDeliverSpanDisabledWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tp, tracer := initMongoProvider(context.Background(), "localhost", 27017)
	assert.Nil(t, tp, "expected nil TracerProvider when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
	assert.Nil(t, tracer, "expected nil Tracer when OTEL_EXPORTER_OTLP_ENDPOINT is unset")
}

func TestMongoDeliverSpanEnabledWithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	tp, tracer := initMongoProvider(context.Background(), "localhost", 27017)
	assert.NotNil(t, tp, "expected non-nil TracerProvider when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	assert.NotNil(t, tracer, "expected non-nil Tracer when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	if tp != nil {
		tp.Shutdown(t.Context()) //nolint:errcheck
	}
}

func TestMongoServiceName(t *testing.T) {
	cases := []struct {
		addr string
		port int
		want string
	}{
		{"", 0, "mongodb"},
		{"localhost", 27017, "mongodb://localhost"},
		{"localhost", 27018, "mongodb://localhost:27018"},
		{"myhost", 0, "mongodb://myhost"},
	}
	for _, tc := range cases {
		got := mongoServiceName(tc.addr, tc.port)
		assert.Equal(t, tc.want, got, "mongoServiceName(%q, %d)", tc.addr, tc.port)
	}
}

func TestParseServerFromClientOptions(t *testing.T) {
	t.Run("nil options", func(t *testing.T) {
		addr, port := parseServerFromClientOptions(nil)
		require.Empty(t, addr)
		require.Zero(t, port)
	})

	t.Run("without apply uri", func(t *testing.T) {
		addr, port := parseServerFromClientOptions(options.Client())
		require.Empty(t, addr)
		require.Zero(t, port)
	})

	t.Run("with apply uri", func(t *testing.T) {
		addr, port := parseServerFromClientOptions(options.Client().ApplyURI("mongodb://mongo:27018"))
		require.Equal(t, "mongo", addr)
		require.Equal(t, 27018, port)
	})
}
