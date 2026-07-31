// Package harness provides reusable, black-box end-to-end integration-test
// infrastructure for the instrumentation modules: a real OTel Collector
// (testcontainers) that re-exports spans to an in-process OTLP sink,
// per-service TracerProviders wired with the consistent probabilistic sampler,
// and assertion helpers for the consistent-sampling invariant.
//
// It is a toolkit, not a framework. You drive your own realistic application
// flow using the instrumentation library exactly as an application would —
// each service's TracerProvider exports to the collector (BuildTracerProvider),
// you seed a known randomness value at the head (SeedContextRV), and you assert
// on the spans collected at the sink (AssertConsistentRV / AssertPresence /
// AssertNoWrapperSpans / SpansByAttr). Trace propagation and span
// parent-child/link topology come from the library itself — the harness never
// injects, extracts, or forces topology.
//
// The same helpers work against a real application: point each service's
// OTEL_EXPORTER_OTLP_ENDPOINT at StartCollector's endpoint, drive traffic, and
// assert with SpansByAttr / AssertConsistentRV. See README.md.
package harness

// RunAttr is the recommended span attribute key for correlating the spans of a
// single logical run across services (span-link hops start a new trace ID, so
// trace ID alone cannot group a mixed-topology flow). Set it on every
// application span you start, then query the sink with (*Sink).ByRun or
// SpansByAttr(spans, RunAttr, runID).
const RunAttr = "chain.run"
