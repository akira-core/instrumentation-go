// Package shared holds interfaces and helpers reused by both the direct
// (disabled-mode) and traced (enabled-mode) WebSocket impls.
package shared

import "context"

// ConnImpl is the strategy-mode interface satisfied by both
// internal/direct.Conn (passthrough, zero OTel SDK code) and
// internal/traced.Conn (full instrumentation). The facade *Conn dispatches
// every public method through this interface so runtime gates disappear
// from public method bodies.
type ConnImpl interface {
	WriteMessage(ctx context.Context, messageType int, data []byte) error
	ReadMessage(ctx context.Context) (context.Context, int, []byte, error)
}
