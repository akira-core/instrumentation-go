package oteljetstream_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// A JetStream handle is created once at application startup and kept for the
// process lifetime, so the tracing flag must be resolved per call rather than
// captured when New returns. This test proves that end to end: spans stop and
// start on a handle that is never recreated.
//
// Nothing is cached and there is no reset hook: a rebound provider is observed
// on the very next operation, so these tests flip the relay and assert directly.
//
// Every test binds the provider BEFORE connecting. relayPossible is resolved at
// construction, so a Conn built while no relay could exist resolves statically
// for its whole life.
//
// Not parallel-safe: the OpenFeature provider and the process environment are
// global. No t.Parallel in this file.

const jsTracingFlagKey = "otel-nats-tracing"

// relayFlag builds an in-memory OpenFeature boolean flag with on/off variants.
// Duplicated per module on purpose — see the note in otelnats' boolFlag.
func relayFlag(v bool) memprovider.InMemoryFlag {
	variant := "off"
	if v {
		variant = "on"
	}
	return memprovider.InMemoryFlag{
		State:          memprovider.Enabled,
		DefaultVariant: variant,
		Variants:       map[string]any{"on": true, "off": false},
	}
}

// clearEnv removes a switch for the duration of the test.
func clearEnv(t *testing.T, name string) {
	t.Helper()
	prev, existed := os.LookupEnv(name)
	_ = os.Unsetenv(name)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, prev)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func setJSRelay(t *testing.T, tracing bool) {
	t.Helper()
	// Bind the NAMED domain: that is what the resolver reads, and a default
	// provider would be shadowed by any named binding left behind elsewhere in
	// this binary.
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
		memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{jsTracingFlagKey: relayFlag(tracing)},
		)))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})
}

func TestJetStreamPublishFollowsTheRelayWithoutRecreatingTheHandle(t *testing.T) {
	url := startJetStreamServer(t)

	// Both environment tiers on: every observation below is
	// attributable to the relay rather than to the environment.
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	setJSRelay(t, false)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     "DYNFLAGS",
		Subjects: []string{"dynflags.>"},
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "dynflags.a", []byte("one"))
	require.NoError(t, err)
	assert.Empty(t, publishSpans(sr), "relay says off → no publish span")

	// Operator flips the flag. Same conn, same js handle.
	setJSRelay(t, true)
	_, err = js.Publish(ctx, "dynflags.b", []byte("two"))
	require.NoError(t, err)
	assert.NotEmpty(t, publishSpans(sr),
		"relay flipped on → the same JetStream handle now emits publish spans")

	before := len(publishSpans(sr))
	setJSRelay(t, false)
	_, err = js.Publish(ctx, "dynflags.c", []byte("three"))
	require.NoError(t, err)
	assert.Len(t, publishSpans(sr), before,
		"relay flipped back off → no further publish spans")
}

// TestMasterVetoStopsEveryJetStreamWrapper: the master switch is ANDed above
// the whole ladder, so a JetStream wrapper derived from a Conn stops with it —
// even though the module's own key and its environment variable both say on.
func TestMasterVetoStopsEveryJetStreamWrapper(t *testing.T) {
	url := startJetStreamServer(t)
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "false")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "true")
	setJSRelay(t, true)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.ConnectWithOptions(url, nil, otelnats.WithTracingEnabled(true))
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     "VETOED",
		Subjects: []string{"vetoed.>"},
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "vetoed.a", []byte("one"))
	require.NoError(t, err)

	assert.Empty(t, publishSpans(sr),
		"the master veto must stop every wrapper derived from the Conn, whatever enabled the module")
}

// TestEnvironmentBeatsOptionForDerivedWrappers pins the ordering change at the
// level a JetStream user sees: a deployment can disable the module even though
// the application's Go code passed WithTracingEnabled(true).
func TestEnvironmentBeatsOptionForDerivedWrappers(t *testing.T) {
	url := startJetStreamServer(t)
	clearEnv(t, "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "false")
	// A provider exists but defines no key, so the relay is silent and the local
	// ladder decides.
	require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain,
		memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{})))
	t.Cleanup(func() {
		require.NoError(t, openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, openfeature.NoopProvider{}))
	})

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.ConnectWithOptions(url, nil, otelnats.WithTracingEnabled(true))
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     "ENVWINS",
		Subjects: []string{"envwins.>"},
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "envwins.a", []byte("one"))
	require.NoError(t, err)

	assert.Empty(t, publishSpans(sr),
		"OTEL_NATS_TRACING_ENABLED=false must beat WithTracingEnabled(true) for derived wrappers too")
}

