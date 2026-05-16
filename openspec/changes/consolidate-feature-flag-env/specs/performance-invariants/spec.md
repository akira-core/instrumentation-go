## ADDED Requirements

### Requirement: Sampler-aware deliverSpan gate

The synthetic broker-hop `deliverSpan` SHALL only be emitted when the relevant
span context is sampled.

- Producer side (`otelnats.tracedConn.StartDeliverSpan`): when the local
  span context in `ctx` is valid but `IsSampled() == false`, the function
  SHALL early-return `ctx` unchanged — no deliverSpan, no exporter traffic,
  no allocation.
- Consumer side (`otelnats.tracedConn.ConsumerContextWithDeliver`): when
  `origin.IsValid()` is true but `origin.IsSampled()` is false, the function
  SHALL early-return `ctx` unchanged.
- The deliverTracer-nil and origin-invalid early returns SHALL remain in
  place as the first-line gates.

Rationale: a sampled=0 trace does not appear in the backend; emitting a
deliverSpan for it produces an orphan broker-hop node whose link target
never arrives. The gates eliminate the wasted Start/End/attribute build /
OTLP encode cost.

#### Scenario: Producer-side skip when local span is unsampled

- **WHEN** `StartDeliverSpan(ctx, subject)` is called with `ctx` carrying a
  valid SpanContext whose `IsSampled() == false`
- **THEN** the function SHALL return `ctx` unchanged (no remote span context
  attached, no deliverSpan recorded by the deliverTracer)

#### Scenario: Consumer-side skip when upstream span is unsampled

- **WHEN** `ConsumerContextWithDeliver(ctx, subject, origin)` is called with
  `origin.IsValid() == true` and `origin.IsSampled() == false`
- **THEN** the function SHALL return `ctx` unchanged

#### Scenario: Both gates remain effective when sampled

- **WHEN** the equivalent calls are made with the relevant SpanContext
  sampled
- **THEN** the deliverSpan SHALL be emitted and the returned `ctx` SHALL
  carry the appropriate remote span context (existing v0.4.x behaviour)

### Requirement: Sampler-aware consumer link gate

Consumer-side span constructors SHALL apply both the `IsValid()` and `IsSampled()` guards before attaching a link derived from the inbound trace context.

Applies to all 4+ hot-paths:
- `otelnats.tracedConn.recordReply`
- `otelnats.tracedConn.wrapMsgHandler`
- `oteljetstream.newTracedMessageBatch`
- `oteljetstream.tracedConsumer.Next`
- `oteljetstream.tracedConsumeHandler`
- `oteljetstream.tracedMessagesContext.Next`

#### Scenario: Link omitted when upstream unsampled

- **WHEN** any of the above consumer paths processes a message carrying a
  `traceparent` with `flags = 00`
- **THEN** the emitted consumer span's `Links()` slice SHALL be empty
- **AND** the wrapper SHALL still emit the consumer span itself (link omission
  is the only effect; span lifecycle is unchanged)

#### Scenario: Link attached when upstream sampled

- **WHEN** the same paths process a message carrying a `traceparent` with
  `flags = 01`
- **THEN** the emitted consumer span SHALL carry exactly one link whose
  SpanContext matches the inbound trace ID and span ID

### Requirement: No-propagation paths emit span without link

When `OTEL_NATS_PROPAGATION_ENABLED` is off, the wrapper SHALL NOT call
`propagator.Extract`, SHALL NOT call `ConsumerContextWithDeliver`, and SHALL
NOT evaluate the link condition — but SHALL still emit the consumer span.

This is the structural form: the link branch lives inside the propagation
closure; when propagation is off the originSpanCtx variable is never assigned
and the link branch is unreachable. Anchored by the universal default-OFF
posture defined in `instrumentation-feature-flags`.

#### Scenario: Subscribe with propagation off still emits standalone span

- **WHEN** `OTEL_NATS_PROPAGATION_ENABLED` is unset or falsy and a message
  arrives carrying a sampled `traceparent` header
- **THEN** the consumer wrapper SHALL emit a span via `tracer.Start`
- **AND** the span's `Links()` SHALL be empty
- **AND** the span's `SpanContext.TraceID()` SHALL NOT equal the inbound
  header's trace ID (no extraction occurred)

#### Scenario: Request/reply with propagation off mirrors the rule

- **WHEN** `recordReply` is invoked with propagation off
- **THEN** the "receive" CONSUMER span SHALL be emitted
- **AND** SHALL carry zero links regardless of the reply header content

### Requirement: MessageBatch lifecycle drain

`oteljetstream.MessageBatch` wrapper goroutines SHALL drain the upstream
`raw.Messages()` channel after `Stop()` is invoked, until the upstream
driver closes the channel.

Applies to both `newDirectMessageBatch` and `newTracedMessageBatch` in
`oteljetstream/consumer.go`.

Failure mode this rule prevents: upstream jetstream driver goroutine blocks
forever on chan send to an unbuffered channel when the wrapper goroutine
exits early via the `<-done` select case without draining.

#### Scenario: Stop while raw still has undelivered messages

- **WHEN** caller invokes `batch.Stop()` while the upstream `raw` channel
  still has buffered or pending messages
