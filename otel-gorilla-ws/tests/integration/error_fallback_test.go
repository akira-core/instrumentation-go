package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	otelgorillaws "github.com/akira-core/instrumentation-go/otel-gorilla-ws"
)

func TestIntegration_Fallback_NonEnvelopeAndClose(t *testing.T) {
	recorder := newIntegrationTP(t)

	// Plain gorilla upgrader: no otel-ws support, validates passthrough fallback.
	plainUpgrader := websocket.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawConn, err := plainUpgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer rawConn.Close()

		_, payload, err := rawConn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, rawConn.WriteMessage(websocket.TextMessage, payload))

		require.NoError(t, rawConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
			time.Now().Add(time.Second),
		))
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	input := []byte("not-an-envelope")
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, input))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, input, got)

	_, _, _, err = conn.ReadMessage(context.Background())
	require.Error(t, err, "second read should return close error from server")
	assert.GreaterOrEqual(t, len(recorder.Ended()), 3)
}
