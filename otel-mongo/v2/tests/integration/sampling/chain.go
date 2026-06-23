// Package sampling verifies otel-mongo/v2 consistent probabilistic sampling
// end-to-end against a real MongoDB + OTel Collector, using the otel-testkit
// harness as a black-box toolkit.
//
// There is no harness "plugin": each service is a normal otelmongo Client whose
// TracerProvider exports to the harness collector. We seed a known randomness
// value at the head (harness.SeedContextRV), drive a realistic produce→consume
// flow through the instrumented driver (the wrapper injects the trace into the
// "_oteltrace" field on write and extracts it on read), and assert on the spans
// collected at the sink. The parent-child / span-link topology comes from which
// read API the consumer uses, not from the harness.
//
// It lives in its own package (not integration_test, whose TestMain force-enables
// every flag) so the harness can observe the feature-flag environment set by the
// matrix in the Makefile/CI.
package sampling

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	otelmongo "github.com/akira-core/instrumentation-go/otel-mongo/v2"
	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
)

// gate names the feature-flag env vars otel-mongo/v2 reads; ExpectationFromEnv
// turns the current matrix row into the behavior the test should assert.
var gate = harness.GateEnv{
	Global:      "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED",
	Tracing:     "OTEL_MONGO_TRACING_ENABLED",
	Propagation: "OTEL_MONGO_PROPAGATION_ENABLED",
}

// setup starts the in-process sink + collector and points exporters (including
// the wrapper's deliver-span TracerProvider) at it; returns sink + collector
// endpoint + mongo URI.
func setup(t *testing.T) (*harness.Sink, string, string) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	ctx := context.Background()
	sink := harness.StartSink(t)
	harness.DumpOnFailure(t, sink) // dump every collected span if the test fails
	endpoint := harness.StartCollector(ctx, t, sink.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	// The harness collector speaks plaintext gRPC. The wrapper's deliver-span
	// exporter is built from OTEL_EXPORTER_OTLP_ENDPOINT (a bare host:port → gRPC)
	// and otherwise defaults to TLS; this tells it to connect insecurely.
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	return sink, endpoint, startMongo(ctx, t)
}

// startMongo runs a single-node replica-set MongoDB container (replica set is
// required for change streams) and returns a direct-connection URI.
func startMongo(ctx context.Context, t *testing.T) string {
	t.Helper()
	container, err := tcmongo.Run(ctx, "mongo:7.0", tcmongo.WithReplicaSet("rs0"))
	require.NoError(t, err, "start mongo")
	t.Cleanup(func() {
		tctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = container.Terminate(tctx)
	})
	uri, err := container.ConnectionString(ctx)
	require.NoError(t, err, "mongo connection string")

	// The testcontainers module initiates the replica set with the member host
	// set to the container-internal IP, and ConnectionString appends
	// "replicaSet=rs0" — which makes the driver do replica-set discovery and try
	// to reach that internal IP (unreachable from the host). Connect directly to
	// the mapped port instead: the single node is the replica-set primary, so
	// reads, writes, and change streams all work.
	u, err := url.Parse(uri)
	require.NoError(t, err, "parse mongo uri")
	q := u.Query()
	q.Del("replicaSet")
	q.Set("directConnection", "true")
	u.RawQuery = q.Encode()
	return u.String()
}

// service is one instrumented MongoDB endpoint bound to its own service TP.
type service struct {
	name   string
	coll   *otelmongo.Collection
	tracer trace.Tracer
}

