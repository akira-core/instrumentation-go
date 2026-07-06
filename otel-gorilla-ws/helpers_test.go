package otelgorillaws_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"

	otelgorillaws "github.com/akira-core/instrumentation-go/otel-gorilla-ws"
)

func TestIsCloseError(t *testing.T) {
	err := &otelgorillaws.CloseError{Code: otelgorillaws.CloseGoingAway, Text: "bye"}

	assert.True(t, otelgorillaws.IsCloseError(err, otelgorillaws.CloseGoingAway))
	assert.False(t, otelgorillaws.IsCloseError(err, otelgorillaws.CloseNormalClosure))
	assert.False(t, otelgorillaws.IsCloseError(errors.New("x"), otelgorillaws.CloseGoingAway))
}

func TestIsUnexpectedCloseError(t *testing.T) {
	err := &otelgorillaws.CloseError{Code: otelgorillaws.CloseGoingAway, Text: "bye"}

	assert.False(t, otelgorillaws.IsUnexpectedCloseError(err, otelgorillaws.CloseGoingAway))
	assert.True(t, otelgorillaws.IsUnexpectedCloseError(err, otelgorillaws.CloseNormalClosure))
}

func TestFormatCloseMessage(t *testing.T) {
	got := otelgorillaws.FormatCloseMessage(otelgorillaws.CloseGoingAway, "server shutdown")
	want := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutdown")
	assert.Equal(t, want, got)
}

func TestHTTPHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", "otel-ws, json")

	assert.True(t, otelgorillaws.IsWebSocketUpgrade(req))
	assert.Equal(t, []string{"otel-ws", "json"}, otelgorillaws.Subprotocols(req))
}
