// Package sampling verifies otel-nats consistent probabilistic sampling
// end-to-end against a real NATS server + OTel Collector, using the
// otel-testkit harness as a black-box toolkit.
//
// There is no harness "plugin": each service is a normal otelnats Conn (plus an
// oteljetstream wrapper) whose TracerProvider exports to the harness collector.
// We seed a known randomness value at the head (harness.SeedContextRV), drive a
// realistic produce→consume flow through the instrumented client (the wrapper
// injects the trace into message headers on publish and extracts it on
// receive), and assert on the spans collected at the sink.
//
// Unlike otel-mongo, the consumer topology is always span-link: every consumer
// path (Subscribe, JetStream Consume/Next/Fetch/...) starts a NEW root span
// linked to the producer's SpanContext — never a parent-child continuation.
// The rv still crosses the link because services use
// harness.ConsistentSampler (WithSingleLinkSeed). The one exception is the
// ADR-41 trace-event hop span (SubscribeTraceEvents), which is a child of the
// original producer span (same trace).
//
// otel-nats has a single tracing gate (no independent propagation flag), so
// gate.Propagation is empty and the env matrix has no propagation-off row.
//
// It lives in its own package (not integration_test, whose TestMain
// force-enables every flag) so the harness can observe the feature-flag
// environment set by the matrix in the Makefile/CI.
package sampling

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
)

// gate names the feature-flag env vars otel-nats reads. Propagation is empty:
// otel-nats bundles header propagation with the tracing gate, so
// ExpectationFromEnv reports PropagationEnabled == TracingEnabled.
var gate = harness.GateEnv{
	Global:  "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED",
	Tracing: "OTEL_NATS_TRACING_ENABLED",
}

// deliverTimeout bounds every wait for a message to cross the broker.
const deliverTimeout = 10 * time.Second

// setup starts the in-process sink + collector, installs the propagator the
// wrappers inject/extract with, and returns sink + collector endpoint + NATS URL.
func setup(t *testing.T) (*harness.Sink, string, string) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	ctx := context.Background()
	sink := harness.StartSink(t)
	harness.DumpOnFailure(t, sink) // dump every collected span if the test fails
	endpoint := harness.StartCollector(ctx, t, sink.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	return sink, endpoint, startNATS(ctx, t)
}

// startNATS runs a JetStream-enabled NATS server container and returns its
// client URL. 2.11+ is required for the ADR-41 trace-event scenario
// (SubscribeTraceEvents); every other scenario works the same as on 2.10.
func startNATS(ctx context.Context, t *testing.T) string {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "nats:2.11-alpine",
		Cmd:          []string{"-js", "-m", "8222"},
		ExposedPorts: []string{"4222/tcp"},
		WaitingFor:   wait.ForLog("Server is ready"),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "start nats container")
	t.Cleanup(func() {
		tctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = container.Terminate(tctx)
	})
	host, err := container.Host(ctx)
	require.NoError(t, err, "nats host")
	port, err := container.MappedPort(ctx, "4222")
	require.NoError(t, err, "nats port")
	return fmt.Sprintf("nats://%s:%s", host, port.Port())
}

// service is one instrumented NATS endpoint bound to its own service TP.
type service struct {
	name   string
	conn   *otelnats.Conn
	js     oteljetstream.JetStream
	tracer trace.Tracer
}

// buildServices creates one otelnats Conn (+ JetStream wrapper) per rate, each
// with a consistent-sampler TracerProvider exporting to endpoint. Service
// names are prefix0, prefix1, … Extra trace options (WithTracingEnabled,
// WithTraceDestination, …) are applied to every connection. Returns the
// services and the service-name→rate map for AssertPresence.
func buildServices(t *testing.T, url, endpoint, prefix string, rates []float64, extra ...otelnats.Option) ([]service, map[string]float64) {
	t.Helper()
	svcs := make([]service, len(rates))
	want := make(map[string]float64, len(rates))
	for i, r := range rates {
		name := fmt.Sprintf("%s%d", prefix, i)
		want[name] = r
		tp := harness.BuildTracerProvider(t, name, harness.ConsistentSampler(r), endpoint)
		opts := append([]otelnats.Option{otelnats.WithTracerProvider(tp)}, extra...)
		conn, err := otelnats.ConnectWithOptions(url, nil, opts...)
		require.NoError(t, err, "connect %s", name)
		t.Cleanup(conn.Close)
		js, err := oteljetstream.New(conn)
		require.NoError(t, err, "jetstream %s", name)
		svcs[i] = service{name: name, conn: conn, js: js, tracer: tp.Tracer("chain")}
	}
	return svcs, want
}

