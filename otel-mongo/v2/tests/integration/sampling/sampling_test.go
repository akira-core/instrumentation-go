package sampling

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel/trace"

	otelmongo "github.com/akira-core/instrumentation-go/otel-mongo/v2"
	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
)

// TestMongoSamplingSuite runs the consistent-sampling E2E checks against a real
// MongoDB + OTel Collector. Behavior is dictated by the feature-flag matrix (set
// via env by the Makefile/CI), so this single test covers every flag state when
// invoked across the matrix.
func TestMongoSamplingSuite(t *testing.T) {
	exp := harness.ExpectationFromEnv(gate)
	sink, endpoint, uri := setup(t)

	switch {
	case !exp.TracingEnabled:
		// Tracing off: the wrapper must emit no spans, but the application's own
		// spans still flow to the collector.
		svcs, _ := buildServices(t, uri, endpoint, []float64{1.0, 1.0})
		run := driveFindChain(t, sink, svcs, []float64{1.0, 1.0}, 1<<55)
		require.NotEmpty(t, run, "application spans should still flow")
		harness.AssertNoWrapperSpans(t, sink.Spans(), otelmongo.ScopeName)

	case !exp.PropagationEnabled:
		// Propagation off: nothing carries rv across the "_oteltrace" field, so
		// each service derives its own randomness → rv is not shared.
		rates := []float64{1.0, 1.0, 1.0}
		svcs, _ := buildServices(t, uri, endpoint, rates)
		run := driveFindChain(t, sink, svcs, rates, 1<<55)
		require.Len(t, run, 3, "all services sample at rate 1.0")
		require.Greater(t, len(harness.DistinctRVs(run)), 1,
			"propagation disabled: services should not share a single rv")

	default:
		// Tracing + propagation on: the core consistency invariant across an rv
		// ladder → expected present sets {}, {svc0}, {svc0,svc1}, {svc0,svc1,svc2}.
		rates := []float64{0.9, 0.5, 0.1}
		svcs, want := buildServices(t, uri, endpoint, rates)
		for _, rv := range []uint64{0, 1 << 53, 1 << 55, (1 << 56) - 1} {
			run := driveFindChain(t, sink, svcs, rates, rv)
			harness.AssertAppSpanCounts(t, run, want, rv, 1) // exactly one app span per sampled node
			if countSampled(rates, rv) > 0 {
				harness.AssertConsistentRV(t, run)
				harness.AssertSameTrace(t, run) // all parent-child → one trace ID
			}
		}
	}
}

// TestMongoAggregateDelivery shows a different read command (Aggregate +
// Cursor.DecodeAndTrace) carrying the trace: the same harness assertions hold
// without any harness change. DecodeAndTrace links the consumer to the origin
// (a new trace, span-link topology), so the rv still propagates consistently.
func TestMongoAggregateDelivery(t *testing.T) {
	if !harness.ExpectationFromEnv(gate).PropagationEnabled {
		t.Skip("needs mongo tracing + propagation enabled")
	}
	sink, endpoint, uri := setup(t)
	rates := []float64{0.9, 0.9}
	svcs, want := buildServices(t, uri, endpoint, rates)
	rv := uint64(1) << 55
	runID := uuid.NewString()
	ctx := context.Background()

	c0, s0 := svcs[0].tracer.Start(harness.SeedContextRV(rv), svcs[0].name, runAttr(runID))
	_, err := svcs[0].coll.InsertOne(c0, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}, {Key: "payload", Value: "x"}})
	require.NoError(t, err)
	s0.End()

	cur, err := svcs[1].coll.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}}}},
	})
	require.NoError(t, err)
	require.True(t, cur.Next(ctx), "aggregate should return the document")
	var decoded bson.M
	rctx, err := cur.DecodeAndTrace(ctx, &decoded)
	require.NoError(t, err)
	require.NoError(t, cur.Close(ctx))

	_, s1 := svcs[1].tracer.Start(rctx, svcs[1].name, runAttr(runID))
	s1.End()

	run := waitRun(t, sink, runID, 2)
	harness.AssertPresence(t, run, want, rv)
	// The link from svc1's trace to svc0's lives on the wrapper's
	// "mongo.cursor.decode" span (no RunAttr), so assert over the full run
	// expansion, not just the app spans.
	full := harness.SpansOfRun(sink.Spans(), runID)
	harness.AssertConsistentRV(t, full)                            // rv survives the aggregate/decode link path
	harness.AssertLinkedTrace(t, full, svcs[0].name, svcs[1].name) // DecodeAndTrace links svc1 to svc0's trace
}

