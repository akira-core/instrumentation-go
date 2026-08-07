package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	otelgorillaws "github.com/akira-core/instrumentation-go/otel-gorilla-ws"
)

func TestIntegration_RoundTrip_TraceContextPropagation(t *testing.T) {
	recorder := newIntegrationTP(t)

	upgrader := &otelgorillaws.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert, not require: this runs on the server's goroutine, where a
		// require failure would Goexit the handler instead of failing the test.
		conn, err := upgrader.Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer conn.Close()

		outCtx, typ, payload, err := conn.ReadMessage(context.Background())
		if !assert.NoError(t, err) {
			return
		}
		assert.NoError(t, conn.WriteMessage(outCtx, typ, payload))
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	tracer := otel.Tracer("integration-test")
	sendCtx, sendSpan := tracer.Start(context.Background(), "client-root")
	payload := []byte(`{"kind":"propagation"}`)
	require.NoError(t, conn.WriteMessage(sendCtx, websocket.TextMessage, payload))
	sendSpan.End()

	recvCtx, typ, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, typ)
	assert.Equal(t, payload, got)

	gotSC := oteltrace.SpanContextFromContext(recvCtx)
	assert.True(t, gotSC.IsValid(), "read context should carry extracted remote span context")

	var receiveSpans []sdktrace.ReadOnlySpan
	for _, sp := range recorder.Ended() {
		if sp.Name() == "websocket.receive" {
			receiveSpans = append(receiveSpans, sp)
		}
	}
	require.NotEmpty(t, receiveSpans)
	assert.NotEmpty(t, receiveSpans[len(receiveSpans)-1].Links())
}