// runAttr tags an application span so the harness can group one logical run.
func runAttr(runID string) trace.SpanStartOption { return harness.RunAttrOption(runID) }

// deliverFn drives one producer→consumer hop over a specific otel-nats send/
// receive method pair: prod publishes under prodCtx (the wrapper injects the
// trace into headers), cons receives, and the extracted consumer context
// (carrying the wrapper's linked receive/process span) is returned.
type deliverFn func(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context

// scenario names one span-emitting interaction and how to drive it.
type scenario struct {
	name    string
	deliver deliverFn
}

// coreScenarios covers every span-emitting core-NATS interaction that chains
// the caller's trace: Publish + PublishMsg (send spans), Subscribe +
// QueueSubscribe (process spans), and the two ctx-rooted request entry points
// (request, reply-receive, and responder process spans).
//
// The two ambient-rooted request entry points (Request / RequestMsg) reach the
// same span sites but deliberately sever the caller's trace, so they live in
// requestAmbientScenarios and are asserted separately. The ADR-41 trace-event
// hop span has its own dedicated test.
func coreScenarios() []scenario {
	return []scenario{
		{"publish-subscribe", coreSubscribe},
		{"publishmsg-subscribe", corePublishMsg},
		{"publish-queuesubscribe", coreQueueSubscribe},
		{"request-reply", coreRequestReply},
		{"requestmsg-reply", coreRequestMsgReply},
	}
}

// requestAmbientScenarios covers the request entry points whose origin
// signature has no ctx, so the wrapper roots their span at
// context.Background(): the caller's trace and rv are intentionally not
// chained. See TestNatsRequestAmbientRoot.
func requestAmbientScenarios() []scenario {
	return []scenario{
		{"request-ambient", coreRequestAmbient},
		{"requestmsg-ambient", coreRequestMsgAmbient},
	}
}

// jetStreamScenarios covers every span-emitting JetStream interaction:
// Publish/PublishMsg (send spans), pull Consume, Messages().Next,
// Consumer.Next, the Fetch family, push Consume, and OrderedConsumer.
func jetStreamScenarios() []scenario {
	return []scenario{
		{"consume", jsConsume},
		{"publishmsg-messages-next", jsMessagesNext},
		{"consumer-next", jsConsumerNext},
		{"fetch", jsFetch},
		{"fetchbytes", jsFetchBytes},
		{"fetchnowait", jsFetchNoWait},
		{"push-consume", jsPushConsume},
		{"ordered-consume", jsOrderedConsume},
	}
}

// driveChain drives one seeded run through a chain of any length (≥2 services)
// over the given delivery method. svcs[0] starts its application span from the
// seeded randomness; every later service starts its application span from the
// consumer context the library extracted on the previous hop, then produces the
// next hop from that span. Each hop gets its own subject so streams and
// subscriptions never collide. Returns the run id and the run's application
// spans (filtered by RunAttr).
//
// A service dropped by its own sampler emits no spans, but its SpanContext (and
// the rv in its tracestate) stays valid, so the carrier still hands the same rv
// to the next hop — the invariant TestNatsMultiHopChain pins.
func driveChain(t *testing.T, sink *harness.Sink, svcs []service, rates []float64, scen scenario, rv uint64) (string, []harness.Span) {
	t.Helper()
	require.GreaterOrEqual(t, len(svcs), 2, "chain needs at least a producer and a consumer")
	require.Len(t, rates, len(svcs), "one rate per service")

	runID := uuid.NewString()
	ctx := harness.SeedContextRV(rv)
	for i := range svcs {
		ci, si := svcs[i].tracer.Start(ctx, svcs[i].name, runAttr(runID))
		if i < len(svcs)-1 {
			// Produce while this service's span is still live: the wrapper's
			// send span must be its child.
			subject := fmt.Sprintf("chain.%s.%d.%s", scen.name, i, runID)
			ctx = scen.deliver(t, svcs[i], svcs[i+1], ci, subject)
		}
		si.End()
	}

	return runID, waitRun(t, sink, runID, countSampled(rates, rv))
}

// waitRun blocks until at least wantCount application spans for runID arrive,
// then returns them (failing with a span dump on timeout).
func waitRun(t *testing.T, sink *harness.Sink, runID string, wantCount int) []harness.Span {
	t.Helper()
	return harness.WaitForAppSpans(t, sink, runID, wantCount, 20*time.Second)
}

// waitRunSpans waits until the run's full span set (application + wrapper spans
// + span-link-reachable traces) satisfies pred, then returns it. driveChain only
// waits for the application spans; wrapper spans are exported asynchronously
// from the consumer's handler goroutine, so any assertion that reads them must
// not race their arrival. Returns the last snapshot on timeout so the caller's
// own assertion produces the failure message.
//
// The wait also matters for reachability, not just completeness: SpansOfRun
// walks outward from the run's application spans along trace IDs and links, so a
// wrapper span in a trace that is only linked (never shared) stays invisible
// until the span carrying that link arrives.
func waitRunSpans(t *testing.T, sink *harness.Sink, runID string, pred func([]harness.Span) bool) []harness.Span {
	t.Helper()
	var full []harness.Span
	sink.WaitFor(20*time.Second, func(ss []harness.Span) bool {
		full = harness.SpansOfRun(ss, runID)
		return pred(full)
	})
	return full
}

// waitLinkedRun waits until the run's full span set contains a toService span
// linking back to fromService's trace, then returns that set. Only call when
// both services are expected to be sampled (otherwise the link carrier never
// exports and this just burns the timeout before the following assertion fails).
func waitLinkedRun(t *testing.T, sink *harness.Sink, runID, fromService, toService string) []harness.Span {
	t.Helper()
	return waitRunSpans(t, sink, runID, func(full []harness.Span) bool {
		fromTrace, ok := harness.TraceIDOf(harness.SpansByService(full, fromService), fromService)
		if !ok {
			return false
		}
		for _, s := range harness.SpansByService(full, toService) {
			for _, l := range s.Links {
				if l.TraceID == fromTrace {
					return true
				}
			}
		}
		return false
	})
}

func countSampled(rates []float64, rv uint64) int { return harness.CountSampled(rates, rv) }

// awaitCtx receives the consumer-extracted context delivered by a handler.
func awaitCtx(t *testing.T, ch <-chan context.Context, what string) context.Context {
	t.Helper()
	select {
	case ctx := <-ch:
		return ctx
	case <-time.After(deliverTimeout):
		t.Fatalf("%s: no message within %s", what, deliverTimeout)
		return nil
	}
}

// ── core NATS delivery methods ───────────────────────────────────────────────

// flushSub blocks until the consumer connection's pending SUB command has been
// processed by the server. Core NATS is fire-and-forget: a publish from
// another connection can otherwise outrun the (async) subscription
// registration and the message is silently dropped.
func flushSub(t *testing.T, cons service) {
	t.Helper()
	require.NoError(t, cons.conn.NatsConn().FlushTimeout(5*time.Second), "flush subscription")
}

// coreSubscribe: Conn.Publish → Conn.Subscribe (send + process spans).
func coreSubscribe(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ch := make(chan context.Context, 1)
	sub, err := cons.conn.Subscribe(subject, func(m otelnats.Msg) { ch <- m.Context() })
	require.NoError(t, err, "subscribe")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	flushSub(t, cons)

	require.NoError(t, prod.conn.Publish(prodCtx, subject, []byte("payload")), "publish")
	return awaitCtx(t, ch, "core subscribe")
}

// corePublishMsg: Conn.PublishMsg → Conn.Subscribe (send span via PublishMsg).
func corePublishMsg(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ch := make(chan context.Context, 1)
	sub, err := cons.conn.Subscribe(subject, func(m otelnats.Msg) { ch <- m.Context() })
	require.NoError(t, err, "subscribe")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	flushSub(t, cons)

	require.NoError(t, prod.conn.PublishMsg(prodCtx, &nats.Msg{Subject: subject, Data: []byte("payload")}), "publishmsg")
	return awaitCtx(t, ch, "core publishmsg")
}

// coreQueueSubscribe: Conn.Publish → Conn.QueueSubscribe (queue process span).
func coreQueueSubscribe(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ch := make(chan context.Context, 1)
	sub, err := cons.conn.QueueSubscribe(subject, "workers", func(m otelnats.Msg) { ch <- m.Context() })
	require.NoError(t, err, "queue subscribe")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	flushSub(t, cons)

	require.NoError(t, prod.conn.Publish(prodCtx, subject, []byte("payload")), "publish")
	return awaitCtx(t, ch, "core queue subscribe")
}

// subscribeResponder installs the responder side shared by all four request
// entry points: it publishes the extracted consumer context and replies.
func subscribeResponder(t *testing.T, cons service, subject string) <-chan context.Context {
	t.Helper()
	ch := make(chan context.Context, 1)
	sub, err := cons.conn.Subscribe(subject, func(m otelnats.Msg) {
		ch <- m.Context()
		_ = m.Msg.Respond([]byte("pong"))
	})
	require.NoError(t, err, "responder subscribe")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	flushSub(t, cons)
	return ch
}

// coreRequestReply: Conn.RequestWithContext → responder Subscribe + Respond.
// Covers three wrapper spans at once: the requester's "<subject> request"
// CLIENT span and reply "receive" span, and the responder's process span.
func coreRequestReply(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ch := subscribeResponder(t, cons, subject)

	rctx, cancel := context.WithTimeout(prodCtx, deliverTimeout)
	defer cancel()
	_, err := prod.conn.RequestWithContext(rctx, subject, []byte("ping"))
	require.NoError(t, err, "request")
	return awaitCtx(t, ch, "request/reply")
}

// coreRequestMsgReply: Conn.RequestMsgWithContext — the other ctx-rooted entry
// point into startRequestSpan, so it chains the caller's trace like
// RequestWithContext does.
func coreRequestMsgReply(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ch := subscribeResponder(t, cons, subject)

	rctx, cancel := context.WithTimeout(prodCtx, deliverTimeout)
	defer cancel()
	_, err := prod.conn.RequestMsgWithContext(rctx, &nats.Msg{Subject: subject, Data: []byte("ping")})
	require.NoError(t, err, "requestmsg")
	return awaitCtx(t, ch, "requestmsg/reply")
}

// coreRequestAmbient: Conn.Request (timeout form, no ctx parameter). The
// wrapper documents that this roots the request span at context.Background()
// because the origin nats.go signature has no ctx — so prodCtx's trace and rv
// are deliberately NOT chained. Asserted by TestNatsRequestAmbientRoot.
func coreRequestAmbient(t *testing.T, prod, cons service, _ context.Context, subject string) context.Context {
	t.Helper()
	ch := subscribeResponder(t, cons, subject)

	_, err := prod.conn.Request(subject, []byte("ping"), deliverTimeout)
	require.NoError(t, err, "request (ambient)")
	return awaitCtx(t, ch, "request (ambient)")
}

// coreRequestMsgAmbient: Conn.RequestMsg (timeout form) — the second
// ambient-rooted entry point; same documented trace severance as Request.
func coreRequestMsgAmbient(t *testing.T, prod, cons service, _ context.Context, subject string) context.Context {
	t.Helper()
	ch := subscribeResponder(t, cons, subject)

	_, err := prod.conn.RequestMsg(&nats.Msg{Subject: subject, Data: []byte("ping")}, deliverTimeout)
	require.NoError(t, err, "requestmsg (ambient)")
	return awaitCtx(t, ch, "requestmsg (ambient)")
}

// ── JetStream delivery methods ───────────────────────────────────────────────

// jsSetup creates a uniquely named stream covering subject and a durable pull
// consumer filtered to it, on the consumer service's JetStream wrapper.
func jsSetup(t *testing.T, cons service, subject string) oteljetstream.Consumer {
	t.Helper()
	ctx := context.Background()
	streamName := jsStreamName(subject)
	_, err := cons.js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err, "create stream %s", streamName)
	c, err := cons.js.CreateOrUpdateConsumer(ctx, streamName, oteljetstream.ConsumerConfig{
		Durable:       "dur-" + streamName,
		FilterSubject: subject,
		AckPolicy:     oteljetstream.AckExplicitPolicy,
	})
	require.NoError(t, err, "create consumer")
	return c
}

