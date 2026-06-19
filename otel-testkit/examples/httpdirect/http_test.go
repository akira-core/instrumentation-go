// Package httpdirect is a black-box reference test showing how to verify
// consistent probabilistic sampling end-to-end over a point-to-point HTTP
// transport using the otel-testkit harness as a toolkit.
//
// It is a template: copy the shape for any instrumentation library. There is no
// harness "plugin" to implement — each service is a normal instrumented HTTP
// server, its TracerProvider exports to the harness collector, and we assert on
// the spans collected at the sink. Trace propagation (inject on the outgoing
// request, extract on the incoming one) and the parent-child topology come from
// the HTTP instrumentation itself; the harness only seeds a known randomness
// value at the head and asserts the invariant.
//
// Here the "instrumentation" is the standard W3C TraceContext propagator over
// net/http (which carries the consistent-sampling rv in tracestate); a real
// otelhttp wrapper plugs in the same way and additionally emits its own
// client/server spans.
package httpdirect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
)

// service is one instrumented HTTP service in the chain. On each request it
// continues the incoming trace, starts its application span, and (if it has a
// successor) forwards to it — exactly what a real microservice handler does.
type service struct {
	name   string
	tracer trace.Tracer
	next   string // successor base URL; "" for the last service
}

func (s *service) handle(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run")
	path := r.URL.Path

	// Extract the upstream trace (with tracestate rv) — a real HTTP wrapper does
	// this in its server middleware, populating the handler's request context.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	// Start this service's application span as a continuation (parent-child).
	ctx, span := s.tracer.Start(ctx, s.name,
		trace.WithAttributes(attribute.String(harness.RunAttr, runID)))
	defer span.End()

	if s.next != "" {
		req, _ := http.NewRequestWithContext(ctx, r.Method, s.next+path+"?run="+runID, nil)
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header)) // wrapper injects on send
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	w.WriteHeader(http.StatusOK)
}

// startChain brings up len(rates) instrumented services wired head→tail, each
// with its own consistent-sampler TracerProvider exporting to endpoint. Returns
// the head URL and the service-name→rate map for AssertPresence.
func startChain(t *testing.T, endpoint string, rates []float64) (headURL string, want map[string]float64) {
	t.Helper()
	want = make(map[string]float64, len(rates))
	next := "" // build back-to-front so each service knows its successor's URL
	for i := len(rates) - 1; i >= 0; i-- {
		name := serviceName(i)
		want[name] = rates[i]
		tp := harness.BuildTracerProvider(t, name, harness.ConsistentSampler(rates[i]), endpoint)
		svc := &service{name: name, tracer: tp.Tracer("chain"), next: next}
		srv := httptest.NewServer(http.HandlerFunc(svc.handle))
		t.Cleanup(srv.Close)
		next = srv.URL
	}
	return next, want
}

func serviceName(i int) string { return "svc" + string(rune('0'+i)) }

// drive sends one request through the chain at path with a seeded rv and returns
// the spans collected for that run.
func drive(t *testing.T, sink *harness.Sink, headURL, path string, rv uint64, wantCount int) []harness.Span {
	t.Helper()
	runID := uuid.NewString()

	head := harness.SeedContextRV(rv) // synthetic inbound carrier holding rv
	req, err := http.NewRequestWithContext(head, http.MethodGet, headURL+path+"?run="+runID, nil)
	require.NoError(t, err)
	otel.GetTextMapPropagator().Inject(head, propagation.HeaderCarrier(req.Header))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	spans := sink.WaitFor(15*time.Second, func(ss []harness.Span) bool {
		return len(harness.SpansByAttr(ss, harness.RunAttr, runID)) >= wantCount
	})
	return harness.SpansByAttr(spans, harness.RunAttr, runID)
}

func startSinkAndCollector(t *testing.T) (*harness.Sink, string) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	sink := harness.StartSink(t)
	endpoint := harness.StartCollector(context.Background(), t, sink.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	return sink, endpoint
}

// TestHTTPSamplingConsistency drives an rv ladder through a 3-service HTTP chain
// and asserts the core invariant at each step: a service appears iff its rate
// samples the rv, and every present span carries the same rv.
func TestHTTPSamplingConsistency(t *testing.T) {
	sink, endpoint := startSinkAndCollector(t)
	rates := []float64{0.9, 0.5, 0.1}
	headURL, want := startChain(t, endpoint, rates)

	// rv ladder → expected present sets {}, {svc0}, {svc0,svc1}, {svc0,svc1,svc2}.
	for _, rv := range []uint64{0, 1 << 53, 1 << 55, (1 << 56) - 1} {
		wantCount := countSampled(rates, rv)
		run := drive(t, sink, headURL, "/a", rv, wantCount)
		harness.AssertAppSpanCounts(t, run, want, rv, 1) // exactly one span per sampled service
		if wantCount > 0 {
			harness.AssertConsistentRV(t, run)
			harness.AssertSameTrace(t, run) // HTTP chain is all parent-child → one trace ID
		}
	}
}

// TestHTTPDifferentEndpoint shows that a different delivery method (here a
// different request path) plugs into the exact same harness assertions — no harness
// change. For pull-based libraries (e.g. mongo) different methods can also yield
// a different topology; for HTTP the topology stays parent-child.
func TestHTTPDifferentEndpoint(t *testing.T) {
	sink, endpoint := startSinkAndCollector(t)
	rates := []float64{0.9, 0.9}
	headURL, want := startChain(t, endpoint, rates)

	rv := uint64(1) << 55
	run := drive(t, sink, headURL, "/b", rv, len(rates))
	harness.AssertAppSpanCounts(t, run, want, rv, 1)
	harness.AssertConsistentRV(t, run)
	harness.AssertSameTrace(t, run)
}

func countSampled(rates []float64, rv uint64) int {
	n := 0
	for _, r := range rates {
		if harness.ExpectedSampled(r, rv) {
			n++
		}
	}
	return n
}
