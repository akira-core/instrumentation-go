package sampling

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	otelnats "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
)

// envNATSTracingEnabled is this module's tracing variable. otel-flags names only
// process-scoped things, so the module variable is spelled out here rather than
// imported.
const envNATSTracingEnabled = "OTEL_NATS_TRACING_ENABLED"

// unsetEnv removes name for the duration of the test and restores it afterwards.
// t.Setenv cannot express absence, and absence is the state the option tier needs.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	prev, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, prev)
		}
	})
}

// rvLadder spans the decision space for the suite's {0.9, 0.5} rates:
// 0 → nobody sampled; 1<<53 → only rate 0.9; 1<<55 → both; max → both.
var rvLadder = []uint64{0, 1 << 53, 1 << 55, (1 << 56) - 1}

// TestNatsSamplingSuite is the one test that runs meaningfully in every row of
// the feature-flag matrix; ExpectationFromEnv picks the assertion set.
// otel-nats has a single tracing gate (propagation tracks tracing), so there
// are exactly two behaviors: everything on, or everything off.
func TestNatsSamplingSuite(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	sink, endpoint, url := setup(t)

	if !exp.TracingEnabled {
		// Disabled rows (global-off / nats-tracing-off): the wrapper must emit
		// no spans and inject/extract nothing, while application spans still
		// flow through the test's own TracerProviders.
		rates := []float64{1.0, 1.0}
		svcs, _ := buildServices(t, url, endpoint, "svc", rates)
		_, run := driveChain(t, sink, svcs, rates, scenario{"disabled", coreSubscribe}, uint64(1)<<55)
		require.Len(t, run, 2, "application spans must flow with tracing disabled")
		harness.AssertNoWrapperSpans(t, sink.Spans(), otelnats.ScopeName)
		// No header propagation → the consumer's app span self-seeds its own
		// randomness instead of continuing the seeded rv.
		require.Greater(t, len(harness.DistinctRVs(run)), 1,
			"tracing disabled: rv must NOT propagate to the consumer")
		return
	}

	// All-on rows: deterministic presence over the rv ladder + rv consistency
	// across the span-link hop.
	rates := []float64{0.9, 0.5}
	svcs, want := buildServices(t, url, endpoint, "svc", rates)
	for _, rv := range rvLadder {
		runID, run := driveChain(t, sink, svcs, rates, scenario{"suite", coreSubscribe}, rv)
		harness.AssertAppSpanCounts(t, run, want, rv, 1)
		if countSampled(rates, rv) > 0 {
			full := harness.SpansOfRun(sink.Spans(), runID)
			require.Equal(t, rv, harness.AssertConsistentRV(t, full),
				"rv must equal the seeded value")
		}
	}
}

// TestNatsCoreMethods drives every span-emitting core-NATS interaction
// (Publish, PublishMsg, Subscribe, QueueSubscribe, Request/reply) through the
// same seeded-rv chain and asserts presence, rv consistency (including the
// wrapper spans via SpansOfRun), and the span-link topology.
func TestNatsCoreMethods(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)
	rates := []float64{0.9, 0.9}
	svcs, want := buildServices(t, url, endpoint, "svc", rates)
	rv := uint64(1) << 55

	for _, scen := range coreScenarios() {
		t.Run(scen.name, func(t *testing.T) {
			runID, run := driveChain(t, sink, svcs, rates, scen, rv)
			harness.AssertPresence(t, run, want, rv)
			full := waitLinkedRun(t, sink, runID, svcs[0].name, svcs[1].name)
			require.Equal(t, rv, harness.AssertConsistentRV(t, full))
			harness.AssertLinkedTrace(t, full, svcs[0].name, svcs[1].name)
		})
	}
}