// jsStreamName derives a JetStream-legal (dot-free) unique stream name from
// the per-run subject.
func jsStreamName(subject string) string {
	return "CHAIN_" + strings.NewReplacer(".", "_", "-", "").Replace(subject)
}

// jsConsume: JetStream.Publish → Consumer.Consume (pull; process span).
func jsConsume(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	c := jsSetup(t, cons, subject)
	ch := make(chan context.Context, 1)
	cc, err := c.Consume(func(m oteljetstream.Msg) {
		_ = m.Ack()
		ch <- m.Context()
	})
	require.NoError(t, err, "consume")
	t.Cleanup(cc.Stop)

	_, err = prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")
	return awaitCtx(t, ch, "js consume")
}

// jsMessagesNext: JetStream.PublishMsg → Messages().Next (receive span).
// Uses PublishMsg so the JetStream PublishMsg send path is covered too.
func jsMessagesNext(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	c := jsSetup(t, cons, subject)
	iter, err := c.Messages()
	require.NoError(t, err, "messages iterator")
	t.Cleanup(iter.Stop)

	_, err = prod.js.PublishMsg(prodCtx, &nats.Msg{Subject: subject, Data: []byte("payload")})
	require.NoError(t, err, "js publishmsg")

	msgCtx, msg, err := iter.Next()
	require.NoError(t, err, "messages next")
	_ = msg.Ack()
	return msgCtx
}

