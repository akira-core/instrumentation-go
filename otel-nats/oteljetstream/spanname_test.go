package oteljetstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
)

// assertNoAttr fails the test if key is present in attrs.
func assertNoAttr(t *testing.T, attrs []attribute.KeyValue, key string) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			t.Errorf("attribute %q unexpectedly present (value %q)", key, kv.Value.AsString())
			return
		}
	}
}

// TestWildcardFilterConsumerReceiveSpansShareOneName pins the low-cardinality
// naming rule: a consumer with a single wildcard filter subject names every
// receive span after the filter, not the concrete delivered subject, while
// still recording the concrete subject as messaging.destination.name.
func TestWildcardFilterConsumerReceiveSpansShareOneName(t *testing.T) {
	js, sr, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "WILDFILTER",
		Subjects: []string{"orders.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "WILDFILTER")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "wildfilter-c",
		FilterSubject: "orders.*",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "orders.1", []byte("one"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "orders.2", []byte("two"))
	require.NoError(t, err)

	batch, err := cons.Fetch(2, jetstream.FetchMaxWait(3*time.Second))
	require.NoError(t, err)
	var received int
	for m := range batch.Messages() {
		received++
		_ = m.Ack()
	}
	require.NoError(t, batch.Error())
	require.Equal(t, 2, received)

	var receiveSpans []trace.ReadOnlySpan
	require.Eventually(t, func() bool {
		receiveSpans = nil
		for _, s := range sr.Ended() {
			if s.Name() == "receive orders.*" && s.SpanKind() == oteltrace.SpanKindClient {
				receiveSpans = append(receiveSpans, s)
			}
		}
		return len(receiveSpans) == 2
	}, 2*time.Second, 5*time.Millisecond, "both messages should produce a \"receive orders.*\" span")

	var gotDestNames []string
	for _, s := range receiveSpans {
		assertAttr(t, s.Attributes(), "messaging.destination.template", "orders.*")
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "messaging.destination.name" {
				gotDestNames = append(gotDestNames, kv.Value.AsString())
			}
		}
	}
	assert.ElementsMatch(t, []string{"orders.1", "orders.2"}, gotDestNames,
		"each span keeps its own concrete subject on messaging.destination.name")
}

// TestExactFilterConsumerReceiveSpanHasNoTemplateAttr verifies a consumer whose
// single filter subject is an exact (non-wildcard) subject keeps the current
// "receive {filter}" naming and does NOT gain messaging.destination.template —
// the filter equals the concrete subject, so there is no template to record.
func TestExactFilterConsumerReceiveSpanHasNoTemplateAttr(t *testing.T) {
	js, sr, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "EXACTFILTER",
		Subjects: []string{"exact.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "EXACTFILTER")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "exactfilter-c",
		FilterSubject: "exact.new",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "exact.new", []byte("v"))
	require.NoError(t, err)

	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	require.NoError(t, err)
	msg, ok := <-batch.Messages()
	require.True(t, ok, "expected one message")
	_ = msg.Ack()

	span := waitSpanByNameAndKind(t, sr, "receive exact.new", oteltrace.SpanKindClient)
	assertNoAttr(t, span.Attributes(), "messaging.destination.template")
}

// TestMultiFilterConsumerFallsBackToConcreteSubject verifies a consumer with
// more than one filter subject falls back to naming its receive span after the
// concrete delivered subject, with no template attribute — the wrapper never
// fabricates a destination by joining multiple filters.
func TestMultiFilterConsumerFallsBackToConcreteSubject(t *testing.T) {
	js, sr, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "MULTIFILTER",
		Subjects: []string{"multi.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "MULTIFILTER")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:        "multifilter-c",
		FilterSubjects: []string{"multi.new", "multi.cancelled"},
		AckPolicy:      oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "multi.cancelled", []byte("v"))
	require.NoError(t, err)

	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	require.NoError(t, err)
	msg, ok := <-batch.Messages()
	require.True(t, ok, "expected one message")
	_ = msg.Ack()

	span := waitSpanByNameAndKind(t, sr, "receive multi.cancelled", oteltrace.SpanKindClient)
	assertNoAttr(t, span.Attributes(), "messaging.destination.template")
}

// TestPublishSpanNamedPublishNotSend pins the D1 rename: the PRODUCER span for
// a JetStream publish is named "publish {subject}", matching its
// messaging.operation.name attribute — not the old "send {subject}".
func TestPublishSpanNamedPublishNotSend(t *testing.T) {
	js, sr, _ := setupTracedJS(t)
	ctx := context.Background()

	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "PUBLISHNAME",
		Subjects: []string{"publishname.>"},
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "publishname.a", []byte("hi"))
	require.NoError(t, err)

	span := waitSpanByNameAndKind(t, sr, "publish publishname.a", oteltrace.SpanKindProducer)
	require.NotNil(t, span)
	assert.Nil(t, findSpanByNameAndKind(sr.Ended(), "send publishname.a", oteltrace.SpanKindProducer),
		"publish span must not be named with the old \"send\" verb")
}