// TestNatsJetStreamMethods is TestNatsCoreMethods for every span-emitting
// JetStream interaction: Publish/PublishMsg, pull Consume, Messages().Next,
// Consumer.Next, Fetch/FetchBytes/FetchNoWait, push Consume, OrderedConsumer.
func TestNatsJetStreamMethods(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)
	rates := []float64{0.9, 0.9}
	svcs, want := buildServices(t, url, endpoint, "svc", rates)
	rv := uint64(1) << 55

	for _, scen := range jetStreamScenarios() {
		t.Run(scen.name, func(t *testing.T) {
			runID, run := driveChain(t, sink, svcs, rates, scen, rv)
			harness.AssertPresence(t, run, want, rv)
			full := waitLinkedRun(t, sink, runID, svcs[0].name, svcs[1].name)
			require.Equal(t, rv, harness.AssertConsistentRV(t, full))
			harness.AssertLinkedTrace(t, full, svcs[0].name, svcs[1].name)
		})
	}
}

// TestNatsTraceEvents covers the last span-emitting path: ADR-41 infrastructure
// trace events (SubscribeTraceEvents → one INTERNAL "nats.<KIND>.<type>" span
// per hop). Unlike every consumer path, hop spans are parent-child children of
// the original producer span — the handler extracts traceparent/tracestate from
// the headers echoed in the event payload — so they land in the producer's
// trace and must carry the same rv. Requires the NATS 2.11+ server started by
// startNATS.
func TestNatsTraceEvents(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)

	const traceDest = "chain.tracedest"
	rates := []float64{0.9, 0.9}
	svcs, want := buildServices(t, url, endpoint, "svc", rates,
		otelnats.WithTraceDestination(traceDest))
	sub, err := otelnats.SubscribeTraceEvents(svcs[0].conn, traceDest)
	require.NoError(t, err, "subscribe trace events")
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	rv := uint64(1) << 55
	runID, run := driveChain(t, sink, svcs, rates, scenario{"traceevents", coreSubscribe}, rv)
	harness.AssertPresence(t, run, want, rv)

	prodTrace, ok := harness.TraceIDOf(run, svcs[0].name)
	require.True(t, ok, "producer app span missing")

	// The server publishes the trace event asynchronously after delivery.
	snapshot := sink.WaitFor(20*time.Second, func(ss []harness.Span) bool {
		return len(hopSpans(ss, prodTrace)) > 0
	})
	hops := hopSpans(snapshot, prodTrace)
	require.NotEmpty(t, hops, "no ADR-41 hop span (nats.*) arrived in the producer trace")

	// Hop spans share the producer trace, so SpansOfRun picks them up along
	// with the send/process wrapper spans; one rv must cover them all.
	full := harness.SpansOfRun(snapshot, runID)
	require.Equal(t, rv, harness.AssertConsistentRV(t, full))
}

// hopSpans returns the ADR-41 hop spans ("nats.<KIND>.<type>", emitted under
// the otelnats scope) belonging to the given trace.
func hopSpans(spans []harness.Span, traceID string) []harness.Span {
	out := make([]harness.Span, 0, 2)
	for _, s := range harness.SpansByScope(spans, otelnats.ScopeName) {
		if s.TraceID == traceID && strings.HasPrefix(s.Name, "nats.") {
			out = append(out, s)
		}
	}
	return out
}

// multiHopRates deliberately gives the MIDDLE service the lowest rate, so the
// nesting of consistent sampling (lower rate ⇒ higher threshold) yields an rv
// that drops svc1 while both of its neighbours are sampled. Thresholds are
// ≈(1-p)·2^56: 0.9→7.21e15, 0.1→6.49e16, 0.5→3.60e16.
var multiHopRates = []float64{0.9, 0.1, 0.5}

// multiHopLadder picks rv values with a wide margin from every threshold above.
var multiHopLadder = []struct {
	rv   uint64
	what string
}{
	{0, "none sampled"},
	{2 << 53, "head only"},                 // 1.80e16: ≥0.9's, <0.5's, <0.1's
	{5 << 53, "middle dropped, tail kept"}, // 4.50e16: ≥0.9's, ≥0.5's, <0.1's
	{(1 << 56) - 1, "all sampled"},
}