// TestMongoManualSpanLinkConsistency shows the span-link delivery topology
// built by hand: the consumer reads with FindOne and starts a new root linked
// to the extracted producer context, rather than continuing it. This exercises
// the sampler's single-link seed path — it does NOT exercise the wrapper's
// ChangeStream machinery; TestMongoChangeStreamSpanLinkConsistency covers that.
// The consistent sampler reads rv from the link seed, so the same rv still
// propagates and the invariant holds across a new trace ID.
func TestMongoManualSpanLinkConsistency(t *testing.T) {
	if !harness.ExpectationFromEnv(gate).PropagationEnabled {
		t.Skip("needs mongo tracing + propagation enabled")
	}
	sink, endpoint, uri := setup(t)
	rates := []float64{0.9, 0.9}
	svcs, want := buildServices(t, uri, endpoint, rates)
	rv := uint64(1) << 55
	runID := uuid.NewString()
	ctx := context.Background()

	c0, s0 := svcs[0].tracer.Start(harness.SeedContextRV(rv), svcs[0].name, runAttr(runID))
	_, err := svcs[0].coll.InsertOne(c0, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}, {Key: "payload", Value: "x"}})
	require.NoError(t, err)
	s0.End()

	sr := svcs[1].coll.FindOne(ctx, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}})
	require.NoError(t, sr.Err())
	remoteSC := trace.SpanContextFromContext(sr.TraceContext())
	// New root linked to the producer, not a child — span-link topology.
	_, s1 := svcs[1].tracer.Start(context.Background(), svcs[1].name,
		runAttr(runID), trace.WithLinks(trace.Link{SpanContext: remoteSC}))
	s1.End()

	run := waitRun(t, sink, runID, 2)
	harness.AssertPresence(t, run, want, rv)
	harness.AssertConsistentRV(t, run)                            // same rv across the link
	harness.AssertLinkedTrace(t, run, svcs[0].name, svcs[1].name) // svc1 really links to svc0's trace
}

// TestMongoChangeStreamSpanLinkConsistency drives the wrapper's real async
// change-stream path: svc1 opens Collection.Watch, svc0 inserts, and svc1 reads
// the change with ChangeStream.DecodeAndTrace. That call extracts "_oteltrace"
// from the change document and hands back a context linked to the producer, so
// the consumer's span is a new root carrying the producer's rv.
//
// Unlike TestMongoManualSpanLinkConsistency, the link here is built by the
// library, not by the test — this is what proves the wrapper's change-stream
// extraction and link construction stay consistent under sampling.
func TestMongoChangeStreamSpanLinkConsistency(t *testing.T) {
	if !harness.ExpectationFromEnv(gate).PropagationEnabled {
		t.Skip("needs mongo tracing + propagation enabled")
	}
	sink, endpoint, uri := setup(t)
	rates := []float64{0.9, 0.9}
	svcs, want := buildServices(t, uri, endpoint, rates)
	rv := uint64(1) << 55
	runID := uuid.NewString()
	ctx := context.Background()

	// Open the change stream before inserting so the insert is observed.
	cs, err := svcs[1].coll.Watch(ctx, mongo.Pipeline{})
	require.NoError(t, err, "watch")
	defer func() { _ = cs.Close(ctx) }()

	c0, s0 := svcs[0].tracer.Start(harness.SeedContextRV(rv), svcs[0].name, runAttr(runID))
	_, err = svcs[0].coll.InsertOne(c0, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}, {Key: "payload", Value: "x"}})
	require.NoError(t, err)
	s0.End()

	// Next blocks until the insert shows up on the stream.
	nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	require.True(t, cs.Next(nctx), "change stream should deliver the insert: %v", cs.Err())

	var change bson.M
	rctx, err := cs.DecodeAndTrace(ctx, &change)
	require.NoError(t, err, "decode and trace")

	_, s1 := svcs[1].tracer.Start(rctx, svcs[1].name, runAttr(runID))
	s1.End()

	run := waitRun(t, sink, runID, 2)
	harness.AssertPresence(t, run, want, rv)
	// The library-built link lives on the wrapper's change-stream decode span
	// (no RunAttr), so assert over the full run expansion.
	full := harness.SpansOfRun(sink.Spans(), runID)
	harness.AssertConsistentRV(t, full)
	harness.AssertLinkedTrace(t, full, svcs[0].name, svcs[1].name)
}

// TestMongoTopologyIndependence drives a 3-node chain under every combination of
// parent-child / span-link edges and asserts the sampling decision is topology
// independent: the last node reads the same randomness the head seeded, and both
// the set of sampled nodes and the rv are identical across all four topologies
// (including rv surviving a dropped middle node).
func TestMongoTopologyIndependence(t *testing.T) {
	if !harness.ExpectationFromEnv(gate).PropagationEnabled {
		t.Skip("needs mongo tracing + propagation enabled")
	}
	sink, endpoint, uri := setup(t)
	rates := []float64{0.9, 0.5, 0.1}
	svcs, want := buildServices(t, uri, endpoint, rates)

	topologies := [][]topo{
		{parentChild, parentChild},
		{parentChild, spanLink},
		{spanLink, parentChild},
		{spanLink, spanLink},
	}
	// rv ladder → expected present sets {}, {svc0}, {svc0,svc1}, {svc0,svc1,svc2}.
	for _, rv := range []uint64{0, 1 << 53, 1 << 55, (1 << 56) - 1} {
		var refPresent map[string]bool
		var refRV uint64
		haveRef := false
		for ti, hops := range topologies {
			_, run := driveChain(t, sink, svcs, rates, hops, rv)
			harness.AssertAppSpanCounts(t, run, want, rv, 1)
			present := presentSet(run, want)

			if countSampled(rates, rv) > 0 {
				got := harness.AssertConsistentRV(t, run)
				require.Equalf(t, rv, got, "topology %d rv=%x: last node rv != head rv", ti, rv)
				if haveRef {
					require.Equalf(t, refRV, got, "topology %d rv=%x: rv differs from first topology", ti, rv)
				} else {
					refRV, haveRef = got, true
				}
			}
			if ti == 0 {
				refPresent = present
			} else {
				require.Equalf(t, refPresent, present, "topology %d rv=%x: present set differs", ti, rv)
			}
		}
	}
}

