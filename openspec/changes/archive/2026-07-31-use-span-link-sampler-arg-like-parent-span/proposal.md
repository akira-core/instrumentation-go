# Proposal: use-span-link-sampler-arg-like-parent-span

## Why

This repo's instrumentation wrappers intentionally use **span links** (not parent-child) for async consumers — NATS subscribers, MongoDB change-stream / document readers, WebSocket readers. Standard OpenTelemetry probability samplers decide from parent context or the new root's TraceID; they ignore links. That means a span-link consumer starts a new root with a fresh TraceID and gets an independent sampling decision, so producer and consumer diverge under partial sampling even when they share the same logical causality.

We need a sampler stack that treats a single valid span link as the sampling seed the same way a remote parent is treated, while still writing explicit W3C `tracestate` randomness (`ot=rv:…`) so the seed survives propagation across services with different sample rates.

## What Changes

- Add a new Go module `otel-sampler` exporting package `otelsampler`:
  - `ProbabilitySampler` / `ProbabilitySamplerFromEnv` — threshold-based consistent probability sampler mirroring OpenTelemetry Go PR #8123 (reads `ot=rv:…` when present, else TraceID low 56 bits; writes `ot=th:…` on record).
  - `WithSingleLinkSeed(delegate)` — sampler wrapper that, for root spans with **exactly one** valid link and no valid parent, feeds the link's SpanContext (TraceID + tracestate) into the delegate as if it were a remote parent, then restores/writes explicit `ot=rv:…` on the result. Does **not** change parentage or emitted links.
- Recommended composition for services: `WithSingleLinkSeed(ProbabilitySampler(rate))` (also exposed as `harness.ConsistentSampler` / `ConsistentSamplerFromEnv` in `otel-testkit` for E2E verification).
- Supporting verification surface (`otel-testkit` harness + mongo sampling E2E + httpdirect examples) documents and black-box-asserts the consistent-rv / topology invariants; not part of the sampler's public behavioral contract beyond the convenience wrappers mirroring the composition above.

No **BREAKING** changes to existing instrumentation modules' public APIs.

## Capabilities

### New Capabilities

- `consistent-probability-sampling`: threshold probability sampler (`ProbabilitySampler` / `FromEnv`) — randomness source precedence, threshold decision, `ot=th:` / `ot=rv:` tracestate handling, env configuration, and deterministic subset property across rates.
- `span-link-sampling-seed`: `WithSingleLinkSeed` wrapper — when a single valid link stands in for a missing parent, the delegate SHALL sample from that link's randomness/TraceID the same way it would from a remote parent; root spans SHALL emit explicit `ot=rv:`; parent takes precedence; zero/multiple valid links fall through without link seeding.

### Modified Capabilities

None. Existing instrumentation specs already require span-link (not parent-child) for async consumers; this change adds the complementary **application-side sampler** so those links participate in consistent sampling. No requirement text in `otel-*-spans` / `*-tracing` specs needs to change.

## Impact

- **New module**: `otel-sampler/` (`github.com/akira-core/instrumentation-go/otel-sampler/otelsampler`) — independent `go.mod`, unit + SDK integration tests.
- **Consumers**: real services and E2E tests MUST compose `WithSingleLinkSeed(ProbabilitySampler(...))` on their `TracerProvider` for span-link topologies to stay consistent; bare `ProbabilitySampler` alone is insufficient for linked roots (and only writes `th`, not `rv`).
- **`otel-testkit`**: `harness.ConsistentSampler` / `ConsistentSamplerFromEnv` wrap the composition; assertions (`AssertConsistentRV`, topology helpers) and examples (`httpdirect`, `httpdirect-stdlib`) verify sampler-aware vs sampler-agnostic paths.
- **CI / Makefile**: module added to the build/test/lint matrix.
- **Non-impact**: instrumentation wrappers themselves do not change sampling; they continue to create linked roots. Sampling policy remains the application's TracerProvider concern.
