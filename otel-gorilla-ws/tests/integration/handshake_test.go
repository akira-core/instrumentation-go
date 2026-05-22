package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelgorillaws "github.com/Marz32onE/instrumentation-go/otel-gorilla-ws"
)

func newIntegrationTP(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
	return recorder
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestIntegration_Handshake_OtelWSOnlyNoAppProtocol verifies that when the
// client offers only "otel-ws" (no app subprotocol — the JS otel-rxjs-ws
// default), the server responds with bare "otel-ws" and NOT "otel-ws+".
func TestIntegration_Handshake_OtelWSOnlyNoAppProtocol(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")
	upgrader := &otelgorillaws.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
		// No AppSubprotocols: accept-any semantics, negotiated will be "".
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		assert.Equal(t, "", conn.Subprotocol()) // no app proto negotiated

		ctx, typ, payload, err := conn.ReadMessage(context.Background())
		require.NoError(t, err)
		_ = conn.WriteMessage(ctx, typ, payload)
	}))
	defer srv.Close()

	// Simulate JS client: offer only "otel-ws" with no app subprotocol.
	rawDialer := &websocket.Dialer{Subprotocols: []string{"otel-ws"}}
	rawConn, _, err := rawDialer.Dial(wsURL(srv), nil)
	require.NoError(t, err)
	defer rawConn.Close()

	// Server must respond with bare "otel-ws", NOT "otel-ws+".
	assert.Equal(t, "otel-ws", rawConn.Subprotocol())

	// Send a message and receive the echoed response. Because the server Conn has
	// tracingEnabled=true it wraps the outgoing message in the otel wire envelope,
	// so the raw client sees {"header":...,"data":...} — not the original payload.
	// We only verify the round-trip completes without error.
	msg := []byte(`{"hello":"world"}`)
	require.NoError(t, rawConn.WriteMessage(websocket.TextMessage, msg))
	_, got, err := rawConn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(got), `"data"`, "server wraps response in otel wire envelope")
}

func TestIntegration_Handshake_SubprotocolNegotiation(t *testing.T) {
	recorder := newIntegrationTP(t)

	upgrader := &otelgorillaws.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"json"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		assert.Equal(t, "json", conn.Subprotocol())

		ctx, typ, payload, err := conn.ReadMessage(context.Background())
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(ctx, typ, payload))
	}))
	defer srv.Close()

	conn, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, []string{"json"})
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "json", conn.Subprotocol())

	input := []byte(`{"kind":"handshake"}`)
	require.NoError(t, conn.WriteMessage(context.Background(), websocket.TextMessage, input))
	_, _, got, err := conn.ReadMessage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, input, got)

	assert.GreaterOrEqual(t, len(recorder.Ended()), 2)
}

