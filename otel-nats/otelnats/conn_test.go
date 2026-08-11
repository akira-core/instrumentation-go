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
	"go.opentelemetry.io/otel/codes"
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

func assertBoolAttr(t *testing.T, attrs []attribute.KeyValue, key string, want bool) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			assert.Equal(t, want, kv.Value.AsBool(), "attribute %q", key)
			return
		}
	}
	t.Errorf("attribute %q not found", key)
}

func assertNoAttr(t *testing.T, attrs []attribute.KeyValue, key string) {
	t.Helper()
	for _, kv := range attrs {
		assert.NotEqual(t, key, string(kv.Key), "attribute %q should be absent", key)
	}
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
	assert.Equal(t, "publish "+subject, s.Name())
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

	producer := waitSpanByNameAndKind(t, sr, "publish "+subject, oteltrace.SpanKindProducer)
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
	requestSpan := findSpanByNameAndKind(spans, "request "+subject, oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span for request")

	var receiveSpan trace.ReadOnlySpan
	for _, s := range spans {
		if s.SpanKind() == oteltrace.SpanKindClient && s.Name() != requestSpan.Name() {
			receiveSpan = s
			break
		}
	}
	require.NotNil(t, receiveSpan, "no client span for reply receive")
	assert.Equal(t, "receive", receiveSpan.Name())
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
	requestSpan := findSpanByNameAndKind(spans, "request "+subject, oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span for request")
	assertIntAttr(t, requestSpan.Attributes(), "messaging.message.body.size", int64(len(request)))

	receiveSpan := findSpanByNameAndKind(spans, "receive", oteltrace.SpanKindClient)
	require.NotNil(t, receiveSpan, "no client span for reply receive")
	assertIntAttr(t, receiveSpan.Attributes(), "messaging.message.body.size", int64(len(replyPayload)))
}

// TestRequestReplySpansShareConversationID pins F6: the request "send" span,
// the reply-"receive" span, and the responder's "process" span all carry the
// same messaging.message.conversation_id (the reply inbox subject).
func TestRequestReplySpansShareConversationID(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "req.convid"
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		_ = m.Msg.Respond([]byte("pong"))
	})
	require.NoError(t, err)

	reply, err := conn.Request(subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "pong", string(reply.Data))

	inbox := reply.Subject
	require.True(t, strings.HasPrefix(inbox, "_INBOX."), "reply subject %q should be an inbox", inbox)

	spans := sr.Ended()
	requestSpan := findSpanByNameAndKind(spans, "request "+subject, oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span for request")
	assertAttr(t, requestSpan.Attributes(), "messaging.message.conversation_id", inbox)

	receiveSpan := waitSpanByNameAndKind(t, sr, "receive", oteltrace.SpanKindClient)
	assertAttr(t, receiveSpan.Attributes(), "messaging.message.conversation_id", inbox)

	processSpan := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	assertAttr(t, processSpan.Attributes(), "messaging.message.conversation_id", inbox)
}

// TestFailedRequestOmitsConversationID pins that a request with no responder
// never observes the inbox, so no conversation_id is set on the send span.
func TestFailedRequestOmitsConversationID(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "req.noresponder"
	_, err = conn.Request(subject, []byte("ping"), 200*time.Millisecond)
	require.Error(t, err)

	spans := sr.Ended()
	requestSpan := findSpanByNameAndKind(spans, "request "+subject, oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span for request")
	assert.Equal(t, codes.Error, requestSpan.Status().Code, "failed request should record error status")
	for _, kv := range requestSpan.Attributes() {
		assert.NotEqual(t, "messaging.message.conversation_id", string(kv.Key), "conversation_id should be absent on a failed request")
	}
}

// TestPublishMsgWithReplyCarriesConversationID pins the span-start path: a
// manual request/reply via PublishMsg with an explicit caller-chosen Reply
// carries conversation_id from span start (publishAttrs' msg.Reply clause).
func TestPublishMsgWithReplyCarriesConversationID(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.manualreq"
	replyInbox := nats.NewInbox()
	msg := &nats.Msg{Subject: subject, Data: []byte("manual"), Reply: replyInbox}
	err = conn.PublishMsg(context.Background(), msg)
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	producer := spans[0]
	require.Equal(t, oteltrace.SpanKindProducer, producer.SpanKind())
	assertAttr(t, producer.Attributes(), "messaging.message.conversation_id", replyInbox)
}

// TestFireAndForgetProcessSpanOmitsConversationID pins that a subscribe handler
// for a plain publish (no Reply subject) carries no conversation_id.
func TestFireAndForgetProcessSpanOmitsConversationID(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.fireforget"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)

	err = conn.Publish(context.Background(), subject, []byte("no reply expected"))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	processSpan := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	for _, kv := range processSpan.Attributes() {
		assert.NotEqual(t, "messaging.message.conversation_id", string(kv.Key), "conversation_id should be absent on a reply-less message")
	}
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

// TestPublishSpanNameIsOperationFirst pins the rename from "send {subject}" to
// "publish {subject}" (design.md D1): the span name now matches the
// messaging.operation.name attribute already emitted.
func TestPublishSpanNameIsOperationFirst(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "test.publish.opname"
	err = conn.Publish(context.Background(), subject, []byte("hello"))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	s := spans[0]
	assert.Equal(t, "publish "+subject, s.Name())
	assertAttr(t, s.Attributes(), "messaging.operation.name", "publish")
}

// TestRequestSpanNameIsOperationFirst pins the rename from the
// destination-first "{subject} request" to the operation-first
// "request {subject}".
func TestRequestSpanNameIsOperationFirst(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "req.opname"
	_, err = conn.NatsConn().Subscribe(subject, func(msg *nats.Msg) {
		_ = msg.Respond([]byte("pong"))
	})
	require.NoError(t, err)

	_, err = conn.Request(subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)

	spans := sr.Ended()
	wantName := "request " + subject
	requestSpan := findSpanByNameAndKind(spans, wantName, oteltrace.SpanKindClient)
	require.NotNil(t, requestSpan, "no client span named %q", wantName)
	for _, s := range spans {
		assert.NotEqual(t, subject+" request", s.Name(), "destination-first span name should not be emitted")
	}
}

// TestReplyReceiveSpanBareNameAndDestinationMarkers pins design.md D2: the
// reply-receive span carries no destination segment in its name and is the
// only span carrying messaging.destination.temporary/anonymous — an ordinary
// publish/process span carries neither.
func TestReplyReceiveSpanBareNameAndDestinationMarkers(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	reqSubject := "req.markers"
	_, err = conn.NatsConn().Subscribe(reqSubject, func(msg *nats.Msg) {
		_ = msg.Respond([]byte("pong"))
	})
	require.NoError(t, err)

	_, err = conn.Request(reqSubject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)

	receiveSpan := waitSpanByNameAndKind(t, sr, "receive", oteltrace.SpanKindClient)
	assertBoolAttr(t, receiveSpan.Attributes(), "messaging.destination.temporary", true)
	assertBoolAttr(t, receiveSpan.Attributes(), "messaging.destination.anonymous", true)

	ordinarySubject := "test.markers.ordinary"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(ordinarySubject, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)
	err = conn.Publish(context.Background(), ordinarySubject, []byte("ordinary"))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	processSpan := waitSpanByNameAndKind(t, sr, "process "+ordinarySubject, oteltrace.SpanKindConsumer)
	assertNoAttr(t, processSpan.Attributes(), "messaging.destination.temporary")
	assertNoAttr(t, processSpan.Attributes(), "messaging.destination.anonymous")

	producerSpan := findSpanByNameAndKind(sr.Ended(), "publish "+ordinarySubject, oteltrace.SpanKindProducer)
	require.NotNil(t, producerSpan)
	assertNoAttr(t, producerSpan.Attributes(), "messaging.destination.temporary")
	assertNoAttr(t, producerSpan.Attributes(), "messaging.destination.anonymous")
}

// TestWildcardSubscribeProcessSpanUsesSubscriptionSubjectAsTemplate pins the
// span-name destination resolution: a wildcard subscription's process span is
// named after the subscription subject, not the concrete delivered subject,
// and records both as messaging.destination.template / messaging.destination.name.
func TestWildcardSubscribeProcessSpanUsesSubscriptionSubjectAsTemplate(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	wildcard := "orders.*"
	concrete := "orders.1"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(wildcard, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)

	err = conn.Publish(context.Background(), concrete, []byte("order"))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	processSpan := waitSpanByNameAndKind(t, sr, "process "+wildcard, oteltrace.SpanKindConsumer)
	assertAttr(t, processSpan.Attributes(), "messaging.destination.template", wildcard)
	assertAttr(t, processSpan.Attributes(), "messaging.destination.name", concrete)
}

// TestPublishToInboxOmitsDestinationFromSpanName pins the responder half of a
// manual request/reply exchange: nc.Publish(msg.Reply, data) targets a
// per-request inbox, so the span name drops the destination segment while the
// inbox stays queryable on the attributes.
func TestPublishToInboxOmitsDestinationFromSpanName(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url)
	require.NoError(t, err)
	defer conn.Close()

	const inbox = "_INBOX.7Yh2kQ.3"
	require.NoError(t, conn.Publish(context.Background(), inbox, []byte("reply")))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	s := spans[0]
	assert.Equal(t, "publish", s.Name())
	assertAttr(t, s.Attributes(), "messaging.destination.name", inbox)
	assertAttr(t, s.Attributes(), "messaging.message.conversation_id", inbox)
	assertBoolAttr(t, s.Attributes(), "messaging.destination.temporary", true)
	assertBoolAttr(t, s.Attributes(), "messaging.destination.anonymous", true)
	assertNoAttr(t, s.Attributes(), "messaging.destination.template")
}

// TestSubscribeToInboxOmitsDestinationFromSpanName pins the other manual
// request/reply half: a subscription on an inbox subject. The filter is the
// inbox, so the destination resolution has nothing low-cardinality to name the
// span after.
func TestSubscribeToInboxOmitsDestinationFromSpanName(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url)
	require.NoError(t, err)
	defer conn.Close()

	const inbox = "_INBOX.7Yh2kQ.3"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(inbox, func(m otelnats.Msg) { done <- struct{}{} })
	require.NoError(t, err)

	require.NoError(t, conn.Publish(context.Background(), inbox, []byte("reply")))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	s := waitSpanByNameAndKind(t, sr, "process", oteltrace.SpanKindConsumer)
	assertAttr(t, s.Attributes(), "messaging.destination.name", inbox)
	assertAttr(t, s.Attributes(), "messaging.message.conversation_id", inbox)
	assertBoolAttr(t, s.Attributes(), "messaging.destination.temporary", true)
	assertBoolAttr(t, s.Attributes(), "messaging.destination.anonymous", true)
}

// TestInboxDetectionUsesResolvedDestination pins why the inbox test runs on the
// resolved destination rather than the concrete subject: a subscription to
// "<inbox>.>" resolves to a FILTER that carries the request's nuid, so testing
// the concrete subject alone would still leave an unbounded string in the name.
func TestInboxDetectionUsesResolvedDestination(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url)
	require.NoError(t, err)
	defer conn.Close()

	const filter = "_INBOX.7Yh2kQ.>"
	const concrete = "_INBOX.7Yh2kQ.3"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(filter, func(m otelnats.Msg) { done <- struct{}{} })
	require.NoError(t, err)

	require.NoError(t, conn.Publish(context.Background(), concrete, []byte("reply")))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	s := waitSpanByNameAndKind(t, sr, "process", oteltrace.SpanKindConsumer)
	assertAttr(t, s.Attributes(), "messaging.destination.name", concrete)
	assertNoAttr(t, s.Attributes(), "messaging.destination.template")
}

