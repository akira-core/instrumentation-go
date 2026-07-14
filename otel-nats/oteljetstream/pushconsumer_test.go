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
	assertAttr(t, consumerSpan.Attributes(), "messaging.consumer.group.name", consumerName)
	producerSpan := findSpanByKind(sr.Ended(), oteltrace.SpanKindProducer)
	require.NotNil(t, producerSpan, "no producer span")
	require.Len(t, consumerSpan.Links(), 1, "consumer span should link the producer")
	assert.Equal(t, producerSpan.SpanContext().TraceID(), consumerSpan.Links()[0].SpanContext.TraceID())
}

// TestOteljetstreamInheritsConnTracingOption verifies a JetStream wrapper
// built from a Conn constructed with WithTracingEnabled(false) selects the
// direct (untraced) impl — via New()'s existing conn.TracingEnabled() check —
// even though the process-wide env gate is on (set by startJetStreamServer,
// already resolved true by prior tests in this binary). This is also the
// first runtime exercise of the direct JetStream path in this package: before
// WithTracingEnabled existed, the process-wide, sync.Once-cached env gate
// made it impossible to construct an untraced connection once any sibling
// test had resolved it to enabled (see the removed NOTE this replaces).
func TestOteljetstreamInheritsConnTracingOption(t *testing.T) {
	url := startJetStreamServer(t) // sets both tracing env vars true as a side effect
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)

	conn, err := otelnats.ConnectWithOptions(url, nil, otelnats.WithTracingEnabled(false))
	require.NoError(t, err)
	defer conn.Close()
	require.False(t, conn.TracingEnabled(), "option must override the truthy env gate")

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "OPTOVERRIDE",
		Subjects: []string{"optoverride.>"},
	})
	require.NoError(t, err)
	cons, err := js.CreateOrUpdateConsumer(ctx, "OPTOVERRIDE", oteljetstream.ConsumerConfig{
		Durable:       "optoverride-consumer",
		FilterSubject: "optoverride.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "optoverride.test", []byte("untraced"))
	require.NoError(t, err)

	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	require.NoError(t, err)
	msg, ok := <-batch.Messages()
	require.True(t, ok, "expected one message")
	assert.Equal(t, "untraced", string(msg.Data()))
	assert.False(t, oteltrace.SpanFromContext(msg.Context()).SpanContext().IsValid(),
		"direct (untraced) path must not attach a trace context")
	_ = msg.Ack()

	assert.Empty(t, sr.Ended(), "no spans should be recorded on a WithTracingEnabled(false) connection")
}

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

	consumerSpan := waitSpanByNameAndKind(t, sr, "receive orderedprefix.test", oteltrace.SpanKindClient)
	assertAttr(t, consumerSpan.Attributes(), "messaging.consumer.group.name", "my-ordered")
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

// TestNextHonorsLiveCancellation verifies Consumer.Next aborts promptly when
// ctx is canceled mid-wait, even with no deadline set — the fetch would
// otherwise block for jetstream's default ~30s max wait.
func TestNextHonorsLiveCancellation(t *testing.T) {
	js, _, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "MIDWAITCANCEL",
		Subjects: []string{"midwaitcancel.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "MIDWAITCANCEL", oteljetstream.ConsumerConfig{
		Durable:       "midwaitcancel-consumer",
		FilterSubject: "midwaitcancel.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	waitCtx, cancel := context.WithCancel(ctx) // no deadline
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, err = cons.Next(waitCtx) // no message ever published — would otherwise block ~30s
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 2*time.Second, "Next must honor live cancellation, not block for the default max wait")
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "Next returned before cancellation fired")
}

