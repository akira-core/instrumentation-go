// Package httpdirectstdlib is the Core (sampler-agnostic) reference template:
// it verifies feature behavior end-to-end for an instrumentation that does NOT
// use our consistent sampler — here the plain stdlib W3C TraceContext propagator
// with a standard sdktrace.TraceIDRatioBased sampler that never writes "ot=rv:".
//
// It is the counterpart to examples/httpdirect (which uses ConsistentSampler and
// asserts the rv invariant). Because a stdlib sampler emits no rv tracestate, the
// assertions here read only the trace topology — TraceID and span links — plus
// statistical sampled-fraction. They work for any sampler.
//
// Sampler choice — why plain TraceIDRatioBased, not ParentBased:
//
//	sdktrace.TraceIDRatioBased derives its keep/drop decision deterministically
//	from the trace ID, so services that share a trace ID make a *consistent*
//	(nested) decision across the chain — the stdlib analog of consistent
//	sampling, just without the ot=rv: tracestate. That lets a downstream service
//	sample at its own rate independently of the head, which is what the
//	statistical sampling-rate test measures. ParentBased(...) would instead make
//	the child blindly follow the parent's sampled flag (head-based sampling): the
//	downstream rate argument would be ignored and the per-node fraction would
//	collapse to the head's. Use ParentBased only if you intend to verify
//	head-based sampling, and then measure the head's rate.
package httpdirectstdlib

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
)

// spec configures one instrumented HTTP service in the chain.
type spec struct {
	rate      float64 // this service's TraceIDRatioBased rate
	propagate bool    // inject trace context onto its outgoing request (propagation on/off)
	asLink    bool    // start a new root linked to the upstream instead of continuing it
}

// service is one instrumented HTTP service. On each request it (optionally)
// continues or links the incoming trace, starts its application span, and
// forwards to its successor.
type service struct {
	name      string
	tracer    trace.Tracer
	next      string // successor base URL; "" for the last service
	propagate bool
	asLink    bool
}

