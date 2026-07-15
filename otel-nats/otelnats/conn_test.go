package otelnats_test

import (
	"context"
	"strings"
	"testing"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

func newTestProvider() (*trace.TracerProvider, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	return tp, sr
}

func startServer(t *testing.T) string {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "1")
	opts := &natssrv.Options{Host: "127.0.0.1", Port: -1}
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

// waitSpanByNameAndKind polls until the span is in Ended(); subscribe/consume use defer span.End()
// after the handler returns, so reading sr.Ended() right after a done signal races (flaky under -race).
func waitSpanByNameAndKind(t *testing.T, sr *tracetest.SpanRecorder, name string, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	t.Helper()
	var got trace.ReadOnlySpan
	require.Eventually(t, func() bool {
		got = findSpanByNameAndKind(sr.Ended(), name, kind)
		return got != nil
	}, 2*time.Second, 5*time.Millisecond, "wait for ended span %q", name)
	return got
}

func waitSpanByKind(t *testing.T, sr *tracetest.SpanRecorder, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	t.Helper()
	var got trace.ReadOnlySpan
	require.Eventually(t, func() bool {
		got = findSpanByKind(sr.Ended(), kind)
		return got != nil
	}, 2*time.Second, 5*time.Millisecond, "wait for ended span kind %v", kind)
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

func assertIntAttr(t *testing.T, attrs []attribute.KeyValue, key string, want int64) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			assert.Equal(t, want, kv.Value.AsInt64(), "attribute %q", key)
			return
		}
	}
	t.Errorf("attribute %q not found", key)
}

func TestW3CPropagationRoundtrip(t *testing.T) {
	url := startServer(t)
	tp, _ := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	tracer := tp.Tracer("roundtrip-test")
	parentCtx, parentSpan := tracer.Start(context.Background(), "parent")
	defer parentSpan.End()
	wantTraceID := parentSpan.SpanContext().TraceID()

	subject := "rt.test"
	headerCh := make(chan nats.Header, 1)
	_, err = conn.NatsConn().Subscribe(subject, func(msg *nats.Msg) {
		headerCh <- msg.Header
	})
	require.NoError(t, err)

	err = conn.Publish(parentCtx, subject, []byte("ping"))
	require.NoError(t, err)

	var h nats.Header
	select {
	case h = <-headerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	carrier := otelnats.HeaderCarrier{H: h}
	extracted := prop.Extract(context.Background(), carrier)
	gotTraceID := oteltrace.SpanFromContext(extracted).SpanContext().TraceID()
	assert.Equal(t, wantTraceID, gotTraceID)
}

func TestPublishCreatesProducerSpan(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.publish"
	err = conn.Publish(context.Background(), subject, []byte("hello"))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	s := spans[0]
	assert.Equal(t, "send "+subject, s.Name())
	assert.Equal(t, oteltrace.SpanKindProducer, s.SpanKind())
	assertAttr(t, s.Attributes(), "messaging.system", "nats")
	assertAttr(t, s.Attributes(), "messaging.destination.name", subject)
}

func TestPublishMsgCreatesProducerSpan(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.publishmsg"
	msg := &nats.Msg{Subject: subject, Data: []byte("hello msg")}
	err = conn.PublishMsg(context.Background(), msg)
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, oteltrace.SpanKindProducer, spans[0].SpanKind())
}

func TestSubscribeExtractsTraceContext(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	otel.SetTextMapPropagator(prop)
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.subscribe"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		_ = oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID()
		done <- struct{}{}
	})
	require.NoError(t, err)

	tracer := tp.Tracer("publisher")
	pubCtx, pubSpan := tracer.Start(context.Background(), "pub-parent")
	err = conn.Publish(pubCtx, subject, []byte("hello"))
	require.NoError(t, err)
	pubSpan.End()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	consumerSpan := waitSpanByKind(t, sr, oteltrace.SpanKindConsumer)
	spans := sr.Ended()
	producer := findSpanByKind(spans, oteltrace.SpanKindProducer)
	assert.Equal(t, "process "+subject, consumerSpan.Name())
	if producer != nil {
		require.Len(t, consumerSpan.Links(), 1, "consumer span should have 1 link to producer")
		linkCtx := consumerSpan.Links()[0].SpanContext
		assert.Equal(t, producer.SpanContext().TraceID(), linkCtx.TraceID())
		assert.Equal(t, producer.SpanContext().SpanID(), linkCtx.SpanID())
	}
}

