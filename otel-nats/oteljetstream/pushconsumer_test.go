package oteljetstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// setupTracedJS boots a JetStream server, wires a recording TracerProvider, and
// returns a traced JetStream wrapper plus the recorder.
func setupTracedJS(t *testing.T) (oteljetstream.JetStream, *tracetest.SpanRecorder, *trace.TracerProvider) {
	t.Helper()
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	return js, sr, tp
}

func TestPushConsumerConsumeTraceContext(t *testing.T) {
	js, sr, tp := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "PUSHTEST",
		Subjects: []string{"push.>"},
	})
	require.NoError(t, err)

	consumerName := "push-consumer"
	pc, err := js.CreateOrUpdatePushConsumer(ctx, "PUSHTEST", oteljetstream.ConsumerConfig{
		Durable:        consumerName,
		FilterSubject:  "push.test",
		AckPolicy:      oteljetstream.AckExplicitPolicy,
		DeliverSubject: "deliver.push", // outside the stream's push.> subjects — same-stream deliver subject forms a cycle
	})
	require.NoError(t, err)

	done := make(chan oteljetstream.Msg, 1)
	cc, err := pc.Consume(func(m oteljetstream.Msg) {
		_ = m.Ack()
		done <- m
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(ctx, "pub-parent")
	_, err = js.Publish(pubCtx, "push.test", []byte("hello push"))
	pubSpan.End()
	require.NoError(t, err)

	var msg oteljetstream.Msg
	select {
	case msg = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push consumer did not deliver message")
	}
	assert.Equal(t, "hello push", string(msg.Data()))
	span := oteltrace.SpanFromContext(msg.Context())
	assert.True(t, span.SpanContext().TraceID().IsValid(), "handler context should carry valid trace")

	consumerSpan := waitSpanByNameAndKind(t, sr, "process push.test", oteltrace.SpanKindConsumer)
	assertAttr(t, consumerSpan.Attributes(), "messaging.consumer.name", consumerName)
	producerSpan := findSpanByKind(sr.Ended(), oteltrace.SpanKindProducer)
	require.NotNil(t, producerSpan, "no producer span")
	require.Len(t, consumerSpan.Links(), 1, "consumer span should link the producer")
	assert.Equal(t, producerSpan.SpanContext().TraceID(), consumerSpan.Links()[0].SpanContext.TraceID())
}

// NOTE: the direct (tracing-off) push-consumer path has no runtime test here —
// the otelnats gate is cached process-wide (flags.Gate, reset helper is
// otelnats-package-private), so a jetstream external test cannot flip it after
// sibling tests resolve it to enabled. directPushConsumer/directJSImpl parity
// is compile-enforced at New()'s return sites; no direct-path runtime tests
// exist in this package for the pull variants either (same constraint).

func TestMessagesContextNextOpts(t *testing.T) {
	js, _, tp := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "NEXTOPTS",
		Subjects: []string{"nextopts.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "NEXTOPTS", oteljetstream.ConsumerConfig{
		Durable:       "nextopts-consumer",
		FilterSubject: "nextopts.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	iter, err := cons.Messages()
	require.NoError(t, err)
	defer iter.Stop()

	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(ctx, "pub-parent")
	_, err = js.Publish(pubCtx, "nextopts.test", []byte("opts msg"))
	pubSpan.End()
	require.NoError(t, err)

	// NextOpt passthrough: a generous max-wait must still deliver the message
	// with the extracted trace context.
	msgCtx, msg, err := iter.Next(jetstream.NextMaxWait(5 * time.Second))
	require.NoError(t, err)
	assert.Equal(t, "opts msg", string(msg.Data()))
	require.NoError(t, msg.Ack())
	span := oteltrace.SpanFromContext(msgCtx)
	assert.True(t, span.SpanContext().TraceID().IsValid())

	// And a short max-wait on a drained consumer must time out instead of
	// blocking indefinitely — proving the option reaches the underlying iterator.
	start := time.Now()
	_, _, err = iter.Next(jetstream.NextMaxWait(250 * time.Millisecond))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second, "NextMaxWait was not honored")
}

func TestUnwrapEscapeHatch(t *testing.T) {
	js, _, _ := setupTracedJS(t)
	ctx := context.Background()

	raw := js.Unwrap()
	require.NotNil(t, raw)
	// Raw handle reaches upstream APIs the wrapper does not re-expose.
	_, err := raw.AccountInfo(ctx)
	require.NoError(t, err)

	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "UNWRAP",
		Subjects: []string{"unwrap.>"},
	})
	require.NoError(t, err)

	s, err := js.Stream(ctx, "UNWRAP")
	require.NoError(t, err)
	// Stream fully mirrors jetstream.Stream — reach info directly, no Unwrap needed.
	assert.Equal(t, "UNWRAP", s.CachedInfo().Config.Name)

	// ConsumeContext fully mirrors jetstream.ConsumeContext — no Unwrap needed.
	cons, err := js.CreateOrUpdateConsumer(ctx, "UNWRAP", oteljetstream.ConsumerConfig{
		Durable:       "unwrap-consumer",
		FilterSubject: "unwrap.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)
	cc, err := cons.Consume(func(m oteljetstream.Msg) { _ = m.Ack() })
	require.NoError(t, err)
	require.NotNil(t, cc.Closed(), "ConsumeContext.Closed must return the completion channel")
	// Drain triggers a graceful shutdown; Closed fires once it completes.
	cc.Drain()
	select {
	case <-cc.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("ConsumeContext.Closed did not fire after Drain")
	}
}

// TestConsumeNilHandlerRejected verifies the wrappers pass a nil handler through
// to jetstream (rather than a non-nil closure), so upstream returns its
// ErrHandlerRequired instead of panicking later in the delivery goroutine.
func TestConsumeNilHandlerRejected(t *testing.T) {
	js, _, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "NILHANDLER",
		Subjects: []string{"nilhandler.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "NILHANDLER", oteljetstream.ConsumerConfig{
		Durable:       "nil-consumer",
		FilterSubject: "nilhandler.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	cc, err := cons.Consume(nil)
	require.Error(t, err, "Consume(nil) must surface upstream ErrHandlerRequired, not panic")
	assert.Nil(t, cc)
}

func TestOrderedConsumerNamePrefixAttr(t *testing.T) {
	js, sr, tp := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "ORDEREDPREFIX",
		Subjects: []string{"orderedprefix.>"},
	})
	require.NoError(t, err)

	cons, err := js.OrderedConsumer(ctx, "ORDEREDPREFIX", oteljetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"orderedprefix.test"},
		NamePrefix:     "my-ordered",
	})
	require.NoError(t, err)

	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(ctx, "pub-parent")
	_, err = js.Publish(pubCtx, "orderedprefix.test", []byte("ordered msg"))
	pubSpan.End()
	require.NoError(t, err)

	_, msg, err := cons.Next(ctx, jetstream.FetchMaxWait(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, "ordered msg", string(msg.Data()))

	consumerSpan := waitSpanByNameAndKind(t, sr, "receive orderedprefix.test", oteltrace.SpanKindConsumer)
	assertAttr(t, consumerSpan.Attributes(), "messaging.consumer.name", "my-ordered")
}

// TestNextFailsFastOnDoneContext verifies Consumer.Next honors an
// already-canceled or already-expired context instead of blocking for
// jetstream's default max wait (~30s).
func TestNextFailsFastOnDoneContext(t *testing.T) {
	js, _, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "FAILFAST",
		Subjects: []string{"failfast.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "FAILFAST", oteljetstream.ConsumerConfig{
		Durable:       "failfast-consumer",
		FilterSubject: "failfast.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	start := time.Now()
	_, _, err = cons.Next(canceledCtx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 2*time.Second, "Next must fail fast on canceled ctx")

	expiredCtx, cancel2 := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel2()
	start = time.Now()
	_, _, err = cons.Next(expiredCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "Next must fail fast on expired ctx")
}
