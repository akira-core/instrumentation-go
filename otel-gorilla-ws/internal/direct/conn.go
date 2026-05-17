// Package direct holds the disabled-mode WebSocket impl: zero spans,
// zero envelope, pure passthrough to *websocket.Conn. Compiler-enforced
// isolation: this package SHALL NOT import any go.opentelemetry.io/otel/sdk/*
// or go.opentelemetry.io/otel/exporters/* packages.
package direct

import (
	"context"

	"github.com/gorilla/websocket"
)

// Conn delegates every method to the underlying *websocket.Conn unchanged.
type Conn struct {
	WS *websocket.Conn
}

// NewConn returns a passthrough impl wrapping ws.
func NewConn(ws *websocket.Conn) *Conn {
	return &Conn{WS: ws}
}

// WriteMessage delegates to the upstream gorilla connection with no span
// and no envelope wrap.
func (c *Conn) WriteMessage(ctx context.Context, messageType int, data []byte) error {
	_ = ctx
	return c.WS.WriteMessage(messageType, data)
}

// ReadMessage delegates to the upstream gorilla connection with no span,
// no envelope parse, and returns the caller-supplied context unchanged.
func (c *Conn) ReadMessage(ctx context.Context) (context.Context, int, []byte, error) {
	msgType, raw, err := c.WS.ReadMessage()
	return ctx, msgType, raw, err
}