// TestNatsMultiHopChain drives a 3-service chain (two consecutive NATS hops)
// and asserts the seeded rv survives both. The load-bearing rung is
// "middle dropped, tail kept": svc1's own sampler drops it, so it exports no
// span at all, yet its SpanContext still carries the rv in tracestate — so
// svc2 must still observe exactly the rv seeded at svc0. That is the invariant
// making per-service sampling rates independent of one another.
func TestNatsMultiHopChain(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)
	svcs, want := buildServices(t, url, endpoint, "svc", multiHopRates)

	for _, rung := range multiHopLadder {
		t.Run(rung.what, func(t *testing.T) {
			runID, run := driveChain(t, sink, svcs, multiHopRates,
				scenario{"multihop", coreSubscribe}, rung.rv)
			harness.AssertAppSpanCounts(t, run, want, rung.rv, 1)

			n := countSampled(multiHopRates, rung.rv)
			if n == 0 {
				return
			}
			// One rv across every exported span of the run, equal to the seed —
			// even when a service in the middle contributed nothing.
			full := harness.SpansOfRun(sink.Spans(), runID)
			require.Equal(t, rung.rv, harness.AssertConsistentRV(t, full),
				"rv must survive both hops unchanged")

			if n == len(svcs) {
				// Fully sampled: both link hops are observable. (When the
				// middle is dropped, svc2 links to svc1's span-less trace, not
				// svc0's, so a svc0→svc2 link assertion would be wrong.)
				h0 := waitLinkedRun(t, sink, runID, svcs[0].name, svcs[1].name)
				harness.AssertLinkedTrace(t, h0, svcs[0].name, svcs[1].name)
				h1 := waitLinkedRun(t, sink, runID, svcs[1].name, svcs[2].name)
				harness.AssertLinkedTrace(t, h1, svcs[1].name, svcs[2].name)
			}
		})
	}

	// The same two-hop guarantee over JetStream, to show it is a property of the
	// propagation chain rather than of one transport.
	t.Run("jetstream all sampled", func(t *testing.T) {
		rv := uint64(1<<56) - 1
		runID, run := driveChain(t, sink, svcs, multiHopRates,
			scenario{"multihopjs", jsConsume}, rv)
		harness.AssertAppSpanCounts(t, run, want, rv, 1)
		full := harness.SpansOfRun(sink.Spans(), runID)
		require.Equal(t, rv, harness.AssertConsistentRV(t, full))
	})
}

// TestNatsRequestAmbientRoot pins the documented asymmetry of the request
// family: Request and RequestMsg have no ctx parameter, so the wrapper roots
// their CLIENT span at context.Background() (conn_traced.go) and the caller's
// trace is deliberately NOT chained — unlike RequestWithContext /
// RequestMsgWithContext, which TestNatsCoreMethods covers. Both variants still
// emit the same two wrapper spans, which this test also confirms.
//
// Rates are 1.0 so both services export regardless of randomness: the
// responder's rv comes from the request span's freshly generated value, not the
// seeded one, so countSampled(rates, seededRV) could not predict its presence.
func TestNatsRequestAmbientRoot(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)
	rates := []float64{1.0, 1.0}
	svcs, _ := buildServices(t, url, endpoint, "svc", rates)
	rv := uint64(1) << 55

	for _, scen := range requestAmbientScenarios() {
		t.Run(scen.name, func(t *testing.T) {
			runID, run := driveChain(t, sink, svcs, rates, scen, rv)
			require.Len(t, run, 2, "both services export at rate 1.0")

			// Deterministic: the responder neither joins nor links back to the
			// seeded trace, because the request span started a new root.
			harness.AssertTraceNotContinued(t, run, svcs[0].name, svcs[1].name)
			// Consequently the seeded randomness does not reach the responder.
			require.Greater(t, len(harness.DistinctRVs(run)), 1,
				"ambient-rooted request must not carry the seeded rv across")

			// Both request-path span sites still fire: the requester's
			// "<subject> request" CLIENT span and the reply "receive" span.
			// Their trace is reachable only through the responder's link, which
			// arrives asynchronously — so wait for them rather than sampling
			// the sink once.
			full := waitRunSpans(t, sink, runID, func(ss []harness.Span) bool {
				req, recv := requestSpanRoles(harness.SpansByScope(ss, otelnats.ScopeName))
				return req && recv
			})
			wrapper := harness.SpansByScope(full, otelnats.ScopeName)
			sawRequest, sawReceive := requestSpanRoles(wrapper)
			require.True(t, sawRequest, "no request span in %v", spanNames(wrapper))
			require.True(t, sawReceive, "no reply receive span in %v", spanNames(wrapper))
		})
	}
}

