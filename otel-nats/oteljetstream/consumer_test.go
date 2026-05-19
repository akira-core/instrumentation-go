package oteljetstream_test

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
	otelnats "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
)

func startJetStreamServer(t *testing.T) string {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	// Default propagation gate is OFF. Existing tests assume v0.4.x
	// behaviour (header inject + extract on). Opt-in via env set BEFORE
	// the first Connect in this process; the gate caches the value on
	// first read so all subsequent tests in this package share it.
	t.Setenv("OTEL_NATS_PROPAGATION_ENABLED", "1")
	opts := &natssrv.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go s.Start()
	require.True(t, s.ReadyForConnections(5*time.Second), "nats-server not ready")
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

func findSpanByKind(spans []trace.ReadOnlySpan, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	for _, s := range spans {
		if s.SpanKind() == kind {
			return s
		}
	}
	return nil
}

func findSpanByNameAndKind(spans []trace.ReadOnlySpan, name string, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name && s.SpanKind() == kind {
			return s
		}
	}
	return nil
}

// waitSpanByNameAndKind polls until a span is in the recorder's Ended() list.
// Consume callbacks use defer span.End(), so the span is recorded only after the handler returns;
// reading sr.Ended() immediately after a done signal races with that defer (flaky under -race).
func waitSpanByNameAndKind(t *testing.T, sr *tracetest.SpanRecorder, name string, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	t.Helper()
	var got trace.ReadOnlySpan
	require.Eventually(t, func() bool {
		got = findSpanByNameAndKind(sr.Ended(), name, kind)
		return got != nil
	}, 2*time.Second, 5*time.Millisecond, "wait for ended span %q", name)
	return got
}

func assertAttr(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			assert.Equal(t, want, kv.Value.AsString(), "attribute %q", key)
			return
		}
	}
	t.Errorf("attribute %q not found", key)
}

func TestFetchReturnsMessagesWithTraceContext(t *testing.T) {
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	streamName := "FETCHTEST"
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"fetch.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)

	consumerName := "fetch-consumer"
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: "fetch.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	// Publish with trace context
	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(ctx, "pub-parent")
	defer pubSpan.End()
	_, err = js.Publish(pubCtx, "fetch.test", []byte("hello fetch"))
	require.NoError(t, err)

	// Fetch with retries until message is available
	var received int
	var batch oteljetstream.MessageBatch
	for range 30 {
		var ferr error
		batch, ferr = cons.Fetch(5, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, ferr)
		for m := range batch.Messages() {
			received++
			assert.Equal(t, "hello fetch", string(m.Data()))
			span := oteltrace.SpanFromContext(m.Context())
			assert.True(t, span.SpanContext().TraceID().IsValid(), "context should have valid trace ID")
			_ = m.Ack()
		}
		if received == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, received, "expected 1 message after retries")
	if batch != nil {
		require.NoError(t, batch.Error())
	}

	spans := sr.Ended()
	consumerSpan := findSpanByKind(spans, oteltrace.SpanKindConsumer)
	producerSpan := findSpanByKind(spans, oteltrace.SpanKindProducer)
	require.NotNil(t, consumerSpan, "no consumer span")
	assertAttr(t, consumerSpan.Attributes(), "messaging.consumer.name", consumerName)
	if producerSpan != nil && len(consumerSpan.Links()) == 1 {
		linkCtx := consumerSpan.Links()[0].SpanContext
		assert.Equal(t, producerSpan.SpanContext().TraceID(), linkCtx.TraceID())
		assert.Equal(t, producerSpan.SpanContext().SpanID(), linkCtx.SpanID())
	}
}

