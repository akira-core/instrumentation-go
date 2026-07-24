# Design: use-span-link-sampler-arg-like-parent-span

## Context

Instrumentation modules in this repo use **span links** for async consumers so causality is preserved without implying synchronous nesting. Sampling, however, is owned by the application's `TracerProvider`, not by the wrappers. Stock SDK samplers (`TraceIDRatioBased`, parent-based wrappers) do not consult links when deciding a new root.

OpenTelemetry Go PR [#8123](https://github.com/open-telemetry/opentelemetry-go/pull/8123) introduces a threshold-based `ProbabilitySampler` that:

1. Prefers explicit randomness from parent `tracestate` (`ot=rv:<14 hex digits>`).
2. Otherwise derives randomness from the least-significant 56 bits of the TraceID.
3. Compares against a threshold encoded as `ot=th:…` when recording.

That alone still fails for span-link roots: the consumer's new TraceID is unrelated to the producer, so the decision and any derived randomness diverge. The missing piece is a thin input adapter that, for a root with exactly one valid link, presents that link to the delegate as a remote parent — then writes the chosen randomness back as `ot=rv:` on the result so later hops (parent-child or another single-link) reuse the same seed.

This change is already implemented on branch `use-span-link-sampler-arg-like-parent-span`; this design records the architectural decisions for the OpenSpec archive.

## Goals / Non-Goals

**Goals:**

- Consistent sampling decisions across a mixed topology of parent-child hops and single-link hops that share one logical producer seed.
- Explicit `ot=rv:` emission on roots so randomness propagates even when intermediate services drop (non-recording SpanContext still carries tracestate via the SDK).
- Deterministic subset property: if rate A < rate B, every TraceID/randomness sampled at A is also sampled at B.
- Keep the adapter orthogonal to the probability engine so `WithSingleLinkSeed` can wrap non-`ProbabilitySampler` delegates (compatibility documented; consistent-rv guarantees require a delegate that honors parent `ot=rv` / TraceID randomness).

**Non-Goals:**

- Changing instrumentation wrappers to use parent-child instead of links.
- Multi-link fan-in sampling policy (more than one valid link → no seeding; leave to delegate with current TraceID).
- Shipping a custom propagator; W3C TraceContext is sufficient once `rv`/`th` live in `tracestate`.
- Making `otel-testkit` part of the production API surface (it only documents and verifies the recommended composition).
- Upstreaming into `opentelemetry-go` (we mirror #8123 locally until/unless it lands).

## Decisions

### D1. Separate module `otel-sampler`, not inside each instrumentation package

Sampling is an application concern shared across transports. Putting the sampler in each wrapper would force every service to import mongo/nats/ws just for sampling, and would duplicate the algorithm. A standalone module keeps the disabled-mode invariant of the wrappers untouched (no TracerProvider init inside instrumentation).

### D2. Two composable types: `ProbabilitySampler` + `WithSingleLinkSeed`

```
TracerProvider sampler =
  WithSingleLinkSeed(
    ProbabilitySampler(rate)   // or ProbabilitySamplerFromEnv(default)
  )
```

- **Why not bake link-seeding into ProbabilitySampler?** Bare `ProbabilitySampler` is useful for pure parent-child graphs and matches the upstream PR surface. Link seeding is a separate input transform that any `sdktrace.Sampler` can sit under.
- **Why not only document "callers must set ParentContext from the link"?** Call sites that start linked roots (`trace.WithLinks`) do not rewrite `ParentContext`; the sampler is the single place that can apply the policy without every consumer reimplementing it.

### D3. Link seeding rules (strict)

| Condition | Behavior |
|---|---|
| Valid parent in `ParentContext` | Pass params unchanged to delegate (links ignored for seeding). |
| No valid parent, **exactly one** valid link | Set `ParentContext` to remote span context of that link; set `params.TraceID` to the link's TraceID for the delegate call only; after delegate returns, write `ot=rv:` from link randomness (prefer link `ot=rv`, else link TraceID low 56 bits). |
| Zero or multiple valid links | Pass params unchanged; for roots still ensure `ot=rv:` is written from the **current** TraceID randomness. |

Invalid link SpanContexts are skipped when counting "valid" links.

**Critical invariant:** the wrapper only changes **sampler input**. The SDK still creates a new root with its own TraceID; links on the span are unchanged. SDK integration tests assert `linked.TraceID != upstream.TraceID` while `ot=rv` matches.

### D4. Randomness / threshold encoding

- Randomness mask: `2^56 - 1` (56 bits), matching #8123.
- `ot=rv:` value: exactly 14 lowercase hex digits.
- On record: insert/update `ot=th:<threshold hex>` via `insertOrUpdateTraceStateThKeyValue`; preserve other `ot` subkeys (including `rv` when present).
- On drop: `ProbabilitySampler` leaves tracestate as found (typically no new `th`); `WithSingleLinkSeed` may still attach `rv` on roots so dropped-but-propagated contexts remain consistent for downstream services with higher rates.
- Invalid / malformed `rv` → ignore and fall back to TraceID randomness (same as missing `rv`).

### D5. Probability edge cases

- `probability >= 1.0` → always record (`th:0`).
- `NaN` or below `1/2^56` → `sdktrace.NeverSample()`.
- `ProbabilitySamplerFromEnv`: parse `OTEL_TRACES_SAMPLER_ARG` as float; on unset/parse error use the provided default.

### D6. `otel-testkit` mirrors composition, does not redefine it

`harness.ConsistentSampler(rate)` = `WithSingleLinkSeed(ProbabilitySampler(rate))`. Docs call out that the wrapper is required, not optional — bare `ProbabilitySampler` ignores links and does not emit `rv`. Stdlib-sampler examples (`httpdirect-stdlib`) intentionally omit consistent sampling to show sampler-agnostic topology assertions.

## Risks / Trade-offs

- **[Risk] Multi-link spans get no shared seed** → Accepted. Fan-in has no single upstream; inventing a combine function would be speculative. Callers needing consistency must ensure exactly one valid link (the normal case for our wrappers).
- **[Risk] Delegate that ignores parent randomness (e.g. plain `TraceIDRatioBased`) still diverges on linked roots** → Documented. `WithSingleLinkSeed` rewrites `params.TraceID` to the link's TraceID for the delegate call, which helps TraceID-based samplers; explicit `rv` emission still helps later `ProbabilitySampler` hops. Full consistent-threshold semantics require `ProbabilitySampler` (or equivalent).
- **[Risk] Divergence from eventual upstream #8123 API** → Mitigate by keeping the surface small and tested; adjust when upstream lands.
- **[Trade-off] Dropped roots still get `ot=rv:` via the wrapper** → Necessary so a later higher-rate service can still join the consistent decision; slightly larger tracestate on non-recording contexts.
- **[Trade-off] Sampling input TraceID rewrite is invisible to the created span** → Correct and intentional; confusing if someone inspects sampler unit tests without reading the SDK integration tests. Spec scenarios cover both layers.

## Migration Plan

- New module; applications opt in by configuring their `TracerProvider` sampler.
- No change required inside instrumentation packages for correctness of span creation; consistency appears only after apps adopt the composition.
- Rollback: stop using `otelsampler` / revert to previous sampler; no wire-format migration (tracestate keys are additive).

## Open Questions

None for the archived behavior. Follow-ups (out of scope): multi-link policy, upstream contribution of `WithSingleLinkSeed`, CI drift check if #8123 lands with different encoding.
