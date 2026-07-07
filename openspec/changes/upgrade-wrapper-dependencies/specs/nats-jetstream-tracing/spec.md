## MODIFIED Requirements

### Requirement: Unsupported JetStream API surface
`oteljetstream` SHALL NOT wrap `PublishAsync`/`PublishMsgAsync` (these take no `context.Context` and return a non-blocking `PubAckFuture`, incompatible with this wrapper's context-propagation model). At the JetStream level, `oteljetstream.JetStream` SHALL NOT re-expose the `KeyValueManager` and `ObjectStoreManager` surfaces (whole key-value / object-store feature families outside a messaging-trace wrapper's scope) nor `Conn()`/`Options()`/`AccountInfo()`; these remain reachable via `JetStream.Unwrap()`. Push consumers and the consumer-admin operations `PauseConsumer`/`ResumeConsumer`/`UnpinConsumer`/`ResetConsumer`/`ResetConsumerToSequence` ARE wrapped — `nats.go` v1.52.0 exposes them (v1.38.0 did not), so they are re-exposed on the appropriate wrapper interfaces rather than left unsupported.

#### Scenario: Async publish is not exposed
- **WHEN** a caller inspects the `oteljetstream` public API for an async-publish equivalent of `nats.go`'s `PublishAsync`
- **THEN** no such wrapped method exists — callers needing async publish must use the underlying `nats.go` JetStream context directly (via `JetStream.Unwrap()`), outside this wrapper's tracing model

#### Scenario: KeyValue / ObjectStore reached via the JetStream escape hatch
- **WHEN** a caller needs the `KeyValueManager` or `ObjectStoreManager` API that `oteljetstream.JetStream` does not re-expose
- **THEN** `JetStream.Unwrap()` returns the raw `jetstream.JetStream` for those calls, which are outside this messaging-trace wrapper's scope

#### Scenario: Consumer-admin operations are supported
- **WHEN** a caller pauses, resumes, unpins, or resets a consumer through `oteljetstream.Stream`
- **THEN** `PauseConsumer`/`ResumeConsumer`/`UnpinConsumer`/`ResetConsumer`/`ResetConsumerToSequence` are available as direct passthrough methods (no `Unwrap()` required), since `nats.go` v1.52.0 exposes them

## ADDED Requirements

### Requirement: ConsumeContext exposes the full consume-context lifecycle
`oteljetstream.ConsumeContext` SHALL expose the complete `jetstream.ConsumeContext` method set — `Stop()`, `Drain()`, and `Closed() <-chan struct{}` — as direct passthroughs to the underlying consume context. Because the surface is complete, no `Unwrap()` escape hatch is provided (removing the escape hatch previously present is a breaking change, permitted under the pre-1.0 `0.6.0` minor bump).

#### Scenario: Graceful drain awaits completion
- **WHEN** a caller invokes `cc.Drain()` on a `ConsumeContext` and then receives from `cc.Closed()`
- **THEN** buffered messages are processed by the handler and the `Closed()` channel closes once consuming has fully stopped, with no `Unwrap()` call required

### Requirement: Stream mirrors the full jetstream.Stream surface
`oteljetstream.Stream` SHALL re-expose every `jetstream.Stream` method. Consumer-returning methods remain trace-enabled wrappers; the message-management operations (`GetMsg`, `GetLastMsgForSubject`, `DeleteMsg`, `SecureDeleteMsg`, `Purge`) and the consumer-admin operations (`PauseConsumer`, `ResumeConsumer`, `UnpinConsumer`, `ResetConsumer`, `ResetConsumerToSequence`) SHALL be pure passthroughs — control-plane calls that carry no message payload, so no trace context applies. Because the surface is complete, no `Unwrap()` escape hatch is provided (removing the escape hatch previously present is a breaking change, permitted under the pre-1.0 `0.6.0` minor bump).

#### Scenario: Fetching a stored message through the wrapper
- **WHEN** a caller invokes `stream.GetMsg(ctx, seq)` on an `oteljetstream.Stream`
- **THEN** the call returns the underlying `*RawStreamMsg` via a direct passthrough with no span created and no `Unwrap()` required

### Requirement: Single-fetch and iterator Next return equivalent trace context
`oteljetstream.Consumer.Next` and `oteljetstream.MessagesContext.Next` SHALL both return a `context.Context` bearing the wrapper's local consumer receive span (linked to the producer's extracted trace context), so downstream spans created from the returned context nest under the consumer's receive span rather than directly under the remote producer span. This matches the context semantics of the `Consume` handler path. For the single-shot `Consumer.Next`, the receive span is ended immediately (a single fetch has no processing-scope boundary), but the returned context still carries that span so child spans parent to it via its still-valid `SpanContext`.

#### Scenario: Downstream spans nest under the consumer receive span
- **WHEN** `cons.Next(ctx)` returns a message with tracing enabled and the caller starts a downstream span from the returned context
- **THEN** the downstream span is a child of the wrapper's local consumer receive span (which is linked to the producer), identical in shape to what `Messages().Next` and the `Consume` handler produce
