package oteljetstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

// setupInboxJS builds a traced JetStream over a connection whose OWN inboxes use a
// custom prefix. That keeps the JetStream client's API request/reply traffic off
// "_INBOX.", so a stream capturing "_INBOX.>" archives only what the test publishes
// while "_INBOX." stays a recognised inbox prefix (every peer that did not customise).
func setupInboxJS(t *testing.T) (oteljetstream.JetStream, *tracetest.SpanRecorder) {
	t.Helper()
	url := startJetStreamServer(t)
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))

	conn, err := otelnats.Connect(url, nats.CustomInboxPrefix("SVCA"))
	require.NoError(t, err)
	t.Cleanup(conn.Close)

	js, err := oteljetstream.New(conn)
	require.NoError(t, err)
	return js, sr
}

// inboxStream creates a stream that archives replies — a legal NATS configuration that
// request/reply-over-JetStream deployments use to make replies durable.
func inboxStream(t *testing.T, js oteljetstream.JetStream, name string) oteljetstream.Stream {
	t.Helper()
	ctx := context.Background()
	_, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     name,
		Subjects: []string{"_INBOX.>"},
	})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, name)
	require.NoError(t, err)
	return stream
}

func assertBoolAttr(t *testing.T, attrs []attribute.KeyValue, key string, want bool) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			if got := kv.Value.AsBool(); got != want {
				t.Errorf("attribute %q = %v, want %v", key, got, want)
			}
			return
		}
	}
	t.Errorf("attribute %q missing", key)
}

func spanNamed(sr *tracetest.SpanRecorder, name string, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	for _, s := range sr.Ended() {
		if s.Name() == name && s.SpanKind() == kind {
			return s
		}
	}
	return nil
}

func waitSpan(t *testing.T, sr *tracetest.SpanRecorder, name string, kind oteltrace.SpanKind) trace.ReadOnlySpan {
	t.Helper()
	var found trace.ReadOnlySpan
	require.Eventually(t, func() bool {
		found = spanNamed(sr, name, kind)
		return found != nil
	}, 3*time.Second, 5*time.Millisecond, "no %v span named %q", kind, name)
	return found
}

// TestInboxCapturedStreamNamesSpansBare pins the JetStream half of the
// low-cardinality rule. A stream over "_INBOX.>" with an unfiltered consumer resolves
// its destination to the concrete per-request inbox subject, so passing no inbox
// prefixes named every span after a nuid — the same unbounded-name defect the core
// NATS paths fix. Covers the publish site and the Fetch batch forwarder.
func TestInboxCapturedStreamNamesSpansBare(t *testing.T) {
	js, sr := setupInboxJS(t)
	ctx := context.Background()
	stream := inboxStream(t, js, "REPLIES_BARE")

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:   "replies-bare-c",
		AckPolicy: oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	peerInbox := nats.NewInbox()
	require.Contains(t, peerInbox, "_INBOX.")
	_, err = js.Publish(ctx, peerInbox, []byte("reply payload"))
	require.NoError(t, err)

	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	require.NoError(t, err)
	var received int
	for m := range batch.Messages() {
		received++
		_ = m.Ack()
	}
	require.NoError(t, batch.Error())
	require.Equal(t, 1, received)

	publishSpan := spanNamed(sr, "publish", oteltrace.SpanKindProducer)
	require.NotNil(t, publishSpan, "publishing to an inbox names its span bare")
	assertAttr(t, publishSpan.Attributes(), "messaging.destination.name", peerInbox)
	assertAttr(t, publishSpan.Attributes(), "messaging.message.conversation_id", peerInbox)
	assertBoolAttr(t, publishSpan.Attributes(), "messaging.destination.temporary", true)
	assertBoolAttr(t, publishSpan.Attributes(), "messaging.destination.anonymous", true)

	receiveSpan := waitSpan(t, sr, "receive", oteltrace.SpanKindClient)
	assertAttr(t, receiveSpan.Attributes(), "messaging.destination.name", peerInbox)
	assertAttr(t, receiveSpan.Attributes(), "messaging.message.conversation_id", peerInbox)
	assertBoolAttr(t, receiveSpan.Attributes(), "messaging.destination.temporary", true)
	assertNoAttr(t, receiveSpan.Attributes(), "messaging.destination.template")
}

// TestInboxPrefixOnlyFilterStaysInSpanName pins the bounded carve-out: a consumer
// filtered on "_INBOX.>" declared a fixed, low-cardinality subject, so semconv's first
// choice for {destination} (use messaging.destination.template when available) applies
// and the destination stays in the name. The delivery is still an inbox, so the
// temporary/anonymous markers are still recorded. Covers Consumer.Next.
func TestInboxPrefixOnlyFilterStaysInSpanName(t *testing.T) {
	js, sr := setupInboxJS(t)
	ctx := context.Background()
	stream := inboxStream(t, js, "REPLIES_FILTER")

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "replies-filter-c",
		FilterSubject: "_INBOX.>",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	peerInbox := nats.NewInbox()
	_, err = js.Publish(ctx, peerInbox, []byte("reply payload"))
	require.NoError(t, err)

	nextCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, msg, err := cons.Next(nextCtx)
	require.NoError(t, err)
	require.NoError(t, msg.Ack())

	span := waitSpan(t, sr, "receive _INBOX.>", oteltrace.SpanKindClient)
	assertAttr(t, span.Attributes(), "messaging.destination.template", "_INBOX.>")
	assertAttr(t, span.Attributes(), "messaging.destination.name", peerInbox)
	assertAttr(t, span.Attributes(), "messaging.message.conversation_id", peerInbox)
	assertBoolAttr(t, span.Attributes(), "messaging.destination.temporary", true)
	assertBoolAttr(t, span.Attributes(), "messaging.destination.anonymous", true)
}

// TestInboxCapturedStreamConsumeAndMessagesNameSpansBare covers the two remaining
// resolve sites: the Consume delivery handler and the MessagesContext iterator.
func TestInboxCapturedStreamConsumeAndMessagesNameSpansBare(t *testing.T) {
	js, sr := setupInboxJS(t)
	ctx := context.Background()
	stream := inboxStream(t, js, "REPLIES_CONSUME")

	cons, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:   "replies-consume-c",
		AckPolicy: oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	peerInbox := nats.NewInbox()
	_, err = js.Publish(ctx, peerInbox, []byte("one"))
	require.NoError(t, err)

	done := make(chan struct{})
	cc, err := cons.Consume(func(m oteljetstream.Msg) {
		_ = m.Ack()
		close(done)
	})
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Consume handler never ran")
	}
	cc.Stop()

	processSpan := waitSpan(t, sr, "process", oteltrace.SpanKindConsumer)
	assertAttr(t, processSpan.Attributes(), "messaging.destination.name", peerInbox)
	assertBoolAttr(t, processSpan.Attributes(), "messaging.destination.anonymous", true)

	// Second message through the MessagesContext iterator.
	second := nats.NewInbox()
	_, err = js.Publish(ctx, second, []byte("two"))
	require.NoError(t, err)

	iter, err := cons.Messages()
	require.NoError(t, err)
	_, msg, err := iter.Next()
	require.NoError(t, err)
	require.NoError(t, msg.Ack())
	iter.Stop()

	receiveSpan := waitSpan(t, sr, "receive", oteltrace.SpanKindClient)
	assertBoolAttr(t, receiveSpan.Attributes(), "messaging.destination.temporary", true)
}