func TestConsumeTraceContext(t *testing.T) {
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "CONSUMETEST",
		Subjects: []string{"consume.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "CONSUMETEST")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "consume-dup",
		FilterSubject: "consume.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	done := make(chan struct{}, 1)
	cc, err := cons.Consume(func(m oteljetstream.Msg) {
		if oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid() {
			done <- struct{}{}
		}
		_ = m.Ack()
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("pub")
	pubCtx, pubSpan := tracer.Start(ctx, "parent")
	defer pubSpan.End()
	_, _ = js.Publish(pubCtx, "consume.msg", []byte("hi"))
	time.Sleep(300 * time.Millisecond)
	select {
	case <-done:
	default:
		t.Fatal("Consume handler did not receive trace context")
	}
}

func TestMessagesNextTraceContext(t *testing.T) {
	url := startJetStreamServer(t)
	tp := trace.NewTracerProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "MSGTEST",
		Subjects: []string{"msg.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "MSGTEST")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "msg-dup",
		FilterSubject: "msg.one",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	iter, err := cons.Messages()
	require.NoError(t, err)
	defer iter.Stop()

	_, _ = js.Publish(ctx, "msg.one", []byte("data"))
	time.Sleep(300 * time.Millisecond)

	msgCtx, msg, err := iter.Next()
	require.NoError(t, err)
	assert.Equal(t, "data", string(msg.Data()))
	assert.True(t, oteltrace.SpanFromContext(msgCtx).SpanContext().TraceID().IsValid(), "Next should return context with trace")
	_ = msg.Ack()
}

func TestFetchNoWaitReturnsTraceContext(t *testing.T) {
	url := startJetStreamServer(t)
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(tracetest.NewSpanRecorder()))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "NOWAIT",
		Subjects: []string{"nowait.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "NOWAIT")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "nowait-c",
		FilterSubject: "nowait.x",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, _ = js.Publish(ctx, "nowait.x", []byte("v"))
	time.Sleep(200 * time.Millisecond)

	batch, err := cons.FetchNoWait(5)
	require.NoError(t, err)
	n := 0
	for m := range batch.Messages() {
		n++
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(), "context should have trace")
		_ = m.Ack()
	}
	assert.Equal(t, 1, n)
}

func TestFetchBytesTraceContext(t *testing.T) {
	url := startJetStreamServer(t)
	tp := trace.NewTracerProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "BYTESTEST",
		Subjects: []string{"bytes.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "BYTESTEST")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "bytes-c",
		FilterSubject: "bytes.a",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, _ = js.Publish(ctx, "bytes.a", []byte("hello"))
	time.Sleep(200 * time.Millisecond)

	batch, err := cons.FetchBytes(1024, jetstream.FetchMaxWait(5*time.Second))
	require.NoError(t, err)
	for m := range batch.Messages() {
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(), "context should have trace")
		_ = m.Ack()
	}
}

func TestOrderedConsumerTraceContext(t *testing.T) {
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "ORDEREDTEST",
		Subjects: []string{"ordered.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "ORDEREDTEST")
	require.NoError(t, err)

	orderedCons, err := stream.OrderedConsumer(ctx, oteljetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"ordered.msg"},
	})
	require.NoError(t, err)

	done := make(chan struct{}, 1)
	cc, err := orderedCons.Consume(func(m oteljetstream.Msg) {
		if oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid() {
			done <- struct{}{}
		}
		// OrderedConsumer does not require Ack
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("ordered-pub")
	pubCtx, pubSpan := tracer.Start(ctx, "ordered-parent")
	defer pubSpan.End()
	_, err = js.Publish(pubCtx, "ordered.msg", []byte("hello ordered"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OrderedConsumer did not receive trace context")
	}

	consumer := waitSpanByNameAndKind(t, sr, "process ordered.msg", oteltrace.SpanKindConsumer)
	assertAttr(t, consumer.Attributes(), "messaging.consumer.name", "ordered-consumer")
}

func TestConsumerInfo(t *testing.T) {
	url := startJetStreamServer(t)
	otel.SetTracerProvider(trace.NewTracerProvider())
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "CONSINFOTEST",
		Subjects: []string{"consinfo.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "CONSINFOTEST")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "info-cons",
		FilterSubject: "consinfo.x",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	info, err := cons.Info(ctx)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "info-cons", info.Name)
}

func TestJetStreamDeliverSpanConsume(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "DELIVERCONSUME",
		Subjects: []string{"delcons.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "DELIVERCONSUME")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "del-cons",
		FilterSubject: "delcons.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	done := make(chan struct{}, 1)
	cc, err := cons.Consume(func(m oteljetstream.Msg) {
		done <- struct{}{}
		_ = m.Ack()
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("pub")
	pubCtx, pubSpan := tracer.Start(ctx, "parent")
	_, err = js.Publish(pubCtx, "delcons.msg", []byte("hi"))
	require.NoError(t, err)
	pubSpan.End()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	require.Eventually(t, func() bool {
		return findSpanByKind(sr.Ended(), oteltrace.SpanKindConsumer) != nil
	}, 2*time.Second, 10*time.Millisecond)

	spans := sr.Ended()
	producer := findSpanByNameAndKind(spans, "send "+"delcons.msg", oteltrace.SpanKindProducer)
	consumer := findSpanByNameAndKind(spans, "process "+"delcons.msg", oteltrace.SpanKindConsumer)
	require.NotNil(t, producer)
	require.NotNil(t, consumer)
	require.Len(t, consumer.Links(), 1)
	linkSpanID := consumer.Links()[0].SpanContext.SpanID()
	assert.NotEqual(t, producer.SpanContext().SpanID(), linkSpanID,
		"consumer link should point to deliver span, not producer")
	assert.Equal(t, producer.SpanContext().TraceID(), consumer.Links()[0].SpanContext.TraceID(),
		"deliver span should share traceID with producer")
}

func TestJetStreamDeliverSpanFetch(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "DELIVERFETCH",
		Subjects: []string{"delfetch.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "DELIVERFETCH")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "del-fetch",
		FilterSubject: "delfetch.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("pub")
	pubCtx, pubSpan := tracer.Start(ctx, "parent")
	_, err = js.Publish(pubCtx, "delfetch.msg", []byte("hello"))
	require.NoError(t, err)
	pubSpan.End()

	var received int
	for range 30 {
		batch, ferr := cons.Fetch(5, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, ferr)
		for m := range batch.Messages() {
			received++
			_ = m.Ack()
		}
		if received == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, received)

	spans := sr.Ended()
	producer := findSpanByNameAndKind(spans, "send "+"delfetch.msg", oteltrace.SpanKindProducer)
	consumer := findSpanByNameAndKind(spans, "receive "+"delfetch.msg", oteltrace.SpanKindConsumer)
	require.NotNil(t, producer)
	require.NotNil(t, consumer)
	require.Len(t, consumer.Links(), 1)
	linkSpanID := consumer.Links()[0].SpanContext.SpanID()
	assert.NotEqual(t, producer.SpanContext().SpanID(), linkSpanID,
		"consumer link should point to deliver span, not producer")
	assert.Equal(t, producer.SpanContext().TraceID(), consumer.Links()[0].SpanContext.TraceID(),
		"deliver span should share traceID with producer")
}

func TestConsumerCachedInfo(t *testing.T) {
	url := startJetStreamServer(t)
	otel.SetTracerProvider(trace.NewTracerProvider())
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "CACHEDCONSINFO",
		Subjects: []string{"cachedcons.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "CACHEDCONSINFO")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "cached-cons",
		FilterSubject: "cachedcons.x",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)
	_, _ = cons.Info(ctx) // populate cache

	cached := cons.CachedInfo()
	require.NotNil(t, cached)
	require.Equal(t, "cached-cons", cached.Name)
}

func TestJetStreamConsumerManagerConsumerKeepsTraceWrapper(t *testing.T) {
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "JSMANAGERTRACE",
		Subjects: []string{"jsmanager.>"},
	})
	require.NoError(t, err)

	_, err = js.CreateOrUpdateConsumer(ctx, "JSMANAGERTRACE", oteljetstream.ConsumerConfig{
		Durable:       "js-manager-consumer",
		FilterSubject: "jsmanager.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	cons, err := js.Consumer(ctx, "JSMANAGERTRACE", "js-manager-consumer")
	require.NoError(t, err)

	done := make(chan struct{}, 1)
	cc, err := cons.Consume(func(m oteljetstream.Msg) {
		if oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid() {
			done <- struct{}{}
		}
		_ = m.Ack()
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("pub-js-manager")
	pubCtx, pubSpan := tracer.Start(ctx, "js-manager-parent")
	defer pubSpan.End()
	_, err = js.Publish(pubCtx, "jsmanager.msg", []byte("hello"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("JetStream.Consumer returned consumer did not carry trace context")
	}
}

// for batch fetches. Returns the consumer plus a publish helper.
func messageBatchFixture(t *testing.T) (oteljetstream.Consumer, func(payload string), func()) {
	t.Helper()
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	streamName := "BATCHLIFECYCLE"
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"batchlifecycle.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "batchlifecycle-cons",
		FilterSubject: "batchlifecycle.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	pub := func(payload string) {
		_, perr := js.Publish(ctx, "batchlifecycle.test", []byte(payload))
		require.NoError(t, perr)
	}
	cleanup := func() {
		conn.Close()
	}
	t.Cleanup(cleanup)
	return cons, pub, cleanup
}

// TestMessageBatchStopIdempotent verifies Stop() is safe to call multiple
// times without panic (sync.Once invariant).
func TestMessageBatchStopIdempotent(t *testing.T) {
	cons, pub, _ := messageBatchFixture(t)
	pub("p1")
	pub("p2")
	pub("p3")

	var batch oteljetstream.MessageBatch
	for range 30 {
		b, err := cons.Fetch(3, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, err)
		// drain at least one to confirm batch is alive, then stop early
		select {
		case _, ok := <-b.Messages():
			require.True(t, ok)
			batch = b
		case <-time.After(500 * time.Millisecond):
		}
		if batch != nil {
			break
		}
	}
	require.NotNil(t, batch, "expected at least one message available")

	// Call Stop() three times — must not panic.
	batch.Stop()
	batch.Stop()
	batch.Stop()
}

// TestMessageBatchStopReleasesRawBatch verifies the drain invariant:
// after Stop() the wrapper goroutine drains the raw chan so the upstream
// jetstream driver can complete. Detection: goroutine count returns to
// baseline within a reasonable window.
func TestMessageBatchStopReleasesRawBatch(t *testing.T) {
	cons, pub, _ := messageBatchFixture(t)
	// publish enough messages so the raw batch chan still has undelivered
	// items when we Stop().
	for range 10 {
		pub("p")
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	for range 5 {
		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, err)
		// Consume only ONE message then Stop — leaves the raw chan with
		// pending items. Without the drain fix the upstream goroutine
		// blocks on chan send.
		select {
		case m, ok := <-batch.Messages():
			if ok {
				_ = m.Ack()
			}
		case <-time.After(500 * time.Millisecond):
		}
		batch.Stop()
		// Republish so next round has something to fetch.
		for range 3 {
			pub("p")
		}
	}

	// Give wrapper goroutines time to exit + GC.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()
	delta := after - before
	// Allow modest slack for the embedded NATS server housekeeping.
	assert.Less(t, delta, 30,
		"goroutine count grew by %d after 5 early-stop rounds (likely leak in raw batch goroutine)", delta)
}

// TestNoGoroutineLeakAfterEarlyReturn confirms the wrapper goroutine inside
// newTracedMessageBatch exits after Stop() even when caller did not consume
// any message. Exercises the done-branch drain path.
func TestNoGoroutineLeakAfterEarlyReturn(t *testing.T) {
	cons, pub, _ := messageBatchFixture(t)
	for range 5 {
		pub("p")
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	for range 10 {
		batch, err := cons.Fetch(5, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, err)
		// Immediately Stop without consuming.
		batch.Stop()
		for range 2 {
			pub("p")
		}
	}

	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()
	delta := after - before
	assert.Less(t, delta, 30,
		"goroutine count grew by %d after 10 stop-without-consume rounds", delta)
}

// ─── Propagation-flag matrix for JetStream consumer paths (task 13.9) ────────
//
// Mirrors the core-NATS matrix in otel-nats/otelnats/conn_test.go
// (TestPublishWithPropagationOff/On*, TestSubscribeWith*). Asserts that the
// JetStream consumer paths (newTracedMessageBatch backing
// `consumer.Fetch().Messages()` + `consumer.Consume()`) honour
// `OTEL_NATS_PROPAGATION_ENABLED` identically to the core path.

// startJetStreamServerWithPropagation re-runs the normal start with the
// propagation gate overridden. Use sparingly — caller MUST call ResetGatesForTest
// via t.Cleanup which the otelnats package already wires through this helper.
func startJetStreamServerWithPropagation(t *testing.T, propagationValue string) string {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_PROPAGATION_ENABLED", propagationValue)
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)
	opts := &natssrv.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go s.Start()
	require.True(t, s.ReadyForConnections(5*time.Second), "nats-server not ready")
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

// startJetStreamServerTracingOff brings up a JetStream server with every
// tracing env var unset/falsy — the wrapper must behave as raw jetstream.
func startJetStreamServerTracingOff(t *testing.T) string {
	t.Helper()
	_ = os.Unsetenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_NATS_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_NATS_PROPAGATION_ENABLED")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)
	opts := &natssrv.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go s.Start()
	require.True(t, s.ReadyForConnections(5*time.Second), "nats-server not ready")
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

//nolint:nakedret // multi-return helper; named results make assertions in callers explicit
func jetstreamFetchOne(t *testing.T, url string) (gotTraceID oteltrace.TraceID, gotHeader map[string]string, consumerSpans []trace.ReadOnlySpan, producerSpan trace.ReadOnlySpan) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name: "PROPTEST", Subjects: []string{"prop.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "PROPTEST")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "prop-cons",
		FilterSubject: "prop.test",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(ctx, "pub-parent")
	defer pubSpan.End()
	_, err = js.Publish(pubCtx, "prop.test", []byte("payload"))
	require.NoError(t, err)

	for range 30 {
		batch, ferr := cons.Fetch(1, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, ferr)
		for m := range batch.Messages() {
			gotTraceID = oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID()
			hdr := m.Headers()
			gotHeader = map[string]string{}
			if hdr != nil {
				if tpV := hdr.Get("traceparent"); tpV != "" {
					gotHeader["traceparent"] = tpV
				}
				if tsV := hdr.Get("tracestate"); tsV != "" {
					gotHeader["tracestate"] = tsV
				}
			}
			_ = m.Ack()
		}
		if gotHeader != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Settle: spans End on defer after handler returns.
	time.Sleep(150 * time.Millisecond)
	all := sr.Ended()
	producerSpan = findSpanByKind(all, oteltrace.SpanKindProducer)
	for _, s := range all {
		if s.SpanKind() == oteltrace.SpanKindConsumer {
			consumerSpans = append(consumerSpans, s)
		}
	}
	return
}

// TestJetStreamSubscribeWithPropagationOffDoesNotExtract mirrors 13.7 for
// JetStream consumer (Fetch path). With propagation off:
//   - no traceparent header lands on the wire (producer side does not inject)
//   - the consumer span SHALL carry zero links (no Extract performed)
//
// The consumer span always starts a fresh trace (otel-nats uses link-mode
// propagation, not parent-child), so trace_id inequality is not a useful
// signal — link-count is the structural signal.
func TestJetStreamSubscribeWithPropagationOffDoesNotExtract(t *testing.T) {
	url := startJetStreamServerWithPropagation(t, "false")
	_, gotHeader, consumerSpans, producerSpan := jetstreamFetchOne(t, url)

	require.NotNil(t, producerSpan, "producer span must still be emitted")
	require.NotEmpty(t, consumerSpans, "consumer span must still be emitted with propagation off")

	assert.Empty(t, gotHeader["traceparent"],
		"propagation off: traceparent header MUST NOT be on the wire")

	for _, c := range consumerSpans {
		assert.Empty(t, c.Links(),
			"propagation off: consumer span MUST carry zero header-derived links")
	}
}

// TestJetStreamSubscribeWithPropagationOnExtractsRemoteContext mirrors 13.8 for
// JetStream consumer. With propagation on:
//   - traceparent header IS injected on publish
//   - consumer span carries one link whose SpanContext matches the publish
//     wrapper span's trace_id (otel-nats uses link-mode propagation)
func TestJetStreamSubscribeWithPropagationOnExtractsRemoteContext(t *testing.T) {
	url := startJetStreamServerWithPropagation(t, "true")
	_, gotHeader, consumerSpans, producerSpan := jetstreamFetchOne(t, url)

	require.NotNil(t, producerSpan)
	require.NotEmpty(t, consumerSpans)
	require.NotNil(t, gotHeader)
	assert.NotEmpty(t, gotHeader["traceparent"],
		"propagation on: traceparent header MUST be injected on the wire")

	pubTraceID := producerSpan.SpanContext().TraceID()
	var linked bool
	for _, c := range consumerSpans {
		for _, ln := range c.Links() {
			if ln.SpanContext.TraceID() == pubTraceID {
				linked = true
				break
			}
		}
		if linked {
			break
		}
	}
	assert.True(t, linked,
		"propagation on: consumer span MUST carry a link to the producer's trace_id")
}

// TestJetStreamConsumerWithTracingOffSkipsBoth mirrors 13.4 for JetStream:
// every gate off ⇒ no wrapper spans AND no traceparent header on the wire.
func TestJetStreamConsumerWithTracingOffSkipsBoth(t *testing.T) {
	url := startJetStreamServerTracingOff(t)
	gotTraceID, gotHeader, consumerSpans, producerSpan := jetstreamFetchOne(t, url)

	// Producer span is the test's own "pub-parent" — that's allowed; it's
	// not a wrapper span. Filter to wrapper-emitted producer spans by name.
	if producerSpan != nil && producerSpan.Name() != "pub-parent" {
		t.Errorf("unexpected wrapper PRODUCER span in tracing-off mode: %q", producerSpan.Name())
	}
	for _, c := range consumerSpans {
		t.Errorf("unexpected wrapper CONSUMER span in tracing-off mode: %q", c.Name())
	}

	assert.Empty(t, gotHeader["traceparent"],
		"tracing off: no traceparent on the wire")
	// The fetched message ctx carries the inherited test ctx (which had no
	// span) so the trace_id is the zero value.
	assert.Equal(t, "00000000000000000000000000000000", gotTraceID.String(),
		"tracing off: consumer ctx must not carry an extracted trace_id")
}

// TestJetStreamConsumerWithTracingOffPropagationOnStillSkipsBoth mirrors 13.5
// for JetStream: tracing gate is the hard prerequisite — propagation-only
// truthy with tracing off SHALL behave identically to "both off".
func TestJetStreamConsumerWithTracingOffPropagationOnStillSkipsBoth(t *testing.T) {
	_ = os.Unsetenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")
	_ = os.Unsetenv("OTEL_NATS_TRACING_ENABLED")
	t.Setenv("OTEL_NATS_PROPAGATION_ENABLED", "true")
	otelnats.ResetGatesForTest()
	t.Cleanup(otelnats.ResetGatesForTest)

	opts := &natssrv.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
	}
	s, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go s.Start()
	require.True(t, s.ReadyForConnections(5*time.Second), "nats-server not ready")
	t.Cleanup(s.Shutdown)

	gotTraceID, gotHeader, consumerSpans, producerSpan := jetstreamFetchOne(t, s.ClientURL())

	if producerSpan != nil && producerSpan.Name() != "pub-parent" {
		t.Errorf("propagation on but tracing off: unexpected wrapper PRODUCER span %q", producerSpan.Name())
	}
	for _, c := range consumerSpans {
		t.Errorf("propagation on but tracing off: unexpected wrapper CONSUMER span %q", c.Name())
	}
	assert.Empty(t, gotHeader["traceparent"],
		"propagation env on but tracing off: still no traceparent (hard prerequisite)")
	assert.Equal(t, "00000000000000000000000000000000", gotTraceID.String())
}
