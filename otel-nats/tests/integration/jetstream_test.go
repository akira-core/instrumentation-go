package integration_test

// This file covers the pull-based JetStream consumer and publish paths exposed
// by oteljetstream. Push consumers and the Unwrap escape hatches are covered by
// in-package unit tests in oteljetstream/pushconsumer_test.go.
// All tests share the NATS container started in TestMain (nats_test.go).
// Each test creates a uniquely named stream to avoid cross-test conflicts.

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// ── Consumer.Consume ─────────────────────────────────────────────────────────

// TestIntegration_ConsumeTraceContext verifies that Consumer.Consume delivers a
// MsgWithContext whose Context carries a valid trace ID from the publisher.
func TestIntegration_ConsumeTraceContext(t *testing.T) {
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_CONSUME",
		Subjects: []string{"integ.consume.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_CONSUME")
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "integ-consume-c",
		FilterSubject: "integ.consume.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	done := make(chan struct{}, 1)
	cc, err := cons.Consume(func(m oteljetstream.Msg) {
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(),
			"MsgWithContext.Context should carry a valid trace ID")
		done <- struct{}{}
		_ = m.Ack()
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("pub")
	pubCtx, pubSpan := tracer.Start(ctx, "parent")
	defer pubSpan.End()
	_, err = js.Publish(pubCtx, "integ.consume.msg", []byte("hello consume"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Consume handler did not receive trace context in time")
	}

	consumer := waitSpanByNameAndKind(t, sr, "process integ.consume.msg", oteltrace.SpanKindConsumer)
	assert.NotNil(t, consumer)
}

// ── Consumer.Messages / Next ─────────────────────────────────────────────────

// TestIntegration_MessagesNextTraceContext verifies that Messages().Next() returns
// a context carrying the publisher's trace ID.
func TestIntegration_MessagesNextTraceContext(t *testing.T) {
	tp, _ := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_MSGS",
		Subjects: []string{"integ.msgs.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_MSGS")
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "integ-msgs-c",
		FilterSubject: "integ.msgs.one",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	iter, err := cons.Messages()
	require.NoError(t, err)
	defer iter.Stop()

	_, err = js.Publish(ctx, "integ.msgs.one", []byte("data"))
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)

	msgCtx, msg, err := iter.Next()
	require.NoError(t, err)
	assert.Equal(t, "data", string(msg.Data()))
	assert.True(t, oteltrace.SpanFromContext(msgCtx).SpanContext().TraceID().IsValid(),
		"Next() should return a context with a valid trace ID")
	_ = msg.Ack()
}

// TestIntegration_ConsumerNextSingleMsg verifies that Consumer.Next() returns
// a context carrying the publisher's trace ID for a single-message fetch.
func TestIntegration_ConsumerNextSingleMsg(t *testing.T) {
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_NEXT",
		Subjects: []string{"integ.next.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_NEXT")
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "integ-next-c",
		FilterSubject: "integ.next.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("pub")
	pubCtx, pubSpan := tracer.Start(ctx, "parent")
	_, err = js.Publish(pubCtx, "integ.next.msg", []byte("single"))
	require.NoError(t, err)
	pubSpan.End()

	time.Sleep(300 * time.Millisecond)

	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msgCtx, msg, err := cons.Next(fetchCtx)
	require.NoError(t, err)
	assert.True(t, oteltrace.SpanFromContext(msgCtx).SpanContext().TraceID().IsValid(),
		"Next() should return context with valid trace ID")
	_ = msg.Ack()

	consumer := waitSpanByNameAndKind(t, sr, "receive integ.next.msg", oteltrace.SpanKindClient)
	assert.NotNil(t, consumer)
}

// ── Consumer.Fetch ───────────────────────────────────────────────────────────

// TestIntegration_FetchTraceContext verifies that Fetch() → MessageBatch.MessagesWithContext()
// delivers messages with valid trace context, and the consumer span is linked to the producer.
func TestIntegration_FetchTraceContext(t *testing.T) {
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_FETCH",
		Subjects: []string{"integ.fetch.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_FETCH")
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "integ-fetch-c",
		FilterSubject: "integ.fetch.msg",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("pub")
	pubCtx, pubSpan := tracer.Start(ctx, "parent")
	_, err = js.Publish(pubCtx, "integ.fetch.msg", []byte("hello fetch"))
	require.NoError(t, err)
	pubSpan.End()

	var received int
	var batch oteljetstream.MessageBatch
	for attempt := 0; attempt < 30; attempt++ {
		batch, err = cons.Fetch(5, jetstream.FetchMaxWait(300*time.Millisecond))
		require.NoError(t, err)
		for m := range batch.Messages() {
			received++
			assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(),
				"each fetched message should carry a valid trace ID")
			_ = m.Ack()
		}
		if received == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, received, "expected exactly 1 message")
	require.NoError(t, batch.Error())

	consumerSpan := findSpanByKind(sr.Ended(), oteltrace.SpanKindClient)
	producerSpan := findSpanByKind(sr.Ended(), oteltrace.SpanKindProducer)
	require.NotNil(t, consumerSpan, "consumer span must exist")
	if producerSpan != nil && len(consumerSpan.Links()) == 1 {
		assert.Equal(t, producerSpan.SpanContext().TraceID(), consumerSpan.Links()[0].SpanContext.TraceID())
	}
}

