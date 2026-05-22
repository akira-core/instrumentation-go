package otelgorillaws

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Internal-package test so we can exercise the unexported pure functions
// (splitClientProtocols, selectFirst, cloneHeader, appProtocolFromRaw,
// isOTelWireProtocol) directly. These were previously only covered via the
// end-to-end handshake scenarios — direct unit tests pin every branch.

// TestSplitClientProtocols_TableDriven exhaustively covers the three
// client-side inputs the upgrader must split out:
//
//   - bare "otel-ws" token (JS otel-rxjs-ws default)
//   - prefixed "otel-ws+<app>" token (legacy Go client wire format)
//   - app-only tokens (no otel-ws involvement)
//
// The function returns (otelRequested, appProtos). otelRequested must be
// true whenever EITHER variant was found; appProtos must contain only the
// non-otel-ws tokens with prefixes stripped.
func TestSplitClientProtocols_TableDriven(t *testing.T) {
	cases := []struct {
		name          string
		in            []string
		wantOTel      bool
		wantAppProtos []string
	}{
		{"empty_input", nil, false, []string{}},
		{"only_app_protocols", []string{"json", "msgpack"}, false, []string{"json", "msgpack"}},
		{"bare_otel_ws", []string{"otel-ws"}, true, []string{}},
		{"prefixed_only", []string{"otel-ws+json"}, true, []string{"json"}},
		{"otel_ws_with_app_after", []string{"otel-ws", "json"}, true, []string{"json"}},
		{"otel_ws_with_app_before", []string{"json", "otel-ws"}, true, []string{"json"}},
		{"prefixed_with_others", []string{"otel-ws+json", "msgpack"}, true, []string{"json", "msgpack"}},
		{"bare_and_prefixed", []string{"otel-ws", "otel-ws+json"}, true, []string{"json"}},
		{"prefixed_with_empty_app", []string{"otel-ws+"}, true, []string{}}, // empty trimmed value not appended
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOTel, gotApp := splitClientProtocols(tc.in)
			assert.Equal(t, tc.wantOTel, gotOTel, "otelRequested flag")
			assert.Equal(t, tc.wantAppProtos, gotApp, "app protocols")
		})
	}
}

// TestSelectFirst_TableDriven covers the negotiation function the server uses
// after splitClientProtocols: pick the first client app protocol that the
// server also accepts. Accept-any semantics when server is nil.
func TestSelectFirst_TableDriven(t *testing.T) {
	cases := []struct {
		name   string
		client []string
		server []string
		want   string
	}{
		{"both_empty", nil, nil, ""},
		{"client_empty_server_nonempty", nil, []string{"json"}, ""},
		{"client_nonempty_server_nil_accept_any", []string{"json"}, nil, "json"},
		{"first_match_wins", []string{"json", "msgpack"}, []string{"msgpack", "json"}, "json"},
		{"no_match", []string{"json"}, []string{"msgpack"}, ""},
		{"single_match", []string{"json"}, []string{"json"}, "json"},
		{"empty_server_slice_no_match", []string{"json"}, []string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectFirst(tc.client, tc.server)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCloneHeader covers cloneHeader: must always return a non-nil header
// (even for nil input), with every entry copied. Verifies the clone is
// independent from the source — mutating one must not affect the other.
func TestCloneHeader(t *testing.T) {
	t.Run("nil_input_returns_empty_non_nil", func(t *testing.T) {
		got := cloneHeader(nil)
		require.NotNil(t, got)
		assert.Empty(t, got)
	})
	t.Run("copies_all_entries", func(t *testing.T) {
		src := http.Header{
			"X-Single": []string{"v1"},
			"X-Multi":  []string{"a", "b"},
		}
		got := cloneHeader(src)
		assert.Equal(t, "v1", got.Get("X-Single"))
		assert.Equal(t, []string{"a", "b"}, got["X-Multi"])
	})
	t.Run("clone_is_independent", func(t *testing.T) {
		src := http.Header{"X": []string{"original"}}
		got := cloneHeader(src)
		got.Set("X", "mutated")
		assert.Equal(t, "original", src.Get("X"),
			"mutating the clone must not affect the source")
	})
}

// TestAppProtocolFromRaw covers the small helper that strips the "otel-ws+"
// prefix from a negotiated subprotocol token. The bare "otel-ws" case must
// return empty string (no app protocol negotiated).
func TestAppProtocolFromRaw(t *testing.T) {
	cases := map[string]string{
		"":                "",     // no proto
		"json":            "json", // bare app, no prefix
		"otel-ws":         "",     // bare otel-ws, no app proto
		"otel-ws+json":    "json", // typical
		"otel-ws+msgpack": "msgpack",
	}
	for in, want := range cases {
		t.Run(in+"_to_"+want, func(t *testing.T) {
			assert.Equal(t, want, appProtocolFromRaw(in))
		})
	}
}

// TestIsOTelWireProtocol covers the small helper that classifies a raw
// subprotocol token as "carries the otel envelope".
func TestIsOTelWireProtocol(t *testing.T) {
	cases := map[string]bool{
		"":             false,
		"json":         false,
		"otel-ws":      true,
		"otel-ws+json": true,
		"otel-ws-x":    false, // suffix must be the "+" separator, not a dash
		"OTel-ws":      false, // case-sensitive per wire spec
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, isOTelWireProtocol(in))
		})
	}
}
