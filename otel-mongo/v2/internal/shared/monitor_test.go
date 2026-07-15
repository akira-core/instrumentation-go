package shared

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/event"
)

func TestParseConnectionID(t *testing.T) {
	cases := []struct {
		name     string
		connID   string
		wantAddr string
		wantPort int
	}{
		{"host with suffix", "host:27017[-1]", "host", 27017},
		{"host with larger suffix", "host:27017[-42]", "host", 27017},
		{"non-default port", "host:27018[-1]", "host", 27018},
		{"ipv6 with suffix", "[::1]:27017[-1]", "::1", 27017},
		{"no port defaults to 27017", "host[-1]", "host", 27017},
		{"malformed empty", "", "", 0},
		{"malformed only suffix", "[-1]", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, port := parseConnectionID(tc.connID)
			assert.Equal(t, tc.wantAddr, addr)
			assert.Equal(t, tc.wantPort, port)
		})
	}
}

func TestNewCommandMonitor_CapturesAddress(t *testing.T) {
	monitor := NewCommandMonitor(nil)
	ctx, capture := WithAddrCapture(context.Background())
	monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "realhost:27018[-1]"})
	addr, port := capture.Resolve("fallback", 1)
	assert.Equal(t, "realhost", addr)
	assert.Equal(t, 27018, port)
}

// TestNewCommandMonitor_LastWriteWinsOnRetry pins the retry semantics: when the
// driver fires Started more than once for one logical operation (server
// selection retry), the span reports the server that actually executed it —
// the last Started event wins.
func TestNewCommandMonitor_LastWriteWinsOnRetry(t *testing.T) {
	monitor := NewCommandMonitor(nil)
	ctx, capture := WithAddrCapture(context.Background())
	monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "first-host:27017[-1]"})
	monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "second-host:27018[-2]"})
	addr, port := capture.Resolve("fallback", 1)
	assert.Equal(t, "second-host", addr)
	assert.Equal(t, 27018, port)
}

func TestNewCommandMonitor_ChainsExistingCallbacks(t *testing.T) {
	var startedCalled, succeededCalled, failedCalled bool
	existing := &event.CommandMonitor{
		Started:   func(context.Context, *event.CommandStartedEvent) { startedCalled = true },
		Succeeded: func(context.Context, *event.CommandSucceededEvent) { succeededCalled = true },
		Failed:    func(context.Context, *event.CommandFailedEvent) { failedCalled = true },
	}
	monitor := NewCommandMonitor(existing)
	ctx, capture := WithAddrCapture(context.Background())

	monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "host:27017[-1]"})
	require.NotNil(t, monitor.Succeeded)
	require.NotNil(t, monitor.Failed)
	monitor.Succeeded(ctx, &event.CommandSucceededEvent{})
	monitor.Failed(ctx, &event.CommandFailedEvent{})

	assert.True(t, startedCalled, "expected chained Started to fire")
	assert.True(t, succeededCalled, "expected chained Succeeded to fire")
	assert.True(t, failedCalled, "expected chained Failed to fire")
	addr, _ := capture.Resolve("", 0)
	assert.Equal(t, "host", addr, "expected our Started to still capture the address")
}

func TestNewCommandMonitor_NilCallerSubsetDoesNotPanic(t *testing.T) {
	existing := &event.CommandMonitor{Succeeded: func(context.Context, *event.CommandSucceededEvent) {}}
	monitor := NewCommandMonitor(existing)
	ctx, capture := WithAddrCapture(context.Background())

	assert.NotPanics(t, func() {
		monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "host:27017[-1]"})
	})
	assert.Nil(t, monitor.Failed, "no caller Failed callback set → Failed stays nil, not a no-op wrapper")

	addr, _ := capture.Resolve("", 0)
	assert.Equal(t, "host", addr)
}

func TestAddrCapture_ResolveFallback(t *testing.T) {
	var capture *AddrCapture
	addr, port := capture.Resolve("fallback-host", 27019)
	assert.Equal(t, "fallback-host", addr)
	assert.Equal(t, 27019, port)

	_, fresh := WithAddrCapture(context.Background())
	addr, port = fresh.Resolve("fallback-host", 27019)
	assert.Equal(t, "fallback-host", addr)
	assert.Equal(t, 27019, port)
}