// TestHandshakeScenarios_All consolidates the 8 subprotocol-negotiation
// scenarios into a single table-driven suite. Scenario letters align with the
// sibling unit-level tests in otel-gorilla-ws/conn_test.go:
//
//   A) client offers "otel-ws" only (no app)    → negotiated "otel-ws"
//      [no direct conn_test equivalent; JS otel-rxjs-ws default]
//   B) OTel client offers "json"                → negotiated "otel-ws+json"
//      [≈ TestRoundTrip + TestSpanAttributes in conn_test.go]
//   C) OTel client offers "json" + plain srv    → negotiated "json", passthrough
//      [= TestDial_ScenarioC]
//   D) OTel client offers "json" + server ""    → negotiated "", passthrough
//      [= TestDial_ScenarioD]
//   E) OTel client offers nil                   → negotiated "", passthrough
//      [= TestDial_ScenarioE]
//   F) plain client no proto + OTel server      → negotiated "", passthrough
//      [= TestUpgrader_ScenarioF]
//   G) plain client offers "otel-ws+json"       → negotiated "otel-ws+json", traced
//      [= TestUpgrader_ScenarioG_FromPrefixedClientToken]
//   H) plain client offers "json" + OTel server → negotiated "json", passthrough
//      [= TestUpgrader_ScenarioH]
//
// Each row asserts: (1) negotiated subprotocol on the client side and (2)
// whether the outgoing payload was wrapped in the otel envelope.
func TestHandshakeScenarios_All(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_GORILLA_WS_TRACING_ENABLED", "1")

	type role int
	const (
		roleOTelServer role = iota
		rolePlainServerWithProtos
		rolePlainServerNoProtos
	)

	cases := []struct {
		name             string
		serverRole       role
		serverAppProtos  []string
		clientProtos     []string
		useOTelClient    bool
		wantClientProto  string
		wantPayloadEnvel bool
		scenario         string
	}{
		{
			name: "ScenarioA_OtelWSOnlyNoApp", scenario: "A",
			serverRole: roleOTelServer, serverAppProtos: nil,
			clientProtos: []string{"otel-ws"}, useOTelClient: false,
			wantClientProto: "otel-ws", wantPayloadEnvel: true,
		},
		{
			name: "ScenarioB_OtelClientNegotiatesJSON", scenario: "B",
			serverRole: roleOTelServer, serverAppProtos: []string{"json"},
			clientProtos: []string{"json"}, useOTelClient: true,
			wantClientProto: "json", wantPayloadEnvel: false,
		},
		{
			name: "ScenarioC_PlainServerReturnsJSON", scenario: "C",
			serverRole: rolePlainServerWithProtos, serverAppProtos: []string{"json"},
			clientProtos: []string{"json"}, useOTelClient: true,
			wantClientProto: "json", wantPayloadEnvel: false,
		},
		{
			name: "ScenarioD_ServerReturnsEmptyProto", scenario: "D",
			serverRole: rolePlainServerNoProtos, serverAppProtos: nil,
			clientProtos: []string{"json"}, useOTelClient: true,
			wantClientProto: "", wantPayloadEnvel: false,
		},
		{
			name: "ScenarioE_ClientProposesNoSubprotocol", scenario: "E",
			serverRole: rolePlainServerNoProtos, serverAppProtos: nil,
			clientProtos: nil, useOTelClient: true,
			wantClientProto: "", wantPayloadEnvel: false,
		},
		{
			name: "ScenarioF_PlainClientToOTelServer", scenario: "F",
			serverRole: roleOTelServer, serverAppProtos: []string{"json"},
			clientProtos: nil, useOTelClient: false,
			wantClientProto: "", wantPayloadEnvel: false,
		},
		{
			// Matches conn_test.go::TestUpgrader_ScenarioG_FromPrefixedClientToken
			name: "ScenarioG_LegacyPrefixedClientToken", scenario: "G",
			serverRole: roleOTelServer, serverAppProtos: []string{"json"},
			clientProtos: []string{"otel-ws+json"}, useOTelClient: false,
			wantClientProto: "otel-ws+json", wantPayloadEnvel: true,
		},
		{
			// Matches conn_test.go::TestUpgrader_ScenarioH
			name: "ScenarioH_PlainClientJSONOnly", scenario: "H",
			serverRole: roleOTelServer, serverAppProtos: []string{"json"},
			clientProtos: []string{"json"}, useOTelClient: false,
			wantClientProto: "json", wantPayloadEnvel: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
			otel.SetTracerProvider(tp)
			otel.SetTextMapPropagator(
				propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
			)
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			var handler http.HandlerFunc
			switch tc.serverRole {
			case roleOTelServer:
				up := &otelgorillaws.Upgrader{
					CheckOrigin:  func(r *http.Request) bool { return true },
					Subprotocols: tc.serverAppProtos,
				}
				handler = func(w http.ResponseWriter, r *http.Request) {
					c, err := up.Upgrade(w, r, nil)
					if err != nil {
						t.Errorf("upgrade: %v", err)
						return
					}
					defer c.Close()
					ctx, typ, data, err := c.ReadMessage(context.Background())
					if err != nil {
						return
					}
					_ = c.WriteMessage(ctx, typ, data)
				}
			case rolePlainServerWithProtos:
				up := websocket.Upgrader{
					CheckOrigin:  func(r *http.Request) bool { return true },
					Subprotocols: tc.serverAppProtos,
				}
				handler = func(w http.ResponseWriter, r *http.Request) {
					c, err := up.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer c.Close()
					_, data, err := c.ReadMessage()
					if err != nil {
						return
					}
					_ = c.WriteMessage(websocket.TextMessage, data)
				}
			case rolePlainServerNoProtos:
				up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
				handler = func(w http.ResponseWriter, r *http.Request) {
					c, err := up.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer c.Close()
					_, data, err := c.ReadMessage()
					if err != nil {
						return
					}
					_ = c.WriteMessage(websocket.TextMessage, data)
				}
			}

			srv := httptest.NewServer(handler)
			defer srv.Close()
			payload := []byte(`{"scenario":"` + tc.scenario + `"}`)

			if tc.useOTelClient {
				c, _, err := otelgorillaws.Dial(context.Background(), wsURL(srv), nil, tc.clientProtos)
				require.NoError(t, err)
				defer c.Close()
				assert.Equal(t, tc.wantClientProto, c.Subprotocol(),
					"Scenario %s: client must report negotiated app subprotocol with prefix stripped", tc.scenario)

				require.NoError(t, c.WriteMessage(context.Background(), websocket.TextMessage, payload))
				_, _, got, err := c.ReadMessage(context.Background())
				require.NoError(t, err)
				assert.Equal(t, payload, got,
					"Scenario %s: OTel client must see unwrapped payload after ReadMessage", tc.scenario)
			} else {
				d := &websocket.Dialer{Subprotocols: tc.clientProtos}
				c, _, err := d.Dial(wsURL(srv), nil)
				require.NoError(t, err)
				defer c.Close()
				assert.Equal(t, tc.wantClientProto, c.Subprotocol(),
					"Scenario %s: raw client must observe raw negotiated subprotocol token", tc.scenario)

				require.NoError(t, c.WriteMessage(websocket.TextMessage, payload))
				_, got, err := c.ReadMessage()
				require.NoError(t, err)
				if tc.wantPayloadEnvel {
					assert.Contains(t, string(got), `"data"`,
						"Scenario %s: server must wrap echo in OTel envelope when tracing negotiated", tc.scenario)
					assert.Contains(t, string(got), `"header"`,
						"Scenario %s: server envelope must contain header field", tc.scenario)
				} else {
					assert.Equal(t, payload, got,
						"Scenario %s: server must echo unwrapped payload in passthrough mode", tc.scenario)
				}
			}

			time.Sleep(20 * time.Millisecond)
			assert.NotEmpty(t, sr.Ended(),
				"Scenario %s: at least one wrapper span must be recorded server-side", tc.scenario)
		})
	}
}
