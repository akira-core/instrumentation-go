## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The packages SHALL gate span creation and W3C header propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global, environment-only) and the dynamic flag `otel-nats-tracing` (module), the latter resolving through the module's `flags.Resolver` with `OTEL_NATS_TRACING_ENABLED` as its OpenFeature default value. An unset environment variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value SHALL enable. With no OpenFeature provider installed, the resolved behavior SHALL be identical to the release preceding this change.

The global switch SHALL be a hard kill switch: when it is disabled and no `WithTracingEnabled` option is present, no OpenFeature evaluation SHALL occur and no relay value SHALL enable tracing.

The dynamic value SHALL be read per operation rather than cached on the wrapper struct, so a `Conn` and everything derived from it (including `oteljetstream` wrappers, consumers, and long-lived message iterators) observes a relay change within the resolver's TTL without reconstruction. When the caller passes the `WithTracingEnabled(v bool)` option to a connect variant, that value SHALL be authoritative for the resulting `Conn` and everything derived from it, SHALL be resolved once at construction, and SHALL suppress OpenFeature evaluation for that connection entirely, per the shared `WithTracingEnabled` decision table in `shared-feature-flags`.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** all NATS/JetStream tracing is disabled regardless of `OTEL_NATS_TRACING_ENABLED` or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Both tiers on
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is truthy, the dynamic `otel-nats-tracing` value resolves to enabled, and no `WithTracingEnabled` option is passed
- **THEN** `Conn` and JetStream operations create spans and propagate W3C trace context in message headers

#### Scenario: No provider installed reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed and `OTEL_NATS_TRACING_ENABLED` is set to any value
- **THEN** the resolved behavior is identical to the release preceding this change

#### Scenario: Relay disables tracing on a running connection
- **WHEN** a `Conn` constructed without `WithTracingEnabled` is publishing with tracing enabled and the relay subsequently resolves `otel-nats-tracing` to `false`
- **THEN** publishes issued after the resolver's TTL expires delegate natively, emit no spans, and inject no trace headers

#### Scenario: Relay change reaches a long-lived JetStream consumer
- **WHEN** a `MessagesContext` iterator or a `MessageBatch` forwarder created before a relay change is still delivering messages after the resolver's TTL expires
- **THEN** messages delivered after the change are handled per the new value, without the consumer being recreated

#### Scenario: MessageBatch does not freeze the flag at Fetch time
- **WHEN** `Fetch` / `FetchBytes` / `FetchNoWait` returns a `MessageBatch` while tracing is on, and the relay later resolves `otel-nats-tracing` to off (or the reverse) and the TTL elapses before the batch finishes delivering
- **THEN** subsequent messages from that same batch follow the new value (no spans/extract when off; spans/extract when on), without recreating the consumer or the batch handle

#### Scenario: Traced hot path does not rebuild constant attrs per message
- **WHEN** a dynamic JetStream consume path is tracing on for consecutive messages on the same consumer
- **THEN** tracer, propagator, and receive base attributes that are constant for the traced connection are not reallocated solely to re-read the dynamic flag (the flag itself is still consulted per message to decide whether to emit)

#### Scenario: Option enables tracing with env off (unset or falsy)
- **WHEN** `ConnectWithOptions(url, nil, WithTracingEnabled(true))` is called with both tracing env vars unset or explicitly falsy
- **THEN** the connection creates spans and propagates trace context, and no OpenFeature evaluation is performed for that connection

#### Scenario: Option disables tracing despite truthy env vars and a truthy relay flag
- **WHEN** both env gates are truthy, the relay resolves `otel-nats-tracing` to `true`, and the caller passes `WithTracingEnabled(false)`
- **THEN** that connection performs no tracing (native delegation) for its lifetime regardless of any subsequent relay change, while other connections without the option follow the dynamic value
