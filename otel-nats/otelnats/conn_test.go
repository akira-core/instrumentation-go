package otelnats_test

import (
	"context"
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

	otelnats "github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
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
	client := findSpanByKind(spans, oteltrace.SpanKindClient)
	require.NotNil(t, client, "no client span for request")
	assert.Equal(t, subject+" request", client.Name())
	consumer := findSpanByKind(spans, oteltrace.SpanKindConsumer)
	require.NotNil(t, consumer, "no consumer span for reply")
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

func TestDeliverSpanDisabledWithoutEndpoint(t *testing.T) {
	// Ensure OTEL_EXPORTER_OTLP_ENDPOINT is unset — deliver span should be disabled.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	url := startServer(t)
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	assert.False(t, conn.DeliverSpanEnabled(), "deliver span should be disabled without endpoint")

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

func TestDeliverSpanConsumerLinksToDeliverSpan(t *testing.T) {
	// Set endpoint to enable deliver span. The exporter won't connect but spans still get created.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	url := startServer(t)
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	assert.True(t, conn.DeliverSpanEnabled(), "deliver span should be enabled with endpoint")

	subject := "test.deliver"
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
	producer := findSpanByNameAndKind(spans, "send "+subject, oteltrace.SpanKindProducer)
	consumer := findSpanByNameAndKind(spans, "process "+subject, oteltrace.SpanKindConsumer)
	require.NotNil(t, producer, "missing producer span")
	require.NotNil(t, consumer, "missing consumer span")

	// Consumer link should NOT point to producer span (it should point to deliver span)
	require.Len(t, consumer.Links(), 1, "consumer should have 1 link")
	linkSpanID := consumer.Links()[0].SpanContext.SpanID()
	assert.NotEqual(t, producer.SpanContext().SpanID(), linkSpanID,
		"consumer link should point to deliver span, not producer span")
	// The link should share the same traceID as the producer (deliver is child of producer)
	assert.Equal(t, producer.SpanContext().TraceID(), consumer.Links()[0].SpanContext.TraceID(),
		"deliver span should share traceID with producer")
}

// newAlwaysSampleProvider gives a TracerProvider that records all spans,
// independent of the parent's sampled flag. Required for tests that extract
// a sampled=0 traceparent — under ParentBased sampler the derived consumer
// span would also be dropped and never appear in the recorder.
func newAlwaysSampleProvider() (*trace.TracerProvider, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(
		trace.WithSampler(trace.AlwaysSample()),
		trace.WithSpanProcessor(sr),
	)
	return tp, sr
}

// publishWithFlags publishes a message whose traceparent header carries the
// supplied trace-flags byte (01 = sampled, 00 = not sampled). Used to drive
// the consumer-side link gate without depending on producer-side sampler
// configuration.
func publishWithFlags(t *testing.T, nc *nats.Conn, subject string, flagsHex string) {
	t.Helper()
	hdr := nats.Header{}
	hdr.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-"+flagsHex)
	require.NoError(t, nc.PublishMsg(&nats.Msg{Subject: subject, Header: hdr, Data: []byte("payload")}))
	require.NoError(t, nc.Flush())
}

// makeSpanContext builds a SpanContext with the supplied sampled flag.
// 32-hex traceID + 16-hex spanID with caller-chosen TraceFlags.
func makeSpanContext(t *testing.T, sampled bool) oteltrace.SpanContext {
	t.Helper()
	tid, err := oteltrace.TraceIDFromHex("aabbccddeeff00112233445566778899")
	require.NoError(t, err)
	sid, err := oteltrace.SpanIDFromHex("1122334455667788")
	require.NoError(t, err)
	var flags oteltrace.TraceFlags
	if sampled {
		flags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: flags,
		Remote:     true,
	})
}

// connectWithDeliverEnabled returns a Conn whose deliverTracer is enabled
// (OTEL_EXPORTER_OTLP_ENDPOINT set). Hot-path checks for deliverTracer != nil
// will return true.
func connectWithDeliverEnabled(t *testing.T) *otelnats.Conn {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	url := startServer(t)
	tp, _ := newTestProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	require.True(t, conn.DeliverSpanEnabled(), "deliver span must be enabled for sampling-gate tests")
	return conn
}

// TestConsumerSpanNoLinkWhenOriginNotSampled validates the sampler-aware
// link-gate invariant for the core NATS Subscribe path: an upstream span
// with sampled=0 must not be linked from the consumer span (dangling-link
// prevention).
func TestConsumerSpanNoLinkWhenOriginNotSampled(t *testing.T) {
	url := startServer(t)
	tp, sr := newAlwaysSampleProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	subject := "link.unsampled"
	done := make(chan struct{})
	_, err = conn.Subscribe(subject, func(_ otelnats.Msg) { close(done) })
	require.NoError(t, err)

	publishWithFlags(t, conn.NatsConn(), subject, "00")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe handler did not fire")
	}

	consumerSpan := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	require.NotNil(t, consumerSpan)
	assert.Empty(t, consumerSpan.Links(),
		"consumer span must NOT carry a link when origin sampled=0 (dangling-link prevention)")
}

// TestConsumerSpanHasLinkWhenOriginSampled is the positive counterpart —
// the link is preserved when the upstream trace is sampled.
func TestConsumerSpanHasLinkWhenOriginSampled(t *testing.T) {
	url := startServer(t)
	tp, sr := newAlwaysSampleProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	subject := "link.sampled"
	done := make(chan struct{})
	_, err = conn.Subscribe(subject, func(_ otelnats.Msg) { close(done) })
	require.NoError(t, err)

	publishWithFlags(t, conn.NatsConn(), subject, "01")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe handler did not fire")
	}

	consumerSpan := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	require.NotNil(t, consumerSpan)
	require.Len(t, consumerSpan.Links(), 1,
		"consumer span MUST carry a link when origin sampled=1")
	assert.True(t, consumerSpan.Links()[0].SpanContext.IsSampled())
}

