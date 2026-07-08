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
The packages SHALL gate span creation and W3C header propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global) and `OTEL_NATS_TRACING_ENABLED` (module). Both SHALL default to disabled when unset; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value SHALL enable.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy
- **THEN** all NATS/JetStream tracing is disabled regardless of `OTEL_NATS_TRACING_ENABLED`

#### Scenario: Both flags on
- **WHEN** both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` are set to a truthy value
- **THEN** `Conn` and JetStream operations create spans and propagate W3C trace context in message headers

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
`oteljetstream.MessageBatch` SHALL expose `Stop()` to release the batch's internal goroutine and end any in-flight span. Callers that fully drain `Messages()` until the channel closes are not required to call `Stop()`; callers that `break`/`return` before the channel closes SHALL call `Stop()` (typically via `defer`) to avoid leaking the goroutine and the in-flight consumer span.

#### Scenario: Full drain
- **WHEN** a caller ranges over `batch.Messages()` until the channel closes naturally
- **THEN** the batch's goroutine and span are already released without an explicit `Stop()` call

#### Scenario: Early break
- **WHEN** a caller `break`s out of the `range batch.Messages()` loop before the channel closes
- **THEN** an explicit (typically deferred) `batch.Stop()` call is required to end the in-flight span and stop the goroutine; omitting it leaks both

### Requirement: Deliver spans for the NATS service graph
When tracing is enabled and `OTEL_EXPORTER_OTLP_ENDPOINT` is set to a valid full URL (HTTP) or `host:port` (gRPC), `Connect` SHALL initialize an independent deliver-span `TracerProvider` with `service.name` set to `nc.ConnectedUrlRedacted()` (the negotiated connection URL with credentials redacted), falling back to `"nats://" + nc.ConnectedAddr()` only when `ConnectedUrlRedacted()` returns an empty string. Publish/consume operations SHALL emit CONSUMER deliver spans representing NATS as a broker node.

#### Scenario: Endpoint configured and tracing enabled
- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317` (gRPC form) and both tracing flags are enabled
- **THEN** `Connect` initializes the deliver-span provider automatically with no further configuration required

#### Scenario: Tracing disabled
- **WHEN** either tracing flag is disabled, regardless of `OTEL_EXPORTER_OTLP_ENDPOINT`
- **THEN** `Connect` never initializes the deliver-span `TracerProvider`

### Requirement: NATS 2.11+ infrastructure trace events
`WithTraceDestination(subject)` SHALL cause `Publish`/`PublishMsg` to set the `Nats-Trace-Dest` header while tracing is enabled, so the NATS server emits infrastructure-level `TraceEvent` payloads to that subject. `SubscribeTraceEvents(conn, subject)` SHALL convert each `TraceEvent`'s `TraceHop`s into one point-in-time span per hop, started as a **parent-child** descendant of the span extracted from the embedded `traceparent` (not an OTel span link — unlike the Subscribe/Consume consumer path, which does use a link), and SHALL only emit spans when the connection's tracing gate is enabled (discarding events otherwise, while still supporting `Unsubscribe`).

#### Scenario: Trace destination configured
- **WHEN** a connection is created with `WithTraceDestination("nats.trace.events")` and tracing is enabled
- **THEN** every `Publish`/`PublishMsg` call carries the `Nats-Trace-Dest` header

#### Scenario: Consuming trace events with tracing disabled
- **WHEN** `SubscribeTraceEvents` is active but the connection's tracing gate is disabled
- **THEN** received `TraceEvent` payloads are discarded without emitting spans, and `Unsubscribe` still functions

### Requirement: Diagnostic logging via slog
`otelnats` SHALL use `log/slog` with no custom handler installed, logging server-address parse failures and deliver-tracer/trace-event successes at `DEBUG`, and deliver-tracer init failures or trace-event unmarshal failures at `WARN`, using an `otelnats:` prefix. Because Go's default `slog` handler filters at `LevelInfo`, `DEBUG`-level logs are silent by default but `WARN`-level logs print to stderr by default. `oteljetstream` performs no `slog` logging of its own — all diagnostic logging for this capability lives in `otelnats`.

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
`oteljetstream.Consumer.Next` and `oteljetstream.MessagesContext.Next` SHALL both return a `context.Context` bearing the wrapper's local consumer receive span (linked to the producer's extracted trace context), so downstream spans created from the returned context nest under the consumer's receive span rather than directly under the remote producer span. This matches the context semantics of the `Consume` handler path. For the single-shot `Consumer.Next`, the receive span is ended immediately (a single fetch has no processing-scope boundary), but the returned context still carries that span so child spans parent to it via its still-valid `SpanContext`.

#### Scenario: Downstream spans nest under the consumer receive span
- **WHEN** `cons.Next(ctx)` returns a message with tracing enabled and the caller starts a downstream span from the returned context
- **THEN** the downstream span is a child of the wrapper's local consumer receive span (which is linked to the producer), identical in shape to what `Messages().Next` and the `Consume` handler produce