// buildServices creates one otelmongo Client per rate, each with a
// consistent-sampler TracerProvider exporting to endpoint, all sharing one
// collection. Returns the services and the service-name→rate map for
// AssertPresence.
func buildServices(t *testing.T, uri, endpoint string, rates []float64) ([]service, map[string]float64) {
	t.Helper()
	svcs := make([]service, len(rates))
	want := make(map[string]float64, len(rates))
	for i, r := range rates {
		name := fmt.Sprintf("svc%d", i)
		want[name] = r
		tp := harness.BuildTracerProvider(t, name, harness.ConsistentSampler(r), endpoint)
		client, err := otelmongo.NewClient(uri, otelmongo.WithTracerProvider(tp))
		require.NoError(t, err, "connect %s", name)
		t.Cleanup(func() {
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.Disconnect(dctx)
		})
		svcs[i] = service{name: name, coll: client.Database("testkit").Collection("chain"), tracer: tp.Tracer("chain")}
	}
	return svcs, want
}

// runAttr tags an application span so the harness can group one logical run.
func runAttr(runID string) trace.SpanStartOption { return harness.RunAttrOption(runID) }

// topo selects how a consumer attaches its span to the previous hop. The choice
// reflects how the consuming application uses the library (continue vs link); the
// harness does not impose it.
type topo int

const (
	parentChild topo = iota // continue the extracted trace (same trace ID)
	spanLink                // new root linked to the extracted trace
)

// allPC returns a hops slice of n parent-child edges.
func allPC(n int) []topo { return make([]topo, n) }

// driveChain drives one trace through the services. Edge i (svc i → svc i+1)
// uses hops[i]: parentChild continues the extracted trace (SingleResult.
// TraceContext), spanLink starts a new root linked to it. Each service reads the
// previous hop's document with FindOne and writes the next hop. Returns the run
// id and the run's application spans (filtered by RunAttr).
func driveChain(t *testing.T, sink *harness.Sink, svcs []service, rates []float64, hops []topo, rv uint64) (string, []harness.Span) {
	t.Helper()
	runID := uuid.NewString()
	ctx := context.Background()

	// Head: start svc0's span from the seeded randomness, then produce hop 0.
	c0, s0 := svcs[0].tracer.Start(harness.SeedContextRV(rv), svcs[0].name, runAttr(runID))
	_, err := svcs[0].coll.InsertOne(c0, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}, {Key: "payload", Value: "x"}})
	require.NoError(t, err, "svc0 insert")
	s0.End()

	for i := 1; i < len(svcs); i++ {
		sr := svcs[i].coll.FindOne(ctx, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: i - 1}})
		require.NoError(t, sr.Err(), "%s find hop %d", svcs[i].name, i-1)

		var ci context.Context
		var si trace.Span
		if hops[i-1] == spanLink {
			// New root linked to the producer — span-link topology.
			linkSC := trace.SpanContextFromContext(sr.TraceContext())
			ci, si = svcs[i].tracer.Start(context.Background(), svcs[i].name,
				runAttr(runID), trace.WithLinks(trace.Link{SpanContext: linkSC}))
		} else {
			// Continuation (parent-child).
			ci, si = svcs[i].tracer.Start(sr.TraceContext(), svcs[i].name, runAttr(runID))
		}
		if i < len(svcs)-1 {
			_, err := svcs[i].coll.InsertOne(ci, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: i}, {Key: "payload", Value: "x"}})
			require.NoError(t, err, "%s insert", svcs[i].name)
		}
		si.End()
	}

	return runID, waitRun(t, sink, runID, countSampled(rates, rv))
}

// driveFindChain drives an all-parent-child chain and returns just the run's
// application spans.
func driveFindChain(t *testing.T, sink *harness.Sink, svcs []service, rates []float64, rv uint64) []harness.Span {
	t.Helper()
	_, run := driveChain(t, sink, svcs, rates, allPC(len(svcs)-1), rv)
	return run
}

// waitRun blocks until at least wantCount application spans for runID arrive,
// then returns them (failing with a span dump on timeout).
func waitRun(t *testing.T, sink *harness.Sink, runID string, wantCount int) []harness.Span {
	t.Helper()
	return harness.WaitForAppSpans(t, sink, runID, wantCount, 20*time.Second)
}

func countSampled(rates []float64, rv uint64) int { return harness.CountSampled(rates, rv) }
