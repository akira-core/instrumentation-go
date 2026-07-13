// Command-monitor-based per-command server address capture. Correlates a
// CommandStartedEvent's ConnectionID with the traced Collection method call
// that triggered it via a context-scoped holder, so CLIENT spans can carry
// the address of the connection that actually served the command instead of
// a value parsed once from the connection URI at Connect time.
package shared

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/event"
)

// parseConnectionID extracts the server address/port from a driver-internal
// ConnectionID, format "<addr>[-<n>]" (e.g. "host:27017[-1]"). Splits on the
// LAST '[' rather than the first, since an IPv6 addr already contains a
// leading '[' (e.g. "[::1]:27017[-1]"). Returns ("", 0) on any parse failure
// so callers fall back to the static address.
func parseConnectionID(connID string) (addr string, port int) {
	if i := strings.LastIndexByte(connID, '['); i >= 0 {
		connID = connID[:i]
	}
	return SplitHostPort(connID)
}

type addrCaptureKey struct{}

// AddrCapture is a per-call holder written by NewCommandMonitor's Started
// callback and read by the traced Collection method after the raw driver
// call returns. Never shared across concurrent calls — WithAddrCapture mints
// a fresh holder per call, and the write (in Started, same goroutine as the
// raw driver call) happens-before the read (after the raw call returns).
type AddrCapture struct {
	addr string
	port int
}

// WithAddrCapture stashes a fresh holder into ctx and returns both. Pass the
// returned ctx to the raw driver call so a synchronously-fired
// CommandStartedEvent can find and write into the holder.
func WithAddrCapture(ctx context.Context) (context.Context, *AddrCapture) {
	c := &AddrCapture{}
	return context.WithValue(ctx, addrCaptureKey{}, c), c
}

// Resolve returns the captured address/port, falling back to (fallbackAddr,
// fallbackPort) when nothing was captured for this call.
func (c *AddrCapture) Resolve(fallbackAddr string, fallbackPort int) (addr string, port int) {
	if c == nil || c.addr == "" {
		return fallbackAddr, fallbackPort
	}
	return c.addr, c.port
}

func addrCaptureFromContext(ctx context.Context) *AddrCapture {
	c, _ := ctx.Value(addrCaptureKey{}).(*AddrCapture)
	return c
}

// NewCommandMonitor returns an *event.CommandMonitor whose Started callback
// captures the per-command server address into the ctx-scoped holder (see
// WithAddrCapture) before delegating to existing.Started, when non-nil.
// Succeeded/Failed are pure pass-throughs to existing's callbacks when set,
// no-ops otherwise. A nil existing is valid (no caller monitor to chain).
func NewCommandMonitor(existing *event.CommandMonitor) *event.CommandMonitor {
	m := &event.CommandMonitor{
		Started: func(ctx context.Context, ev *event.CommandStartedEvent) {
			if capture := addrCaptureFromContext(ctx); capture != nil {
				capture.addr, capture.port = parseConnectionID(ev.ConnectionID)
			}
			if existing != nil && existing.Started != nil {
				existing.Started(ctx, ev)
			}
		},
	}
	if existing != nil && existing.Succeeded != nil {
		m.Succeeded = existing.Succeeded
	}
	if existing != nil && existing.Failed != nil {
		m.Failed = existing.Failed
	}
	return m
}
