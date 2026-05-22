package otelnats_test

import (
	"context"
	"os"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	otelnats "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

// startBareServer starts a NATS server without touching any tracing env vars.
// Each table-driven case sets its own flag combination before calling this.
func startBareServer(t *testing.T) string {
	t.Helper()
	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
	s, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go s.Start()
	require.True(t, s.ReadyForConnections(5*time.Second), "nats-server not ready")
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

// applyFlag either sets or unsets the env var depending on the value: empty
// string means unset, anything else is set verbatim via t.Setenv.
func applyFlag(t *testing.T, key, value string) {
	t.Helper()
	if value == "" {
		_ = os.Unsetenv(key)
		return
	}
	t.Setenv(key, value)
}

// TestFlagMatrix_AllCombinations exhaustively covers all 8 combinations of
// (OTEL_INSTRUMENTATION_GO_TRACING_ENABLED × OTEL_NATS_TRACING_ENABLED ×
// OTEL_NATS_PROPAGATION_ENABLED) end-to-end against a real NATS server,
// asserting:
//
//  1. Conn.TracingEnabled() and Conn.PropagationEnabled() resolve to the
//     expected gate values.
//  2. When tracing is gated off, Publish emits NO wrapper spans of any kind
//     on the caller's TracerProvider (only the test's own parent span).
//  3. When propagation is gated off, Publish injects NO `traceparent` /
//     `tracestate` headers — wire output is byte-identical to native
//     *nats.Conn.Publish.
//  4. When all three gates are on, BOTH spans AND headers appear.
//
// This is the single integration test that ties the truth-table gates in
// env_flags_test.go to the actual on-wire behaviour callers observe.
func TestFlagMatrix_AllCombinations(t *testing.T) {
	cases := []struct {
		name              string
		global            string // "" = unset
		moduleTracing     string
		modulePropagation string
		wantTracing       bool
		wantPropagation   bool
	}{
		{"all_unset", "", "", "", false, false},
		{"global_only", "1", "", "", false, false},
		{"moduleTracing_only", "", "1", "", false, false},
		{"propagation_only", "", "", "1", false, false},
		{"global_off_others_on", "0", "1", "1", false, false},
		{"global_on_moduleTracing_off_prop_on", "1", "0", "1", false, false},
		{"global_on_moduleTracing_on_prop_off", "1", "1", "0", true, false},
		{"all_on", "1", "1", "1", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyFlag(t, "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", tc.global)
			applyFlag(t, "OTEL_NATS_TRACING_ENABLED", tc.moduleTracing)
			applyFlag(t, "OTEL_NATS_PROPAGATION_ENABLED", tc.modulePropagation)
			otelnats.ResetGatesForTest()
			t.Cleanup(otelnats.ResetGatesForTest)

			url := startBareServer(t)

			tp, sr := newTestProvider()
			otel.SetTracerProvider(tp)
			otel.SetTextMapPropagator(propagation.TraceContext{})

			conn, err := otelnats.Connect(url, nil)
			require.NoError(t, err)
			t.Cleanup(conn.Close)

			assert.Equal(t, tc.wantTracing, conn.TracingEnabled(),
				"TracingEnabled() must reflect the two-tier gate")
			assert.Equal(t, tc.wantPropagation, conn.PropagationEnabled(),
				"PropagationEnabled() must reflect the three-tier gate")

			subject := "flagmatrix." + tc.name
			headerCh := make(chan nats.Header, 1)
			_, err = conn.NatsConn().Subscribe(subject, func(msg *nats.Msg) {
				// Copy header to avoid race when sub fires after select.
				h := nats.Header{}
				for k, v := range msg.Header {
					h[k] = append([]string(nil), v...)
				}
				headerCh <- h
			})
			require.NoError(t, err)

			tracer := tp.Tracer("test")
			ctx, parent := tracer.Start(context.Background(), "parent")
			require.NoError(t, conn.Publish(ctx, subject, []byte("payload")))
			parent.End()

			select {
			case h := <-headerCh:
				if tc.wantPropagation {
					assert.NotEmpty(t, h.Get("traceparent"),
						"propagation on: traceparent must be injected")
				} else {
					assert.Empty(t, h.Get("traceparent"),
						"propagation off: NO traceparent — wire byte-identical with native nats.Conn.Publish")
					assert.Empty(t, h.Get("tracestate"),
						"propagation off: NO tracestate")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("subscribe handler did not fire")
			}

			countWrapperSpans := func() int {
				var n int
				for _, s := range sr.Ended() {
					if s.Name() == "parent" {
						continue
					}
					n++
				}
				return n
			}
			if tc.wantTracing {
				require.Eventually(t, func() bool { return countWrapperSpans() >= 1 },
					2*time.Second, 5*time.Millisecond,
					"tracing on: at least one wrapper span must be recorded")
			} else {
				// assert.Never fails on the FIRST observed span — fast-fail
				// instead of waiting a fixed sleep window.
				assert.Never(t, func() bool { return countWrapperSpans() > 0 },
					300*time.Millisecond, 10*time.Millisecond,
					"tracing off: ZERO wrapper spans — must be observationally identical to native nats.Conn.Publish")
			}
		})
	}
}