// TestNextDeliversMessageBeforeCancellation verifies a message that arrives
// before ctx is canceled is still returned normally, with its receive span
// and returned context intact.
func TestNextDeliversMessageBeforeCancellation(t *testing.T) {
	js, sr, tp := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "NEXTBEFORECANCEL",
		Subjects: []string{"nextbeforecancel.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "NEXTBEFORECANCEL", oteljetstream.ConsumerConfig{
		Durable:       "nextbeforecancel-consumer",
		FilterSubject: "nextbeforecancel.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(ctx, "pub-parent")
	_, err = js.Publish(pubCtx, "nextbeforecancel.test", []byte("in time"))
	pubSpan.End()
	require.NoError(t, err)

	nextCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	msgCtx, msg, err := cons.Next(nextCtx)
	require.NoError(t, err)
	assert.Equal(t, "in time", string(msg.Data()))
	span := oteltrace.SpanFromContext(msgCtx)
	assert.True(t, span.SpanContext().TraceID().IsValid(), "Next should return context with trace")

	receiveSpan := waitSpanByNameAndKind(t, sr, "receive nextbeforecancel.test", oteltrace.SpanKindClient)
	require.NotNil(t, receiveSpan)
}

// TestNextCancelableCtxWithFetchMaxWaitErrors pins the 0.7.0 behavior change:
// a cancelable ctx wires jetstream.FetchContext, which upstream rejects when
// combined with a caller-supplied FetchMaxWait (ErrInvalidOption). Callers
// wanting both use the ctx's own deadline. A non-cancelable ctx
// (context.Background(), Done() == nil) skips the wiring, so FetchMaxWait
// alone keeps working.
func TestNextCancelableCtxWithFetchMaxWaitErrors(t *testing.T) {
	js, _, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "MAXWAITCONFLICT",
		Subjects: []string{"maxwaitconflict.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "MAXWAITCONFLICT", oteljetstream.ConsumerConfig{
		Durable:       "maxwaitconflict-consumer",
		FilterSubject: "maxwaitconflict.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	cancelable, cancel := context.WithCancel(ctx)
	defer cancel()
	_, _, err = cons.Next(cancelable, jetstream.FetchMaxWait(time.Second))
	require.ErrorIs(t, err, jetstream.ErrInvalidOption,
		"cancelable ctx + FetchMaxWait must surface jetstream's native conflict error")

	_, err = js.Publish(ctx, "maxwaitconflict.test", []byte("ok"))
	require.NoError(t, err)
	_, msg, err := cons.Next(ctx, jetstream.FetchMaxWait(2*time.Second))
	require.NoError(t, err, "Background ctx + FetchMaxWait must keep working")
	assert.Equal(t, "ok", string(msg.Data()))
}

// TestNextMethodCtxBeatsCallerFetchContext verifies the method parameter ctx
// stays authoritative when a caller also passes jetstream.FetchContext: the
// wrapper appends its own FetchContext last (fetch options apply in order,
// last write to the request ctx wins), so cancelling the method ctx still
// unblocks Next promptly instead of being shadowed by the caller's context.
func TestNextMethodCtxBeatsCallerFetchContext(t *testing.T) {
	js, _, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "FETCHCTXPRIORITY",
		Subjects: []string{"fetchctxpriority.>"},
	})
	require.NoError(t, err)

	cons, err := js.CreateOrUpdateConsumer(ctx, "FETCHCTXPRIORITY", oteljetstream.ConsumerConfig{
		Durable:       "fetchctxpriority-consumer",
		FilterSubject: "fetchctxpriority.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	waitCtx, cancel := context.WithCancel(ctx) // no deadline
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	otherCtx, otherCancel := context.WithTimeout(ctx, 10*time.Second) // never canceled by the test
	defer otherCancel()

	start := time.Now()
	_, _, err = cons.Next(waitCtx, jetstream.FetchContext(otherCtx)) // no message ever published
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.Canceled,
		"method ctx cancellation must win over a caller-supplied FetchContext")
	assert.Less(t, elapsed, 2*time.Second,
		"Next must return on method-ctx cancellation, not wait out the caller context")
}
