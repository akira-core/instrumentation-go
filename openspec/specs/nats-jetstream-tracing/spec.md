# nats-jetstream-tracing Specification

## Purpose
TBD - created by archiving change document-otel-instrumentation. Update Purpose after archive.
## Requirements
### Requirement: Provider and propagator fallback
`otelnats` and `oteljetstream` SHALL NOT construct or own a global `TracerProvider`. `Connect` and `ConnectWithOptions` SHALL use `otel.GetTracerProvider()` and `otel.GetTextMapPropagator()` unless the caller supplies `WithTracerProvider(tp)` and/or `WithPropagators(p)` via `ConnectWithOptions`. `ConnectTLSWithOptions` and `ConnectWithCredentialsWithOptions` are the equivalent override entry points for TLS and credentials-file connections, respectively.

#### Scenario: Default connect
- **WHEN** an application calls `otelnats.Connect(url, nil)` without options
- **THEN** the connection uses the process-global `TracerProvider` and `TextMapPropagator` at connect time

#### Scenario: Known limitation — ConnectTLS / ConnectWithCredentials panic when tracing is enabled
- **WHEN** an application calls the convenience functions `ConnectTLS(...)` or `ConnectWithCredentials(...)` (not the `...WithOptions` variants) while both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` are truthy
- **THEN** the connection panics with a nil-pointer dereference: both functions forward a bare untyped `nil` as the sole positional argument into their `...WithOptions` sibling's variadic `traceOpts ...Option` parameter, producing a one-element `[]Option{nil}` slice (not an empty slice); `newConnConfig` then calls `.apply(c)` on that nil `Option` interface value and panics. This is a real, currently-shipped bug (not an intended behavior) — untested by `conn_test.go`, which exercises neither function. Callers needing tracing with TLS or credentials-file auth must use `ConnectTLSWithOptions`/`ConnectWithCredentialsWithOptions` directly instead.

### Requirement: Two-tier tracing feature-flag gating
The packages SHALL gate span creation and W3C header propagation behind two switches, composed by conjunction with short-circuit semantics:

```
tracing := master && natsTracing
```

Each switch SHALL be resolved down the precedence ladder defined in `shared-feature-flags` — `relay > env > option > default` — implemented as a single `Boolean` call whose evaluation default is the env-or-option-or-default value fixed at construction:

| Switch | Relay key | Option | Environment variable | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| tracing | `otel-nats-tracing` | `WithTracingEnabled` | `OTEL_NATS_TRACING_ENABLED` | `false` |

A relay value SHALL override the local value in **either** direction. Supplying `WithTracingEnabled` and `OTEL_NATS_TRACING_ENABLED` together SHALL NOT be an error; the **environment variable** wins, and the option decides only when the variable is unset. `WithTracingEnabled` SHALL supply the module tier only, never the master.

An environment value outside the recognised truthy and falsy lists, including the empty string, SHALL make the connect variant return an error wrapping `otelflags.ErrInvalidFlagValue`, per `shared-feature-flags`.

Which implementations exist SHALL be decided at construction by whether a relay can exist at all — `relayPossible || (masterLocal && tracingLocal)`, matching the other three modules. When it is false only `directConn` / `directJSImpl` SHALL be constructed and no OpenFeature client SHALL be created, because the relay is structurally incapable of returning anything but the local value. When it is true both implementations SHALL be constructed, because the relay may enable tracing the environment left off.

Both relay-backed switches SHALL be read per operation rather than cached on the wrapper struct, so a `Conn` and everything derived from it — `oteljetstream` wrappers, consumers, long-lived message iterators and batch forwarders — observes a change on its next operation without reconstruction. `WithTracingEnabled` SHALL NOT make a connection static.

#### Scenario: Master off disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is falsy, or the relay resolves `otel-instrumentation-go-tracing` to `false`
- **THEN** all NATS/JetStream tracing is disabled regardless of `OTEL_NATS_TRACING_ENABLED`, `WithTracingEnabled` or `otel-nats-tracing`

#### Scenario: Nothing configured traces nothing
- **WHEN** no environment variable is set, no option is passed, and no relay flag exists
- **THEN** the master resolves to `true`, the tracing switch resolves to its default of `false`, no spans are created and no headers are injected

#### Scenario: Module switch off constructs only the passthrough implementation
- **WHEN** a `Conn` is constructed with no endpoint variable, no installed provider, and the tracing switch resolving locally to disabled
- **THEN** only `directConn` is built, `oteljetstream.New` on that `Conn` returns `directJSImpl`, no OpenFeature client is created, and no evaluation is ever performed for that connection

#### Scenario: Environment variable alone enables tracing
- **WHEN** `OTEL_NATS_TRACING_ENABLED` is truthy, no option is passed and no relay flag exists
- **THEN** `Conn` and JetStream operations create spans and propagate W3C trace context in message headers, because the master defaults to `true`

#### Scenario: No relay configured reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed, no endpoint variable is set, and the switches are configured through environment variables and options only
- **THEN** the resolved behaviour is the conjunction of those sources with the hardcoded defaults, and no OpenFeature evaluation is performed

#### Scenario: Relay disables tracing on a running connection
- **WHEN** a `Conn` is publishing with tracing enabled and the relay subsequently resolves `otel-nats-tracing` to `false`
- **THEN** the next publish delegates natively, emits no span, and injects no trace headers

#### Scenario: Relay enables tracing the deployment left off
- **WHEN** `OTEL_NATS_TRACING_ENABLED` is unset, `relayPossible` is true, and the relay resolves `otel-nats-tracing` to `true`
- **THEN** the next publish creates a span and injects trace headers, without the connection being recreated

#### Scenario: Relay change reaches a long-lived JetStream consumer
- **WHEN** a `MessagesContext` iterator or a `MessageBatch` forwarder created before a relay change is still delivering messages after it
- **THEN** messages delivered after the change follow the new value, without the consumer being recreated

#### Scenario: MessageBatch does not freeze the flag at Fetch time
- **WHEN** `Fetch` / `FetchBytes` / `FetchNoWait` returns a `MessageBatch` and the relay subsequently changes `otel-nats-tracing` before the batch finishes delivering
- **THEN** subsequent messages from that same batch follow the new value, without recreating the consumer or the batch handle

#### Scenario: Traced hot path does not rebuild constant attrs per message
- **WHEN** a dynamic JetStream consume path is tracing on for consecutive messages on the same consumer
- **THEN** tracer, propagator, and receive base attributes that are constant for the traced connection are not reallocated solely to re-read the gate (the gate itself is still consulted per message to decide whether to emit)

#### Scenario: Option and environment variable together are legal
- **WHEN** `OTEL_NATS_TRACING_ENABLED` is set to any recognised value and `ConnectWithOptions`, `ConnectTLSWithOptions`, or `ConnectWithCredentialsWithOptions` is passed `WithTracingEnabled(v)`
- **THEN** the call succeeds and the tracing switch takes the variable's value, whatever the option said

#### Scenario: Option alone enables tracing
- **WHEN** no environment variable is set and `ConnectWithOptions(url, nil, WithTracingEnabled(true))` is called
- **THEN** the connection creates spans and propagates trace context, because the master defaults to `true`

#### Scenario: Option-carrying connection still observes a relay disable
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is tracing and the relay subsequently resolves `otel-nats-tracing` to `false`
- **THEN** the next operation emits no span

#### Scenario: Option-carrying connection is stopped by the master
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is tracing and the relay resolves `otel-instrumentation-go-tracing` to `false`
- **THEN** the next operation emits no span

#### Scenario: Invalid environment value fails construction
- **WHEN** `OTEL_NATS_TRACING_ENABLED` is set to `enabled`, `2` or the empty string
- **THEN** the connect variant returns an error wrapping `otelflags.ErrInvalidFlagValue` naming the variable and the value, and no `Conn` is created

### Requirement: Header-based trace propagation
When tracing is enabled, `Publish`/`PublishMsg` (core NATS) and JetStream publish operations SHALL inject the current span's W3C trace context into `nats.Header` via `HeaderCarrier`. `Subscribe`/`QueueSubscribe` handlers SHALL receive a `Msg` whose `.Context()` carries the trace extracted from the message headers.

#### Scenario: Publish and subscribe round-trip
- **WHEN** a message is published with an active span and tracing enabled, then received by a `Subscribe` handler
- **THEN** the handler's `Msg.Context()` contains a span context linked to the publisher's span via the propagated headers

### Requirement: Subscribe handler signature
`Conn.Subscribe` and `Conn.QueueSubscribe` SHALL accept a `MsgHandler` with signature `func(Msg)` (the wrapper's own `Msg` type), not the native `func(*nats.Msg)`.

#### Scenario: Handler receives wrapped Msg
- **WHEN** a subscription handler is registered via `Conn.Subscribe(subject, handler)`
- **THEN** `handler` is invoked with an `otelnats.Msg` exposing `.Msg` (native) and `.Context()` (extracted trace)

### Requirement: Request/Reply span pairing
`Conn.Request`, `RequestWithContext`, `RequestMsg`, and `RequestMsgWithContext` SHALL mirror the equivalent `nats.Conn` methods' signatures and behavior, while opening a CLIENT span for the request and a CONSUMER span for the reply. `Request` and `RequestMsg` (no `context.Context` parameter, mirroring `nats.Conn`) SHALL root their producer span at `context.Background()`; `RequestWithContext` and `RequestMsgWithContext` SHALL chain into the caller-supplied context. The reply's CONSUMER span defaults to a parent-child descendant of the CLIENT span; it becomes a span **link** to a distinct trace only in the less common case where the reply message itself already carries a valid, extractable W3C trace context in its headers (e.g. a responder that itself propagates trace context back).

#### Scenario: RequestWithContext chains into an existing trace
- **WHEN** `RequestWithContext(ctx, subject, data)` is called with `ctx` carrying an active span, and the reply carries no propagated trace headers (the common case, e.g. a plain `msg.Respond(...)`)
- **THEN** the request CLIENT span is a child of that active span, and the reply's CONSUMER span is started as a child of the CLIENT span's context (no `trace.Link` is added)

#### Scenario: Reply carries its own trace context
- **WHEN** the reply message's headers contain a valid, extractable W3C trace context
- **THEN** the reply's CONSUMER span is additionally given a `trace.Link` pointing to that extracted span context

#### Scenario: Request has no context parameter
- **WHEN** `Request(subject, data, timeout)` is called
- **THEN** its producer span is rooted at `context.Background()` rather than any ambient trace

### Requirement: JetStream consumer manager wrapping
`oteljetstream.JetStream` SHALL fully wrap `StreamConsumerManager`, and `oteljetstream.Stream` SHALL fully wrap `ConsumerManager`, with methods returning `Consumer` remaining trace-enabled wrappers over the underlying `jetstream.Consumer`.

#### Scenario: Creating a consumer through the wrapped manager
- **WHEN** `js.CreateConsumer(ctx, stream, cfg)` is called via the `oteljetstream.JetStream` wrapper
- **THEN** the returned `Consumer` is a trace-enabled wrapper whose `Consume`/`Messages`/`Fetch` methods extract trace context from message headers

### Requirement: Unsupported JetStream API surface
`oteljetstream` SHALL NOT wrap `PublishAsync`/`PublishMsgAsync` (these take no `context.Context` and return a non-blocking `PubAckFuture`, incompatible with this wrapper's context-propagation model). At the JetStream level, `oteljetstream.JetStream` SHALL NOT re-expose the `KeyValueManager` and `ObjectStoreManager` surfaces (whole key-value / object-store feature families outside a messaging-trace wrapper's scope) nor `Conn()`/`Options()`/`AccountInfo()`; these remain reachable via `JetStream.Unwrap()`. Push consumers and the consumer-admin operations `PauseConsumer`/`ResumeConsumer`/`UnpinConsumer` ARE wrapped — `nats.go` v1.50.0 exposes them (v1.38.0 did not), so they are re-exposed on the appropriate wrapper interfaces rather than left unsupported. `Stream.ResetConsumer`/`ResetConsumerToSequence` are NOT wrapped: they first appear in `nats.go` v1.52.0, beyond the v1.50.0 pin held to stay aligned with the downstream consumer policy (`flywindy/o11y`), so they are unsupported until a future policy-aligned nats.go bump re-introduces them.

#### Scenario: Async publish is not exposed
- **WHEN** a caller inspects the `oteljetstream` public API for an async-publish equivalent of `nats.go`'s `PublishAsync`
- **THEN** no such wrapped method exists — callers needing async publish must use the underlying `nats.go` JetStream context directly (via `JetStream.Unwrap()`), outside this wrapper's tracing model

#### Scenario: KeyValue / ObjectStore reached via the JetStream escape hatch
- **WHEN** a caller needs the `KeyValueManager` or `ObjectStoreManager` API that `oteljetstream.JetStream` does not re-expose
- **THEN** `JetStream.Unwrap()` returns the raw `jetstream.JetStream` for those calls, which are outside this messaging-trace wrapper's scope

#### Scenario: Consumer-admin operations are supported
- **WHEN** a caller pauses, resumes, or unpins a consumer through `oteljetstream.Stream`
- **THEN** `PauseConsumer`/`ResumeConsumer`/`UnpinConsumer` are available as direct passthrough methods (no `Unwrap()` required), since `nats.go` v1.50.0 exposes them

#### Scenario: Consumer reset is not exposed at the v1.50.0 pin
- **WHEN** a caller looks for `ResetConsumer`/`ResetConsumerToSequence` on `oteljetstream.Stream`
- **THEN** no such wrapped method exists — those `jetstream.Stream` methods first ship in `nats.go` v1.52.0, above the v1.50.0 pin held for downstream-policy alignment, and are re-exposed only when a future policy-aligned nats.go bump makes them available

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

### Requirement: NATS 2.11+ infrastructure trace events
`WithTraceDestination(subject)` SHALL cause `Publish`/`PublishMsg` to set the `Nats-Trace-Dest` header while tracing is enabled, so the NATS server emits infrastructure-level `TraceEvent` payloads to that subject. `SubscribeTraceEvents(conn, subject)` SHALL convert each `TraceEvent`'s `TraceHop`s into one point-in-time span per hop, started as a **parent-child** descendant of the span extracted from the embedded `traceparent` (not an OTel span link — unlike the Subscribe/Consume consumer path, which does use a link), and SHALL only emit spans when the connection's tracing gate is enabled (discarding events otherwise, while still supporting `Unsubscribe`).

#### Scenario: Trace destination configured
- **WHEN** a connection is created with `WithTraceDestination("nats.trace.events")` and tracing is enabled
- **THEN** every `Publish`/`PublishMsg` call carries the `Nats-Trace-Dest` header

#### Scenario: Consuming trace events with tracing disabled
- **WHEN** `SubscribeTraceEvents` is active but the connection's tracing gate is disabled
- **THEN** received `TraceEvent` payloads are discarded without emitting spans, and `Unsubscribe` still functions

### Requirement: Diagnostic logging via slog
`otelnats` SHALL use `log/slog` with no custom handler installed, logging server-address parse failures and trace-event successes at `DEBUG`, and trace-event unmarshal failures at `WARN`, using an `otelnats:` prefix. Because Go's default `slog` handler filters at `LevelInfo`, `DEBUG`-level logs are silent by default but `WARN`-level logs print to stderr by default. `oteljetstream` performs no `slog` logging of its own — all diagnostic logging for this capability lives in `otelnats`.

#### Scenario: Trace event unmarshal failure
- **WHEN** a message on the trace-event subject fails to unmarshal as a `TraceEvent`
- **THEN** a `WARN`-level log entry with the `otelnats:` prefix is emitted by default (visible on stderr with no custom handler) and no span is created for that message

### Requirement: ConsumeContext exposes the full consume-context lifecycle
`oteljetstream.ConsumeContext` SHALL expose the complete `jetstream.ConsumeContext` method set — `Stop()`, `Drain()`, and `Closed() <-chan struct{}` — as direct passthroughs to the underlying consume context. Because the surface is complete, no `Unwrap()` escape hatch is provided (removing the escape hatch previously present is a breaking change, permitted under the pre-1.0 `0.6.0` minor bump).

#### Scenario: Graceful drain awaits completion
- **WHEN** a caller invokes `cc.Drain()` on a `ConsumeContext` and then receives from `cc.Closed()`
- **THEN** buffered messages are processed by the handler and the `Closed()` channel closes once consuming has fully stopped, with no `Unwrap()` call required

### Requirement: Stream mirrors the full jetstream.Stream surface
`oteljetstream.Stream` SHALL re-expose every `jetstream.Stream` method available at the pinned `nats.go` v1.50.0. Consumer-returning methods remain trace-enabled wrappers; the message-management operations (`GetMsg`, `GetLastMsgForSubject`, `DeleteMsg`, `SecureDeleteMsg`, `Purge`) and the consumer-admin operations (`PauseConsumer`, `ResumeConsumer`, `UnpinConsumer`) SHALL be pure passthroughs — control-plane calls that carry no message payload, so no trace context applies. (`ResetConsumer`/`ResetConsumerToSequence` are excluded: they are not part of the `jetstream.Stream` surface until nats.go v1.52.0, above the policy-aligned pin.) Because the surface is complete for this pin, no `Unwrap()` escape hatch is provided (removing the escape hatch previously present is a breaking change, permitted under the pre-1.0 `0.6.0` minor bump).

#### Scenario: Fetching a stored message through the wrapper
- **WHEN** a caller invokes `stream.GetMsg(ctx, seq)` on an `oteljetstream.Stream`
- **THEN** the call returns the underlying `*RawStreamMsg` via a direct passthrough with no span created and no `Unwrap()` required

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

### Requirement: HeaderCarrier multi-value and canonical-fallback reads
`otelnats.HeaderCarrier` SHALL implement `propagation.ValuesGetter` in addition to `propagation.TextMapCarrier`. `Values(key)` SHALL return all values stored under the verbatim key when present, otherwise all values stored under `textproto.CanonicalMIMEHeaderKey(key)`. `Get(key)` SHALL follow the same lookup order (verbatim first, canonical fallback) and return the first value. The fallback SHALL trigger on key **absence**, not value emptiness — a verbatim key present with an empty value wins over a canonical entry, identically for `Get` and `Values`. `Set` SHALL remain unchanged, writing the verbatim key — the canonical fallback is a read-side compatibility measure only, so messages produced by current writers are unaffected. A third, case-insensitive (`strings.EqualFold`) scan over the header keys SHALL be tried only after both exact forms miss (added post-review in 0.7.0), with the same key-presence precedence.

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