// jsConsumerNext: JetStream.Publish → Consumer.Next (single-shot receive span).
func jsConsumerNext(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	c := jsSetup(t, cons, subject)

	_, err := prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")

	fctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	msgCtx, msg, err := c.Next(fctx)
	require.NoError(t, err, "consumer next")
	_ = msg.Ack()
	return msgCtx
}

// jsBatchDeliver drains one message from the batches returned by fetch,
// retrying until the published message lands (JetStream persistence is
// asynchronous relative to the publish ack observed by the test).
func jsBatchDeliver(t *testing.T, fetch func() (oteljetstream.MessageBatch, error), what string) context.Context {
	t.Helper()
	deadline := time.Now().Add(deliverTimeout)
	for {
		batch, err := fetch()
		require.NoError(t, err, "%s", what)
		for m := range batch.Messages() {
			_ = m.Ack()
			return m.Context()
		}
		require.NoError(t, batch.Error(), "%s batch error", what)
		if time.Now().After(deadline) {
			t.Fatalf("%s: no message within %s", what, deliverTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// jsFetch: JetStream.Publish → Consumer.Fetch (batch receive span).
func jsFetch(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	c := jsSetup(t, cons, subject)
	_, err := prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")
	return jsBatchDeliver(t, func() (oteljetstream.MessageBatch, error) {
		return c.Fetch(1, jetstream.FetchMaxWait(time.Second))
	}, "fetch")
}

// jsFetchBytes: JetStream.Publish → Consumer.FetchBytes (batch receive span).
func jsFetchBytes(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	c := jsSetup(t, cons, subject)
	_, err := prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")
	return jsBatchDeliver(t, func() (oteljetstream.MessageBatch, error) {
		return c.FetchBytes(1024, jetstream.FetchMaxWait(time.Second))
	}, "fetchbytes")
}

// jsFetchNoWait: JetStream.Publish → Consumer.FetchNoWait (batch receive span).
func jsFetchNoWait(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	c := jsSetup(t, cons, subject)
	_, err := prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")
	return jsBatchDeliver(t, func() (oteljetstream.MessageBatch, error) {
		return c.FetchNoWait(5)
	}, "fetchnowait")
}

// jsPushConsume: JetStream.Publish → PushConsumer.Consume (push process span).
func jsPushConsume(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ctx := context.Background()
	streamName := jsStreamName(subject)
	_, err := cons.js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err, "create stream %s", streamName)
	pc, err := cons.js.CreateOrUpdatePushConsumer(ctx, streamName, oteljetstream.ConsumerConfig{
		Durable:       "dur-" + streamName,
		FilterSubject: subject,
		AckPolicy:     oteljetstream.AckExplicitPolicy,
		// Deliver subject must live outside the stream's own subjects — a
		// same-stream deliver subject forms a delivery cycle.
		DeliverSubject: "deliver." + streamName,
	})
	require.NoError(t, err, "create push consumer")

	ch := make(chan context.Context, 1)
	cc, err := pc.Consume(func(m oteljetstream.Msg) {
		_ = m.Ack()
		ch <- m.Context()
	})
	require.NoError(t, err, "push consume")
	t.Cleanup(cc.Stop)

	_, err = prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")
	return awaitCtx(t, ch, "push consume")
}

// jsOrderedConsume: JetStream.Publish → OrderedConsumer.Consume (process span).
func jsOrderedConsume(t *testing.T, prod, cons service, prodCtx context.Context, subject string) context.Context {
	t.Helper()
	ctx := context.Background()
	streamName := jsStreamName(subject)
	_, err := cons.js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err, "create stream %s", streamName)
	stream, err := cons.js.Stream(ctx, streamName)
	require.NoError(t, err, "stream handle")
	oc, err := stream.OrderedConsumer(ctx, oteljetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subject},
	})
	require.NoError(t, err, "ordered consumer")

	ch := make(chan context.Context, 1)
	cc, err := oc.Consume(func(m oteljetstream.Msg) { ch <- m.Context() })
	require.NoError(t, err, "ordered consume")
	t.Cleanup(cc.Stop)

	_, err = prod.js.Publish(prodCtx, subject, []byte("payload"))
	require.NoError(t, err, "js publish")
	return awaitCtx(t, ch, "ordered consume")
}
