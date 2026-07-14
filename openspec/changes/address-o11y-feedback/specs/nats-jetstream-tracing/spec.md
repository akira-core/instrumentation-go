# nats-jetstream-tracing Delta Specification

## ADDED Requirements

### Requirement: HeaderCarrier multi-value and canonical-fallback reads
`otelnats.HeaderCarrier` SHALL implement `propagation.ValuesGetter` in addition to `propagation.TextMapCarrier`. `Values(key)` SHALL return all values stored under the verbatim key when present, otherwise all values stored under `textproto.CanonicalMIMEHeaderKey(key)`. `Get(key)` SHALL follow the same lookup order (verbatim first, canonical fallback) and return the first value. The fallback SHALL trigger on key **absence**, not value emptiness — a verbatim key present with an empty value wins over a canonical entry, identically for `Get` and `Values`. `Set` SHALL remain unchanged, writing the verbatim key — the canonical fallback is a read-side compatibility measure only, so messages produced by current writers are unaffected.

#### Scenario: Multi-instance baggage header preserved
- **WHEN** a message carries two `baggage` header values and a propagator extracts via a carrier that supports `ValuesGetter`
- **THEN** `Values("baggage")` returns both values in order, so no baggage entry is silently truncated

#### Scenario: Canonicalized producer header still extracts
- **WHEN** a message in a durable stream carries its trace context under the MIME-canonical key `Traceparent` (written by a canonicalizing producer) and a consumer extracts with key `traceparent`
- **THEN** `Get`/`Values` fall back to the canonical form and return the stored value, preserving the trace link

#### Scenario: Verbatim key wins over canonical form
- **WHEN** a header stores values under both `traceparent` (verbatim) and `Traceparent` (canonical)
- **THEN** `Get`/`Values` return the verbatim entry's value(s) and do not merge the two forms

### Requirement: JetStream consumer name uses the semconv consumer-group key
JetStream consumer spans SHALL attach the consumer/durable name under the semconv v1.39.0 generated key `messaging.consumer.group.name` (`semconv.MessagingConsumerGroupNameKey`), on both the per-message consumer spans and the ordered-consumer fallback path. The non-semconv literal `messaging.consumer.name` SHALL NOT be emitted.

#### Scenario: Durable consumer span carries the semconv key
- **WHEN** a message is received through a durable JetStream consumer with tracing enabled
- **THEN** the receive span has attribute `messaging.consumer.group.name` set to the durable/consumer name, and no `messaging.consumer.name` attribute

#### Scenario: Ordered consumer fallback uses the same key
- **WHEN** an ordered consumer without an explicit name produces receive spans
- **THEN** the fallback name attribute is attached under `messaging.consumer.group.name`

### Requirement: Consumer.Next honors live context cancellation
`oteljetstream.Consumer.Next(ctx, opts...)` SHALL abort its wait and return `ctx.Err()` promptly when `ctx` is cancelled, including a ctx with no deadline. Cancellation SHALL be wired via `jetstream.FetchContext(ctx)` — its internal fetch goroutine selects on `ctx.Done()` natively, so no wrapper-side `Stop()` escape hatch or negative-acknowledgement machinery is involved. The wrapper's `FetchContext` SHALL be appended **after** all caller-supplied fetch options, making the method parameter `ctx` authoritative: a caller-supplied `FetchContext(otherCtx)` SHALL NOT shadow it. A ctx that can never fire (`ctx == nil`, or `ctx.Done() == nil` as with `context.Background()`/`context.TODO()`) SHALL skip the wiring entirely, preserving caller-supplied `FetchMaxWait` behavior. Because upstream jetstream rejects combining `FetchContext` with `FetchMaxWait`, a cancelable ctx combined with a caller-supplied `FetchMaxWait` SHALL surface jetstream's native `ErrInvalidOption` rather than silently dropping cancellation; callers wanting both use the ctx's own deadline.

#### Scenario: Cancelling a deadline-less wait
- **WHEN** `Next(ctx)` is waiting on an empty stream and the caller cancels `ctx` (no deadline set)
- **THEN** `Next` returns `ctx.Err()` promptly (bounded by scheduling, not by any server-side max-wait), and no goroutine remains parked on the fetch

#### Scenario: Message arrives before cancellation
- **WHEN** a message is delivered before `ctx` is cancelled
- **THEN** `Next` returns the message exactly as before, with the same receive-span and returned-context semantics

#### Scenario: Cancelable ctx with caller FetchMaxWait errors loudly
- **WHEN** `Next(cancelableCtx, jetstream.FetchMaxWait(d))` is called
- **THEN** `Next` returns jetstream's `ErrInvalidOption` without contacting the server, while `Next(context.Background(), jetstream.FetchMaxWait(d))` keeps working unchanged

#### Scenario: Method ctx wins over a caller-supplied FetchContext
- **WHEN** `Next(ctx, jetstream.FetchContext(otherCtx))` is called and `ctx` is cancelled while waiting
- **THEN** `Next` returns `ctx.Err()` promptly — the caller's `FetchContext` cannot shadow the method parameter's cancellation

## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The packages SHALL gate span creation and W3C header propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global) and `OTEL_NATS_TRACING_ENABLED` (module). Both SHALL default to disabled when unset; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value SHALL enable. The env-derived result SHALL serve as the **default**: when the caller passes the `WithTracingEnabled(v bool)` option to a connect variant, that value SHALL be authoritative for the resulting `Conn` (and everything derived from it, including `oteljetstream` wrappers and deliver-span initialization), overriding both environment gates in either direction per the shared `WithTracingEnabled` decision table in `shared-feature-flags`. Connections constructed without the option SHALL behave exactly as before.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** all NATS/JetStream tracing is disabled regardless of `OTEL_NATS_TRACING_ENABLED`

#### Scenario: Both flags on
- **WHEN** both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` are set to a truthy value and no `WithTracingEnabled` option is passed
- **THEN** `Conn` and JetStream operations create spans and propagate W3C trace context in message headers

#### Scenario: Option enables tracing with env off (unset or falsy)
- **WHEN** `ConnectWithOptions(url, nil, WithTracingEnabled(true))` is called with both tracing env vars unset or explicitly falsy
- **THEN** the connection creates spans and propagates trace context — the option overrides the env default

#### Scenario: Option disables tracing despite truthy env vars
- **WHEN** both env gates are truthy and the caller passes `WithTracingEnabled(false)`
- **THEN** that connection performs no tracing (native delegation, no deliver-provider initialization), while other connections without the option still trace per the env gates

### Requirement: MessageBatch lifecycle and Stop()
`oteljetstream.MessageBatch` SHALL expose `Stop()` to release the batch's internal forwarding goroutine. `Stop()` SHALL take effect promptly regardless of where the goroutine is parked: the forwarding loop SHALL observe the stop signal both while waiting to **receive** from the native batch and while waiting to **send** to the wrapper channel. Callers that fully drain `Messages()` until the channel closes are not required to call `Stop()`; callers that `break`/`return` before the channel closes SHALL call `Stop()` (typically via `defer`) to release the goroutine. Because receive spans end at handover (see the consume-path lifecycle requirement), abandoning a batch no longer risks an unbounded in-flight span — `Stop()`'s obligation is goroutine release.

#### Scenario: Full drain
- **WHEN** a caller ranges over `batch.Messages()` until the channel closes naturally
- **THEN** the batch's goroutine is already released without an explicit `Stop()` call

#### Scenario: Early break
- **WHEN** a caller `break`s out of the `range batch.Messages()` loop before the channel closes
- **THEN** an explicit (typically deferred) `batch.Stop()` call is required to release the forwarding goroutine; omitting it leaks the goroutine

#### Scenario: Stop while parked on an empty stream
- **WHEN** the forwarding goroutine is blocked waiting for the native batch to produce a message (no message has arrived) and the caller invokes `Stop()`
- **THEN** the goroutine exits promptly without requiring the native fetch to produce a message or expire

### Requirement: Single-fetch and iterator Next return equivalent trace context
`oteljetstream.Consumer.Next` and `oteljetstream.MessagesContext.Next` SHALL both return a `context.Context` bearing the wrapper's local consumer receive span (linked to the producer's extracted trace context), so downstream spans created from the returned context nest under the consumer's receive span rather than directly under the remote producer span. This matches the context semantics of the `Consume` handler path. Across **all** consume paths — single-shot `Consumer.Next`, `MessagesContext.Next`, and the batch (`Fetch`/`FetchBytes`/`FetchNoWait`) forwarding goroutine — the receive span SHALL already be ended by the time the caller observes the message: the `Next` variants end it before returning, and the batch forwarder ends it **before** the channel send (ending after the send would race the receiver's `IsRecording()` check across the channel rendezvous). No consume path may hold a message's receive span open until the next message is read. The returned/attached context still carries the ended span, and child spans parent to it correctly via its still-valid `SpanContext`; callers measure their processing time with their own child spans.

#### Scenario: Downstream spans nest under the consumer receive span
- **WHEN** `cons.Next(ctx)` returns a message with tracing enabled and the caller starts a downstream span from the returned context
- **THEN** the downstream span is a child of the wrapper's local consumer receive span (which is linked to the producer), identical in shape to what `Messages().Next` and the `Consume` handler produce

#### Scenario: Batch message span is ended at delivery
- **WHEN** a message is delivered through `batch.Messages()` with tracing enabled and the caller immediately calls `trace.SpanFromContext(msg.Context()).IsRecording()`
- **THEN** the receive span has already ended (`IsRecording() == false`) — its duration measured receive-to-handover, not the gap until the next message was read

#### Scenario: Iterator Next ends the span before returning
- **WHEN** `MessagesContext.Next()` returns a message with tracing enabled
- **THEN** that message's receive span is already ended at return, matching single-shot `Consumer.Next` semantics, and no bookkeeping defers its end to the subsequent `Next()` call