// requestSpanRoles reports whether the wrapper spans include the requester's
// "<subject> request" CLIENT span and the reply "receive <inbox>" span.
func requestSpanRoles(wrapper []harness.Span) (request, receive bool) {
	for _, s := range wrapper {
		switch {
		case strings.HasSuffix(s.Name, " request"):
			request = true
		case strings.HasPrefix(s.Name, "receive "):
			receive = true
		}
	}
	return request, receive
}

// spanNames lists span names for failure messages.
func spanNames(spans []harness.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}

// TestNatsSamplingRate drives one run per evenly-spaced rv at the env-provided
// rate (OTEL_TRACES_SAMPLER_ARG) and checks the all-or-none invariant per run
// plus the sampled fraction.
//
// The rv values come from harness.UniformRVs rather than RandomRV: with a
// random draw the fraction is binomial, so at m=40 a meaningful tolerance sits
// around 2.5σ and the line flakes roughly 1% of every CI run. Sweeping the
// randomness space evenly makes the fraction land within 1/m of the rate every
// time while still failing on a wrong threshold.
func TestNatsSamplingRate(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)
	rate := harness.EnvSamplerArg(0.5)
	rates := []float64{rate, rate}
	svcs, _ := buildServices(t, url, endpoint, "svc", rates)

	const m = 40
	rvs := harness.UniformRVs(m)
	runs := make([][]harness.Span, 0, m)
	for _, rv := range rvs {
		_, run := driveChain(t, sink, svcs, rates, scenario{"rate", coreSubscribe}, rv)
		// Same rate + same rv ⇒ the two services must agree: 0 or 2 app spans.
		require.Contains(t, []int{0, 2}, len(run), "run must be all-or-none")
		if len(run) > 0 {
			harness.AssertConsistentRV(t, run)
		}
		runs = append(runs, run)
	}
	harness.AssertSampledFraction(t, runs, svcs[0].name, rate, 2.0/m)
}