func (s *service) handle(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run")
	path := r.URL.Path

	// Extract the upstream trace from the incoming headers — a real HTTP wrapper
	// does this in its server middleware.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	var span trace.Span
	if s.asLink {
		// Start a new root span linked to the upstream span context (span-link
		// topology): a different trace, connected by a link.
		upstream := trace.SpanContextFromContext(ctx)
		ctx, span = s.tracer.Start(context.Background(), s.name,
			harness.RunAttrOption(runID), trace.WithLinks(trace.Link{SpanContext: upstream}))
	} else {
		// Continue the upstream trace (parent-child).
		ctx, span = s.tracer.Start(ctx, s.name, harness.RunAttrOption(runID))
	}
	defer span.End()

	if s.next != "" {
		req, _ := http.NewRequestWithContext(ctx, r.Method, s.next+path+"?run="+runID, nil)
		if s.propagate {
			otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	w.WriteHeader(http.StatusOK)
}

// startChain brings up len(specs) instrumented services wired head→tail, each
// with its own TraceIDRatioBased TracerProvider exporting to endpoint. Returns
// the head URL and the service-name→rate map for the assertions.
func startChain(t *testing.T, endpoint string, specs []spec) (headURL string, want map[string]float64) {
	t.Helper()
	want = make(map[string]float64, len(specs))
	next := "" // build back-to-front so each service knows its successor's URL
	for i := len(specs) - 1; i >= 0; i-- {
		name := serviceName(i)
		want[name] = specs[i].rate
		tp := harness.BuildTracerProvider(t, name, sdktrace.TraceIDRatioBased(specs[i].rate), endpoint)
		svc := &service{
			name:      name,
			tracer:    tp.Tracer("chain"),
			next:      next,
			propagate: specs[i].propagate,
			asLink:    specs[i].asLink,
		}
		srv := httptest.NewServer(http.HandlerFunc(svc.handle))
		t.Cleanup(srv.Close)
		next = srv.URL
	}
	return next, want
}

func serviceName(i int) string { return "svc" + string(rune('0'+i)) }

// send fires one request through the chain from a fresh (random trace ID) head
// and returns the run ID.
func send(t *testing.T, headURL string) string {
	t.Helper()
	runID := uuid.NewString()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, headURL+"/a?run="+runID, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return runID
}

// driveExact runs the chain once when the app-span count is known (every service
// at rate 1.0) and waits for all of them — robust regardless of export ordering
// or collector latency.
func driveExact(t *testing.T, sink *harness.Sink, headURL string, want int) []harness.Span {
	t.Helper()
	return harness.WaitForAppSpans(t, sink, send(t, headURL), want, 15*time.Second)
}

// driveStatistical runs the chain once when the per-run count is unpredictable
// (a downstream samples below 1.0). It gates on the always-on, last-to-export
// head ("svc0") so a slow head span is never missed.
func driveStatistical(t *testing.T, sink *harness.Sink, headURL string) []harness.Span {
	t.Helper()
	return harness.WaitForStable(t, sink, send(t, headURL), "svc0", 500*time.Millisecond, 15*time.Second)
}

// TestPropagation: at rate 1.0 with propagation on, the downstream continues the
// upstream's trace (parent-child) — the sampler-agnostic propagation check.
func TestPropagation(t *testing.T) {
	env := harness.StartTelemetryEnv(t)
	headURL, _ := startChain(t, env.Endpoint, []spec{
		{rate: 1.0, propagate: true},
		{rate: 1.0, propagate: true},
	})
	run := driveExact(t, env.Sink, headURL, 2) // both services emit one app span
	harness.AssertTraceContinued(t, run, "svc0", "svc1")
	harness.AssertSameTrace(t, run)
}

// TestPropagationDisabled: with the head not injecting trace context, the
// downstream starts its own root — a different trace with no link back. Run at
// rate 1.0 so the sampler cannot drop the spans under test.
func TestPropagationDisabled(t *testing.T) {
	env := harness.StartTelemetryEnv(t)
	headURL, _ := startChain(t, env.Endpoint, []spec{
		{rate: 1.0, propagate: false}, // svc0 does not inject → trace severed
		{rate: 1.0, propagate: true},
	})
	run := driveExact(t, env.Sink, headURL, 2) // both services emit one app span
	harness.AssertTraceNotContinued(t, run, "svc0", "svc1")
}

// TestSpanLink: the downstream starts a new root linked to the upstream span
// (async/span-link topology) — a different trace connected by a link.
func TestSpanLink(t *testing.T) {
	env := harness.StartTelemetryEnv(t)
	headURL, _ := startChain(t, env.Endpoint, []spec{
		{rate: 1.0, propagate: true},
		{rate: 1.0, propagate: true, asLink: true},
	})
	run := driveExact(t, env.Sink, headURL, 2) // head + linked consumer, one app span each
	harness.AssertLinkedTrace(t, run, "svc0", "svc1")
}

// TestSamplingRate: drive many random-trace runs and assert the measured service
// is sampled at ~its configured rate. svc0 runs at 1.0 as an always-present
// anchor (driveStatistical gates on it so each run settles without missing the
// last-to-export head); svc1 decides independently from the shared trace ID at
// rate r.
func TestSamplingRate(t *testing.T) {
	env := harness.StartTelemetryEnv(t)
	const r = 0.5
	headURL, _ := startChain(t, env.Endpoint, []spec{
		{rate: 1.0, propagate: true},
		{rate: r, propagate: true},
	})

	const n = 40
	runs := make([][]harness.Span, 0, n)
	for i := 0; i < n; i++ {
		runs = append(runs, driveStatistical(t, env.Sink, headURL))
	}
	harness.AssertSampledFraction(t, runs, "svc0", 1.0, 0.01) // anchor always present
	harness.AssertSampledFraction(t, runs, "svc1", r, 0.2)
}