// publishSpans returns the recorded spans produced by JetStream publishes.
func publishSpans(sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		for _, a := range s.Attributes() {
			if string(a.Key) == "messaging.operation.name" && a.Value.AsString() == "publish" {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// TestConsumeFollowsTheRelayWithoutResubscribing pins the C4 regression: a
// Consume callback runs for the process lifetime, so the flag must be resolved
// per delivered message, not once when Consume registered the handler.
func TestConsumeFollowsTheRelayWithoutResubscribing(t *testing.T) {
	url := startJetStreamServer(t)
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	setJSRelay(t, false)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: "DYNCONSUME", Subjects: []string{"dynconsume.>"},
	})
	require.NoError(t, err)
	cons, err := js.CreateOrUpdateConsumer(ctx, "DYNCONSUME", jetstream.ConsumerConfig{
		Durable: "dyn", AckPolicy: jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	delivered := make(chan struct{}, 16)
	cc, err := cons.Consume(func(m oteljetstream.Msg) {
		_ = m.Ack()
		delivered <- struct{}{}
	})
	require.NoError(t, err)
	defer cc.Stop()

	awaitDelivery := func() {
		t.Helper()
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatal("message never delivered")
		}
	}
	processSpans := func() int {
		n := 0
		for _, s := range sr.Ended() {
			if strings.HasPrefix(s.Name(), "process ") {
				n++
			}
		}
		return n
	}

	_, err = js.Publish(ctx, "dynconsume.a", []byte("one"))
	require.NoError(t, err)
	awaitDelivery()
	assert.Zero(t, processSpans(), "relay off → the running Consume loop emits no process span")

	setJSRelay(t, true)
	_, err = js.Publish(ctx, "dynconsume.b", []byte("two"))
	require.NoError(t, err)
	awaitDelivery()
	// The process span ends AFTER the user handler signals delivery (defer
	// inside the consume closure), so poll instead of asserting immediately.
	assert.Eventually(t, func() bool { return processSpans() > 0 }, 3*time.Second, 20*time.Millisecond,
		"relay on → the SAME Consume loop emits process spans without resubscribing")
}

// TestMessagesIteratorFollowsTheRelayMidStream pins the C5 regression: the
// MessagesContext iterator resolves the flag per Next, not at Messages() time.
func TestMessagesIteratorFollowsTheRelayMidStream(t *testing.T) {
	url := startJetStreamServer(t)
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	setJSRelay(t, false)

	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: "DYNMSGS", Subjects: []string{"dynmsgs.>"},
	})
	require.NoError(t, err)
	cons, err := js.CreateOrUpdateConsumer(ctx, "DYNMSGS", jetstream.ConsumerConfig{
		Durable: "dyniter", AckPolicy: jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	iter, err := cons.Messages()
	require.NoError(t, err)
	defer iter.Stop()

	receiveSpans := func() int {
		n := 0
		for _, s := range sr.Ended() {
			if strings.HasPrefix(s.Name(), "receive ") {
				n++
			}
		}
		return n
	}

	// The iterator is created while the relay says off; first message must not trace.
	_, err = js.Publish(ctx, "dynmsgs.a", []byte("one"))
	require.NoError(t, err)
	_, msg, err := iter.Next()
	require.NoError(t, err)
	require.NoError(t, msg.Ack())
	assert.Zero(t, receiveSpans(), "relay off → Next emits no receive span")

	// Flip on; the SAME iterator must start tracing.
	setJSRelay(t, true)
	_, err = js.Publish(ctx, "dynmsgs.b", []byte("two"))
	require.NoError(t, err)
	_, msg, err = iter.Next()
	require.NoError(t, err)
	require.NoError(t, msg.Ack())
	assert.Positive(t, receiveSpans(),
		"relay on → the SAME iterator emits receive spans without being recreated")
}