// TestNatsFullSpanShape pins the exact span shape of one publish→subscribe
// hop: producer app span + wrapper PRODUCER "send" span in the seeded trace;
// wrapper CONSUMER "process" span + consumer app span in a new trace linked to
// the producer's — all carrying the seeded rv.
func TestNatsFullSpanShape(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	if !exp.PropagationEnabled {
		t.Skip("propagation disabled by env matrix; covered by TestNatsSamplingSuite")
	}
	sink, endpoint, url := setup(t)
	rates := []float64{0.9, 0.5}
	svcs, _ := buildServices(t, url, endpoint, "svc", rates)
	rv := uint64(3) << 54 // sampled by both 0.9 and 0.5, far from both thresholds

	runID, _ := driveChain(t, sink, svcs, rates, scenario{"fullshape", coreSubscribe}, rv)

	// Wait until both wrapper spans (producer send, consumer process) arrive.
	snapshot := sink.WaitFor(20*time.Second, func(ss []harness.Span) bool {
		w := harness.SpansByScope(ss, otelnats.ScopeName)
		return len(harness.SpansByService(w, svcs[0].name)) > 0 &&
			len(harness.SpansByService(w, svcs[1].name)) > 0
	})

	full := harness.SpansOfRun(snapshot, runID)
	require.Equal(t, rv, harness.AssertConsistentRV(t, full))
	// Every span on this path is expected to carry the seeded rv, so assert the
	// strict form: AssertConsistentRV alone would pass even if one hop lost it.
	harness.AssertAllSpansCarryRV(t, full, rv)

	wrapper := harness.SpansByScope(full, otelnats.ScopeName)
	require.Len(t, harness.SpansByService(wrapper, svcs[0].name), 1, "producer: one send span")
	require.Len(t, harness.SpansByService(wrapper, svcs[1].name), 1, "consumer: one process span")
	apps := harness.SpansByAttr(full, harness.RunAttr, runID)
	require.Len(t, harness.SpansByService(apps, svcs[0].name), 1, "producer: one app span")
	require.Len(t, harness.SpansByService(apps, svcs[1].name), 1, "consumer: one app span")

	// Producer app + send share the seeded trace; the consumer's process span
	// roots a new trace (with its app child) linked back to the producer.
	harness.AssertSameTrace(t, harness.SpansByService(full, svcs[0].name))
	harness.AssertSameTrace(t, harness.SpansByService(full, svcs[1].name))
	harness.AssertLinkedTrace(t, full, svcs[0].name, svcs[1].name)
}

// TestNatsTracingToggleInProcess exercises the per-connection
// WithTracingEnabled option in both directions within a single process: an "on"
// pair must behave exactly like the all-on suite, and an "off" pair must emit no
// wrapper spans and propagate nothing, side by side.
//
// Since 0.8.0 the option is the ladder's third rung — below both the master and
// the module environment variable — so it decides only when neither has an
// opinion. The test clears both for its duration, which is also what makes it
// meaningful in every row of the matrix: the row's values are precisely what the
// option is no longer allowed to override, so leaving them set would test the
// row, not the option. Nothing in this package runs in parallel, so mutating the
// process environment here is safe.
func TestNatsTracingToggleInProcess(t *testing.T) {
	unsetEnv(t, otelflags.EnvGlobalTracing)
	unsetEnv(t, envNATSTracingEnabled)

	sink, endpoint, url := setup(t)
	rates := []float64{1.0, 1.0}
	rv := uint64(1) << 55

	// Option-on pair: full consistent-sampling behavior.
	onSvcs, _ := buildServices(t, url, endpoint, "on", rates, otelnats.WithTracingEnabled(true))
	onRunID, onRun := driveChain(t, sink, onSvcs, rates, scenario{"toggle-on", coreSubscribe}, rv)
	require.Len(t, onRun, 2)
	onFull := waitLinkedRun(t, sink, onRunID, onSvcs[0].name, onSvcs[1].name)
	require.Equal(t, rv, harness.AssertConsistentRV(t, onFull))
	harness.AssertLinkedTrace(t, onFull, onSvcs[0].name, onSvcs[1].name)
	require.NotEmpty(t, harness.SpansByScope(onFull, otelnats.ScopeName),
		"forced-on pair must emit wrapper spans")

	// Option-off pair: app spans flow, but no wrapper spans and no propagation.
	offSvcs, _ := buildServices(t, url, endpoint, "off", rates, otelnats.WithTracingEnabled(false))
	_, offRun := driveChain(t, sink, offSvcs, rates, scenario{"toggle-off", coreSubscribe}, rv)
	require.Len(t, offRun, 2)
	offSpans := harness.SpansByServicePrefix(sink.Spans(), "off")
	harness.AssertNoWrapperSpans(t, offSpans, otelnats.ScopeName)
	require.Greater(t, len(harness.DistinctRVs(offRun)), 1,
		"forced-off pair: rv must NOT propagate to the consumer")
}
