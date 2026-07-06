package integration_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// natsURL is the connection string for the shared NATS container, set once in TestMain.
var natsURL string

// TestMain starts a NATS container with JetStream enabled, runs all tests, then stops it.
func TestMain(m *testing.M) {
	_ = os.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "1")
	_ = os.Setenv("OTEL_NATS_TRACING_ENABLED", "1")

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "nats:2.10-alpine",
		Cmd:          []string{"-js", "-m", "8222"},
		ExposedPorts: []string{"4222/tcp"},
		WaitingFor:   wait.ForLog("Server is ready"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("start nats container: %v", err)
	}
	port, err := c.MappedPort(ctx, "4222")
	if err != nil {
		log.Fatalf("get nats port: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		log.Fatalf("get nats host: %v", err)
	}
	natsURL = fmt.Sprintf("nats://%s:%s", host, port.Port())

	code := m.Run()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestProvider() (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return tp, sr
}

func findSpanByKind(spans []sdktrace.ReadOnlySpan, kind oteltrace.SpanKind) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.SpanKind() == kind {
			return s
		}
	}
	return nil
}

func findSpanByNameAndKind(spans []sdktrace.ReadOnlySpan, name string, kind oteltrace.SpanKind) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name && s.SpanKind() == kind {
			return s
		}
	}
	return nil
}

// waitSpanByNameAndKind polls until the named span appears in the recorder.
// Consumer handlers use defer span.End(); polling avoids a race under -race.
func waitSpanByNameAndKind(t *testing.T, sr *tracetest.SpanRecorder, name string, kind oteltrace.SpanKind) sdktrace.ReadOnlySpan {
	t.Helper()
	var got sdktrace.ReadOnlySpan
	require.Eventually(t, func() bool {
		got = findSpanByNameAndKind(sr.Ended(), name, kind)
		return got != nil
	}, 2*time.Second, 5*time.Millisecond, "timed out waiting for ended span %q", name)
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
	t.Errorf("attribute %q not found in span", key)
}

// ── core NATS consumer tests ──────────────────────────────────────────────────

// TestIntegration_W3CPropagationRoundtrip verifies that the W3C traceparent header
// survives a Publish → Subscribe round-trip through the real NATS container.
func TestIntegration_W3CPropagationRoundtrip(t *testing.T) {
	tp, _ := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	tracer := tp.Tracer("roundtrip-test")
	parentCtx, parentSpan := tracer.Start(context.Background(), "parent")
	defer parentSpan.End()
	wantTraceID := parentSpan.SpanContext().TraceID()

	subject := "integ.rt.test"
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
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}

	carrier := otelnats.HeaderCarrier{H: h}
	extracted := prop.Extract(context.Background(), carrier)
	gotTraceID := oteltrace.SpanFromContext(extracted).SpanContext().TraceID()
	assert.Equal(t, wantTraceID, gotTraceID)
}

// TestIntegration_SubscribeExtractsTraceContext verifies that Subscribe delivers a
// MsgWithContext whose Context carries the producer's trace ID, and that the
// consumer span is linked to the producer span.
func TestIntegration_SubscribeExtractsTraceContext(t *testing.T) {
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "integ.subscribe.trace"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(),
			"MsgWithContext.Context should carry a valid trace ID")
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
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Subscribe handler")
	}

	consumer := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	producer := findSpanByNameAndKind(sr.Ended(), "send "+subject, oteltrace.SpanKindProducer)
	require.Len(t, consumer.Links(), 1, "consumer span should have exactly 1 link")
	if producer != nil {
		assert.Equal(t, producer.SpanContext().TraceID(), consumer.Links()[0].SpanContext.TraceID())
		assert.Equal(t, producer.SpanContext().SpanID(), consumer.Links()[0].SpanContext.SpanID())
	}
}

// TestIntegration_QueueSubscribeRecordsQueueName verifies that QueueSubscribe records
// the queue group name in the messaging.consumer.group.name span attribute.
func TestIntegration_QueueSubscribeRecordsQueueName(t *testing.T) {
	tp, sr := newTestProvider()
	otel.SetTracerProvider(tp)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject, queue := "integ.queue.sub", "integ-workers"
	done := make(chan struct{}, 1)
	_, err = conn.QueueSubscribe(subject, queue, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)

	err = conn.Publish(context.Background(), subject, []byte("work"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for QueueSubscribe handler")
	}

	consumer := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	assertAttr(t, consumer.Attributes(), "messaging.consumer.group.name", queue)
}

// TestIntegration_SubscribeConsumerSpanLinkedToProducer verifies the span link
// from the consumer span to the producer span across a real NATS container.
func TestIntegration_SubscribeConsumerSpanLinkedToProducer(t *testing.T) {
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	subject := "integ.linkage.test"
	done := make(chan struct{}, 1)
	_, err = conn.Subscribe(subject, func(m otelnats.Msg) {
		done <- struct{}{}
	})
	require.NoError(t, err)

	err = conn.Publish(context.Background(), subject, []byte("link-test"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	producer := waitSpanByNameAndKind(t, sr, "send "+subject, oteltrace.SpanKindProducer)
	consumer := waitSpanByNameAndKind(t, sr, "process "+subject, oteltrace.SpanKindConsumer)
	require.Len(t, consumer.Links(), 1, "consumer span should have 1 link")
	assert.Equal(t, producer.SpanContext().TraceID(), consumer.Links()[0].SpanContext.TraceID())
	assert.Equal(t, producer.SpanContext().SpanID(), consumer.Links()[0].SpanContext.SpanID())
}