// TestMongoSamplingRate drives one trace per evenly-spaced rv at a single rate
// (read from OTEL_TRACES_SAMPLER_ARG) and checks the sampled fraction matches
// the rate, with each trace consistent (all three nodes sampled, or none).
//
// The rv values come from harness.UniformRVs rather than RandomRV: with a
// random draw the fraction is binomial, so at m=40 a meaningful tolerance sits
// around 2.5σ and the line flakes roughly 1% of every CI run. Sweeping the
// randomness space evenly makes the fraction land within 1/m of the rate every
// time while still failing on a wrong threshold.
func TestMongoSamplingRate(t *testing.T) {
	if !harness.ExpectationFromEnv(gate).PropagationEnabled {
		t.Skip("needs mongo tracing + propagation enabled")
	}
	sink, endpoint, uri := setup(t)
	rate := harness.EnvSamplerArg(0.5)
	rates := []float64{rate, rate, rate}
	svcs, _ := buildServices(t, uri, endpoint, rates)

	const m = 40
	runs := make([][]harness.Span, 0, m)
	for _, rv := range harness.UniformRVs(m) {
		run := driveFindChain(t, sink, svcs, rates, rv)
		require.Containsf(t, []int{0, 3}, len(run), "single rate → all or none (got %d)", len(run))
		if len(run) > 0 {
			harness.AssertConsistentRV(t, run)
		}
		runs = append(runs, run)
	}
	harness.AssertSampledFraction(t, runs, "svc0", rate, 2.0/m)
}

// TestMongoFullSpanShape verifies the full set of spans the producer→consumer
// trace produces — the producer's application span, its wrapper CLIENT span, and
// the consumer's continuation app span — all share one randomness, with counts
// matching. Producer and consumer use different node rates.
//
// Note: the consumer's read (FindOne) emits its own CLIENT span in a separate
// trace (the read runs with its own context), so it is not part of the logical
// producer→consumer run.
func TestMongoFullSpanShape(t *testing.T) {
	if !harness.ExpectationFromEnv(gate).PropagationEnabled {
		t.Skip("needs mongo tracing + propagation enabled")
	}
	sink, endpoint, uri := setup(t)
	rp, rc := 0.9, 0.5 // different node rates; both sampled at the chosen rv
	svcs, _ := buildServices(t, uri, endpoint, []float64{rp, rc})
	rv := uint64(3) << 54 // ≈0.75·2^56, clear of every threshold below

	runID, _ := driveChain(t, sink, svcs, []float64{rp, rc}, allPC(1), rv)

	// Wait for the producer's wrapper client span to settle at the sink.
	snapshot := sink.WaitFor(20*time.Second, func(ss []harness.Span) bool {
		f := harness.SpansOfRun(ss, runID)
		producerClient := harness.SpansByService(harness.SpansByScope(f, otelmongo.ScopeName), svcs[0].name)
		return len(producerClient) >= 1
	})
	full := harness.SpansOfRun(snapshot, runID)

	// Producer app + client AND the consumer's continuation all share rv.
	require.Equal(t, rv, harness.AssertConsistentRV(t, full))
	// Every span on this path is expected to carry the seeded rv, so assert the
	// strict form: AssertConsistentRV alone would pass even if one hop lost it.
	harness.AssertAllSpansCarryRV(t, full, rv)

	wrapper := harness.SpansByScope(full, otelmongo.ScopeName)
	appSpans := harness.SpansByAttr(full, harness.RunAttr, runID)
	// producer node (rp=0.9, sampled): one app + one wrapper client span.
	require.Len(t, harness.SpansByService(appSpans, svcs[0].name), 1, "producer app span")
	require.Len(t, harness.SpansByService(wrapper, svcs[0].name), 1, "producer client span")
	// consumer node (rc=0.5, sampled): the continuation app span is in the run trace;
	// its read (find) client span lives in a separate trace, not here.
	require.Len(t, harness.SpansByService(appSpans, svcs[1].name), 1, "consumer continuation app span")
	require.Empty(t, harness.SpansByService(wrapper, svcs[1].name), "consumer read span is a separate trace")
}

// presentSet reports which of the named services have ≥1 span in run.
func presentSet(run []harness.Span, want map[string]float64) map[string]bool {
	out := make(map[string]bool, len(want))
	for name := range want {
		out[name] = len(harness.SpansByService(run, name)) > 0
	}
	return out
}