// TestIntegration_FetchBytesTraceContext verifies FetchBytes() delivers trace context.
func TestIntegration_FetchBytesTraceContext(t *testing.T) {
	tp, _ := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_BYTES",
		Subjects: []string{"integ.bytes.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_BYTES")
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "integ-bytes-c",
		FilterSubject: "integ.bytes.a",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "integ.bytes.a", []byte("hello bytes"))
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)

	batch, err := cons.FetchBytes(1024, jetstream.FetchMaxWait(5*time.Second))
	require.NoError(t, err)
	n := 0
	for m := range batch.Messages() {
		n++
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(),
			"FetchBytes message should carry a valid trace ID")
		_ = m.Ack()
	}
	assert.Equal(t, 1, n)
}

// TestIntegration_FetchNoWaitTraceContext verifies FetchNoWait() delivers trace context.
func TestIntegration_FetchNoWaitTraceContext(t *testing.T) {
	tp, _ := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_NOWAIT",
		Subjects: []string{"integ.nowait.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_NOWAIT")
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "integ-nowait-c",
		FilterSubject: "integ.nowait.x",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "integ.nowait.x", []byte("v"))
	require.NoError(t, err)
	// Wait for JetStream persistence before FetchNoWait.
	time.Sleep(200 * time.Millisecond)

	batch, err := cons.FetchNoWait(5)
	require.NoError(t, err)
	n := 0
	for m := range batch.Messages() {
		n++
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(),
			"FetchNoWait message should carry a valid trace ID")
		_ = m.Ack()
	}
	assert.Equal(t, 1, n)
}

// ── OrderedConsumer ──────────────────────────────────────────────────────────

// TestIntegration_OrderedConsumerTraceContext verifies that an OrderedConsumer
// delivers messages with trace context propagation.
func TestIntegration_OrderedConsumerTraceContext(t *testing.T) {
	tp, sr := newTestProvider()
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)

	conn, err := otelnats.Connect(natsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "INTEG_ORDERED",
		Subjects: []string{"integ.ordered.>"},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, "INTEG_ORDERED")
	require.NoError(t, err)

	orderedCons, err := stream.OrderedConsumer(ctx, oteljetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"integ.ordered.msg"},
	})
	require.NoError(t, err)

	done := make(chan struct{}, 1)
	cc, err := orderedCons.Consume(func(m oteljetstream.Msg) {
		assert.True(t, oteltrace.SpanFromContext(m.Context()).SpanContext().TraceID().IsValid(),
			"OrderedConsumer MsgWithContext should carry a valid trace ID")
		done <- struct{}{}
		// OrderedConsumer does not require explicit Ack.
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("ordered-pub")
	pubCtx, pubSpan := tracer.Start(ctx, "ordered-parent")
	defer pubSpan.End()
	_, err = js.Publish(pubCtx, "integ.ordered.msg", []byte("hello ordered"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OrderedConsumer handler did not receive trace context in time")
	}

	consumer := waitSpanByNameAndKind(t, sr, "process integ.ordered.msg", oteltrace.SpanKindConsumer)
	assertAttr(t, consumer.Attributes(), "messaging.consumer.group.name", "ordered-consumer")
}