- **THEN** the wrapper goroutine SHALL enter a drain loop reading from
  `raw.Messages()` until it is closed
- **AND** the drain SHALL be a tight no-op loop (no span work, no caller
  channel send)

#### Scenario: Stop is idempotent

- **WHEN** `batch.Stop()` is invoked multiple times
- **THEN** subsequent calls SHALL be no-ops (sync.Once invariant)

### Requirement: Header sentinel — no allocation on nil msg headers

JetStream consumer wrappers SHALL NOT allocate a fresh `nats.Header` for
messages whose `Headers()` returns nil.

Affected sites: `oteljetstream/consumer.go` and
`oteljetstream/consumer_traced.go` (Fetch / Consume / MessagesContext.Next
paths).

The optional traceparent early-return path (`if hdr != nil && hdr.Get("traceparent") != ""`)
SHALL precede any header-based work so nil-header messages incur zero
allocation overhead.

#### Scenario: Nil header path allocates zero headers

- **WHEN** a jetstream message with `Headers() == nil` arrives at any
  affected consumer wrapper
- **THEN** the wrapper SHALL NOT call `make(nats.Header)`
- **AND** SHALL early-return from header inspection without allocating

### Requirement: BSON inject must use type-switch and clone before inject

BSON inject helpers SHALL use a type-switch fast path for `bson.D`, `bson.M`, and `map[string]any` inputs, and SHALL allocate a fresh `bson.D` for every returned value so the caller's slice or map backing storage is never aliased. Applies to `otelmongo.InjectTraceIntoDocument` and `otelmongo.InjectTraceIntoUpdate` in both v1 and v2 (`internal/shared/tracing.go`).

- Fast path SHALL skip the legacy `Marshal → Unmarshal` round-trip for the
  three concrete types above.
- Fallback path (struct or other inputs) SHALL preserve the existing
  marshal/unmarshal behaviour.
- `upsertSetField` SHALL clone the inner `bson.D` / `bson.M` of `$set` so
  the same shared-backing-array hazard does not leak through the operator
  path.

#### Scenario: Caller's bson.D is not mutated

- **WHEN** the inject function is called with a `bson.D` input
- **THEN** the caller's slice SHALL remain unchanged (same length, same
  contents, same backing-array values after the call)

#### Scenario: Returned bson.D does not share backing array with caller

- **WHEN** the caller subsequently appends entries to its original `bson.D`
- **THEN** the returned `bson.D` SHALL NOT observe those appended entries
  (no shared backing array)

#### Scenario: Struct fallback path produces equivalent output

- **WHEN** the inject function is called with a struct input
- **THEN** the function SHALL return a `bson.D` containing the struct's
  fields plus the `_oteltrace` entry (existing behaviour preserved via the
  marshal/unmarshal fallback)

### Requirement: WS marshalWire — pooled hand-written serializer

`otelgorillaws.marshalWire` SHALL use a `sync.Pool` of byte buffers and a
hand-written JSON serializer (no `encoding/json.Marshal` reflection on the
hot path).

Output bytes SHALL be structurally equivalent to the legacy encoding/json
form (round-trips through `json.Unmarshal` into the same `wireEnvelope`
struct fields).

#### Scenario: Output round-trips through json.Unmarshal

- **WHEN** `marshalWire` returns bytes for any valid input
- **THEN** the returned bytes SHALL be valid JSON
- **AND** SHALL unmarshal into a `wireEnvelope` whose `Header` field matches
  the truthy traceparent / tracestate entries supplied by the caller
- **AND** whose `Data` field matches the caller's payload (verbatim for
  valid JSON, JSON-string-wrapped for non-JSON)

#### Scenario: Concurrent calls do not bleed pool buffers

- **WHEN** `marshalWire` is invoked concurrently from 16+ goroutines with
  distinct traceparent values
- **THEN** each returned byte slice SHALL contain only the caller's own
  traceparent (no cross-goroutine contamination from the pooled buffer)

#### Scenario: Wire format compatibility with JS peer

- **WHEN** the JS instrumentation packages (`otel-rxjs-ws`, `otel-ws`)
  unmarshal a frame produced by `marshalWire`
- **THEN** they SHALL decode the envelope identically to the v0.4.x form
  (preserved by the round-trip equivalence guaranteed above)

### Requirement: Tests SHALL lock each invariant

For every requirement in this spec, at least one `_test.go` test SHALL
explicitly assert the invariant — both the positive case (rule applies, e.g.
link attached when sampled) and the negative case (rule does not apply, e.g.
no link when unsampled).

Tests SHALL live next to the code they cover, named to describe the
behaviour (not the plan codename — see audit phase of this change for
naming convention).

#### Scenario: Regression test exists for each invariant

- **WHEN** a maintainer searches the test suite for any of the rules above
- **THEN** at least one test name SHALL describe the rule's behaviour (e.g.
  `TestSubscribeWithPropagationOffStillEmitsSpanWithoutLink`,
  `TestInjectDoesNotShareBackingArray`,
  `TestMessageBatchStopReleasesRawBatch`)
- **AND** removing the corresponding production code SHALL cause that test
  to fail (the test is load-bearing, not decorative)
