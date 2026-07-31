# span-link-sampling-seed Delta: use-span-link-sampler-arg-like-parent-span

## ADDED Requirements

### Requirement: Single-link seed wrapper

The module SHALL export `WithSingleLinkSeed(delegate sdktrace.Sampler) sdktrace.Sampler` that adapts sampling **input** for root spans that carry span links, without changing span parentage or the links attached to the created span.

When `delegate` is `nil`, the wrapper SHALL use `sdktrace.AlwaysSample()` as the delegate.

On each `ShouldSample` call the wrapper SHALL apply these rules in order:

1. **Valid parent present**: if `ParentContext` already contains a valid SpanContext, pass `SamplingParameters` unchanged to the delegate and return its result without link-based seeding.
2. **Exactly one valid link**: if there is no valid parent and exactly one link whose `SpanContext.IsValid()` is true, the wrapper SHALL invoke the delegate with:
   - `ParentContext` set to a context carrying that link's SpanContext as a **remote** span context, and
   - `TraceID` set to that link's TraceID (sampler-input only; the SDK-created span TraceID remains the new root's TraceID).
   After the delegate returns, the wrapper SHALL ensure the result `tracestate` contains `ot=rv:<14 hex>` derived from the link: prefer a valid `ot=rv` on the link, otherwise the link TraceID's low 56 bits.
3. **Zero or multiple valid links**: pass parameters unchanged to the delegate. For these root cases (and for rule 2's result path that already has an rv), the wrapper SHALL still ensure an explicit `ot=rv:` is present on the result using the **current** `SamplingParameters.TraceID` randomness when rule 2 did not apply.

Invalid link SpanContexts SHALL be ignored when counting valid links. The wrapper's `Description` SHALL identify the delegate (e.g. `WithSingleLinkSeed{…}`).

Recommended application composition for consistent sampling across parent-child and single-link topologies:

`WithSingleLinkSeed(ProbabilitySampler(rate))` (or `ProbabilitySamplerFromEnv`).

#### Scenario: Single valid link seeds randomness like a parent

- **WHEN** a root span is sampled with no valid parent, exactly one valid link carrying `ot=rv:f0000000000000`, and a fresh TraceID whose own low 56 bits would drop at rate `0.5`
- **THEN** `WithSingleLinkSeed(ProbabilitySampler(0.5))` SHALL return `RecordAndSample`
- **AND** the result `tracestate` SHALL contain `rv:f0000000000000`

#### Scenario: Link without rv uses link TraceID

- **WHEN** a root is sampled with one valid link that has no `ot=rv` and whose TraceID low 56 bits equal `0xf0000000000000`, while the new root TraceID would drop
- **THEN** the wrapped sampler SHALL return `RecordAndSample`
- **AND** the result SHALL contain `ot=rv:f0000000000000`

#### Scenario: Parent takes precedence over links

- **WHEN** sampling parameters include both a valid parent whose TraceID randomness drops at rate `0.5` and a valid link whose TraceID randomness would sample
- **THEN** the wrapper SHALL NOT seed from the link
- **AND** the decision SHALL match sampling with the parent alone (`Drop`)
- **AND** the result SHALL NOT carry the link's `rv`

#### Scenario: Multiple valid links skip link seeding

- **WHEN** a root has two valid links and a TraceID that drops at rate `0.5`
- **THEN** `WithSingleLinkSeed(ProbabilitySampler(0.5))` SHALL return `Drop` (delegate sees the current TraceID, not either link)

#### Scenario: Root always emits explicit rv

- **WHEN** a root with no parent and no links is sampled through `WithSingleLinkSeed(ProbabilitySampler(0.5))`
- **THEN** a sampled result SHALL contain `ot=rv:` matching the TraceID low 56 bits
- **AND** a dropped result SHALL still contain `ot=rv:` matching that TraceID randomness (so later hops can reuse the seed)

#### Scenario: Linked chain preserves rv and decision shape

- **WHEN** services A→B→C are connected only by single span links (new roots) and D continues from C via parent-child, all using `WithSingleLinkSeed(ProbabilitySampler(…))` with the rates in the A/B/C/D/E threshold matrix
- **THEN** every hop SHALL carry the same `ot=rv` value originating from A's randomness
- **AND** equal-rate services SHALL make the same sample/drop decision for that randomness
- **AND** the deterministic subset property across rates SHALL hold along the chain

### Requirement: SDK span identity is unchanged by link seeding

When used as the `TracerProvider` sampler, `WithSingleLinkSeed` SHALL affect only the sampling decision and `tracestate`. A span started with `trace.WithLinks` and no parent SHALL remain a new root: its TraceID SHALL differ from the linked upstream TraceID, while its SpanContext `tracestate` SHALL carry the upstream-derived `ot=rv` when the single-link seed path applies.

A dropped linked span SHALL still yield a non-recording but valid SpanContext whose `tracestate` carries that `ot=rv`, so W3C TraceContext inject/extract can propagate the seed to downstream services.

#### Scenario: Linked SDK span keeps new TraceID and upstream rv

- **WHEN** an SDK tracer starts span B with one link to upstream A (`ot=rv:f0000000000000`) under `WithSingleLinkSeed(ProbabilitySampler(0.5))`
- **THEN** B's TraceID SHALL NOT equal A's TraceID
- **AND** B's SpanContext `tracestate` SHALL contain `rv:f0000000000000`
- **AND** B SHALL be sampled

#### Scenario: Dropped linked span still propagates rv

- **WHEN** an SDK tracer starts a linked root under a rate that drops for the upstream rv
- **THEN** no finished span SHALL be exported for that start
- **AND** the returned SpanContext SHALL be non-sampled
- **AND** TraceContext injection from that context SHALL include the upstream `ot=rv` in `tracestate`