// TestCustomInboxPrefixRecognisedAlongsideDefault pins both halves of the prefix
// rule: a connection using nats.CustomInboxPrefix recognises its own inboxes AND
// still recognises default-prefix peers, which is the common shape because the
// requester is the side that customises (to keep "subscribe _INBOX.>" from
// handing it every other client's replies) while responders need no inbox
// permission at all.
func TestCustomInboxPrefixRecognisedAlongsideDefault(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nats.CustomInboxPrefix("SVCA"))
	require.NoError(t, err)
	defer conn.Close()

	const ownInbox = "SVCA.7Yh2kQ.3"
	const peerInbox = "_INBOX.9Zk4mP.1"
	require.NoError(t, conn.Publish(context.Background(), ownInbox, []byte("x")))
	require.NoError(t, conn.Publish(context.Background(), peerInbox, []byte("x")))

	spans := sr.Ended()
	require.Len(t, spans, 2)
	for _, s := range spans {
		assert.Equal(t, "publish", s.Name())
		assertBoolAttr(t, s.Attributes(), "messaging.destination.anonymous", true)
	}
	assertAttr(t, spans[0].Attributes(), "messaging.destination.name", ownInbox)
	assertAttr(t, spans[1].Attributes(), "messaging.destination.name", peerInbox)
}

// TestOrdinarySubjectUnaffectedByInboxDetection guards the false-positive edge:
// a subject that merely shares a prefix boundary with "_INBOX." is named
// normally and carries none of the inbox markers.
func TestOrdinarySubjectUnaffectedByInboxDetection(t *testing.T) {
	url := startServer(t)
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url)
	require.NoError(t, err)
	defer conn.Close()

	const subject = "_INBOXES.orders"
	require.NoError(t, conn.Publish(context.Background(), subject, []byte("x")))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	s := spans[0]
	assert.Equal(t, "publish "+subject, s.Name())
	assertNoAttr(t, s.Attributes(), "messaging.destination.temporary")
	assertNoAttr(t, s.Attributes(), "messaging.destination.anonymous")
	assertNoAttr(t, s.Attributes(), "messaging.message.conversation_id")
}