// TestRecordReplyLinkRespectsSamplerFlag validates the link-gate invariant
// for the Request/Reply path: the synthetic "receive" CONSUMER span emitted
// on reply must respect the reply-side sampled flag the same way Subscribe
// does.
func TestRecordReplyLinkRespectsSamplerFlag(t *testing.T) {
	url := startServer(t)
	tp, sr := newAlwaysSampleProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	subject := "link.reply"
	rawSub, err := conn.NatsConn().Subscribe(subject, func(m *nats.Msg) {
		hdr := nats.Header{}
		hdr.Set("traceparent", "00-cccccccccccccccccccccccccccccccc-dddddddddddddddd-00")
		_ = m.RespondMsg(&nats.Msg{Header: hdr, Data: []byte("reply")})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawSub.Unsubscribe() })

	_, err = conn.RequestWithContext(context.Background(), subject, []byte("ping"))
	require.NoError(t, err)

	// Reply subject is an _INBOX.* random subject; locate the CONSUMER span by
	// kind + name prefix "receive ".
	require.Eventually(t, func() bool {
		for _, s := range sr.Ended() {
			if s.SpanKind() == oteltrace.SpanKindConsumer && len(s.Name()) > len("receive ") && s.Name()[:8] == "receive " {
				return assert.Empty(t, s.Links(),
					"reply consumer span must NOT carry a link when reply sampled=0")
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "wait for reply CONSUMER span")
}

// TestDeliverSpanSkippedWhenUpstreamNotSampled validates the sampler-aware
// skip invariant for ConsumerContextWithDeliver: when origin sampled=0, the
// function must early-return without emitting a deliverSpan.
func TestDeliverSpanSkippedWhenUpstreamNotSampled(t *testing.T) {
	conn := connectWithDeliverEnabled(t)

	origin := makeSpanContext(t, false)
	out := conn.ConsumerContextWithDeliver(context.Background(), "subj.test", origin)

	remote := oteltrace.SpanContextFromContext(out)
	assert.False(t, remote.IsValid(),
		"sampled=0 upstream must NOT trigger deliverSpan (no remote span context expected)")
}

// TestDeliverSpanCreatedWhenUpstreamSampled is the positive counterpart —
// origin sampled=1 must produce a remote span context (= deliverSpan emitted).
func TestDeliverSpanCreatedWhenUpstreamSampled(t *testing.T) {
	conn := connectWithDeliverEnabled(t)

	origin := makeSpanContext(t, true)
	out := conn.ConsumerContextWithDeliver(context.Background(), "subj.test", origin)

	remote := oteltrace.SpanContextFromContext(out)
	assert.True(t, remote.IsValid(),
		"sampled=1 upstream MUST trigger deliverSpan (remote span context expected)")
}

// TestStartDeliverSpanSkippedWhenLocalNotSampled validates the producer-side
// invariant: StartDeliverSpan must skip when the local span context is valid
// but not sampled.
func TestStartDeliverSpanSkippedWhenLocalNotSampled(t *testing.T) {
	conn := connectWithDeliverEnabled(t)

	notSampled := makeSpanContext(t, false)
	parent := oteltrace.ContextWithSpanContext(context.Background(), notSampled)

	out := conn.StartDeliverSpan(parent, "subj.test")

	gotSC := oteltrace.SpanContextFromContext(out)
	assert.Equal(t, notSampled.SpanID(), gotSC.SpanID(),
		"sampled=0 local span must NOT trigger deliverSpan (span ID should be unchanged)")
}

// TestStartDeliverSpanCreatedWhenLocalSampled is the positive counterpart.
func TestStartDeliverSpanCreatedWhenLocalSampled(t *testing.T) {
	conn := connectWithDeliverEnabled(t)

	sampled := makeSpanContext(t, true)
	parent := oteltrace.ContextWithSpanContext(context.Background(), sampled)

	out := conn.StartDeliverSpan(parent, "subj.test")

	gotSC := oteltrace.SpanContextFromContext(out)
	assert.True(t, gotSC.IsValid(), "sampled=1 local span MUST trigger deliverSpan")
	assert.NotEqual(t, sampled.SpanID(), gotSC.SpanID(),
		"a fresh deliverSpan should appear with a different span ID")
}

// TestConsumerSpanStillCreatedWhenDeliverSkipped verifies that skipping the
// deliverSpan does NOT suppress the downstream consumer span — the consumer
// wrapper still emits its own span, just without a broker-hop parent.
// Provider order matters: AlwaysSample must be installed BEFORE Connect so
// the conn caches the AlwaysSample-backed tracer.
func TestConsumerSpanStillCreatedWhenDeliverSkipped(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	url := startServer(t)
	tp, sr := newAlwaysSampleProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	conn, err := otelnats.Connect(url, nil)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	require.True(t, conn.DeliverSpanEnabled())

	subject := "deliver.consumerstill"
	done := make(chan struct{})
	_, err = conn.Subscribe(subject, func(_ otelnats.Msg) { close(done) })
	require.NoError(t, err)

	publishWithFlags(t, conn.NatsConn(), subject, "00")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe handler did not fire")
	}

	consumerSpan := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	require.NotNil(t, consumerSpan, "consumer span must still be emitted even when deliverSpan is skipped")
}