func TestQueueSubscribeRecordsQueueName(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject, queue := "test.queue", "workers"
	done := make(chan struct{}, 1)
	_, err = conn.QueueSubscribe(subject, queue, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)
	err = conn.Publish(context.Background(), subject, []byte("work"))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
	// Allow span to be recorded (handled asynchronously in -race builds).
	require.Eventually(t, func() bool {
		return findSpanByKind(sr.Ended(), oteltrace.SpanKindConsumer) != nil
	}, 2*time.Second, 10*time.Millisecond, "no consumer span")
	consumerSpan := findSpanByKind(sr.Ended(), oteltrace.SpanKindConsumer)
	assertAttr(t, consumerSpan.Attributes(), "messaging.consumer.group.name", queue)
}

func TestSubscribeConsumerSpanLinkedToProducer(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	otel.SetTextMapPropagator(prop)
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.linkage"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)
	err = conn.Publish(context.Background(), subject, []byte("link-test"))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	producer := waitSpanByNameAndKind(t, sr, "send "+subject, oteltrace.SpanKindProducer)
	consumer := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	require.Len(t, consumer.Links(), 1, "consumer span should have 1 link to producer")
	linkCtx := consumer.Links()[0].SpanContext
	assert.Equal(t, producer.SpanContext().TraceID(), linkCtx.TraceID())
	assert.Equal(t, producer.SpanContext().SpanID(), linkCtx.SpanID())
}

func TestRequestCreatesClientSpanAndReturnsReply(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "req.reply"
	_, err = conn.NatsConn().Subscribe(subject, func(msg *nats.Msg) {
		_ = msg.Respond([]byte("pong"))
	})
	require.NoError(t, err)

	reply, err := conn.Request(subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(reply.Data))

	spans := sr.Ended()
	requestSpan := findSpanByNameAndKind(spans, subject+" request", oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span for request")

	var receiveSpan trace.ReadOnlySpan
	for _, s := range spans {
		if s.SpanKind() == oteltrace.SpanKindClient && s.Name() != requestSpan.Name() {
			receiveSpan = s
			break
		}
	}
	require.NotNil(t, receiveSpan, "no client span for reply receive")
	assert.True(t, strings.HasPrefix(receiveSpan.Name(), "receive "), "reply receive span name %q", receiveSpan.Name())
}

// TestRequestSpanKeepsRequestBodySize pins that recordReply does not overwrite
// the request span's messaging.message.body.size with the reply size: the send
// span reports the request payload, the receive span reports the reply payload.
func TestRequestSpanKeepsRequestBodySize(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "req.bodysize"
	request := []byte("ping")
	replyPayload := []byte("pong-pong!")
	_, err = conn.NatsConn().Subscribe(subject, func(msg *nats.Msg) {
		_ = msg.Respond(replyPayload)
	})
	require.NoError(t, err)

	reply, err := conn.Request(subject, request, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, replyPayload, reply.Data)

	spans := sr.Ended()
	requestSpan := findSpanByNameAndKind(spans, subject+" request", oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span for request")
	assertIntAttr(t, requestSpan.Attributes(), "messaging.message.body.size", int64(len(request)))

	receiveSpan := findSpanByNameAndKind(spans, "receive "+reply.Subject, oteltrace.SpanKindClient)
	require.NotNil(t, receiveSpan, "no client span for reply receive")
	assertIntAttr(t, receiveSpan.Attributes(), "messaging.message.body.size", int64(len(replyPayload)))
}

func TestTraceContextReturnsTracerAndPropagator(t *testing.T) {
	url := startServer(t)
	tp := trace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	tracer, prop := conn.TraceContext()
	assert.NotNil(t, tracer, "TraceContext() tracer should not be nil")
	assert.NotNil(t, prop, "TraceContext() propagator should not be nil")
}

// TestNoDeliverSpanOnPublishAndConsume asserts the removal of deliver spans:
// a publish + subscribe round trip produces exactly producer + consumer spans,
// with the consumer span linked directly to the producer span.
func TestNoDeliverSpanOnPublishAndConsume(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.nodeliver"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)

	err = conn.Publish(context.Background(), subject, []byte("ping"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	// Allow spans to settle
	require.Eventually(t, func() bool {
		return len(sr.Ended()) >= 2
	}, 2*time.Second, 10*time.Millisecond)

	spans := sr.Ended()
	// Should have exactly 2 spans: producer + consumer (no deliver span)
	require.Len(t, spans, 2, "expected producer + consumer only, no deliver span")
	producer := findSpanByKind(spans, oteltrace.SpanKindProducer)
	consumer := findSpanByKind(spans, oteltrace.SpanKindConsumer)
	require.NotNil(t, producer)
	require.NotNil(t, consumer)
	// Consumer link should point to producer span
	require.Len(t, consumer.Links(), 1)
	assert.Equal(t, producer.SpanContext().SpanID(), consumer.Links()[0].SpanContext.SpanID())
}
