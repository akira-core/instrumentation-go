## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The packages SHALL gate span creation and W3C header propagation behind a conjunction of tiers, evaluated with short-circuit semantics:

```
tracing := gate1 && EnvEnabled(OTEL_NATS_TRACING_ENABLED) && resolver.Allowed(idxTracing)
```

`gate1` SHALL be `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` or the `WithTracingEnabled(v bool)` option, which are mutually exclusive per the shared `shared-feature-flags` rule; supplying both SHALL make the connect variant return an error.

The relay flag `otel-nats-tracing` SHALL be resolved with an evaluation default of `true` and SHALL only ever subtract: a `false` on the relay disables tracing the deployment enabled; no relay value SHALL enable tracing the deployment left off. When `OTEL_NATS_TRACING_ENABLED` is unset or falsy the module SHALL NOT consult the resolver at all.

Environment truthiness SHALL follow the allow-list in `shared-feature-flags`: only `1`, `true`, `yes`, `on` (trimmed, case-insensitive) enable; every other value, including the empty string, disables.

Which implementations exist SHALL be decided at construction by the static part of the conjunction, `gate1 && EnvEnabled(OTEL_NATS_TRACING_ENABLED)`, matching the other three modules. When it is false only `directConn` / `directJSImpl` SHALL be constructed, because the relay can only revoke and therefore can never make the traced implementation reachable.

The relay verdict SHALL be read per operation rather than cached on the wrapper struct, so a `Conn` and everything derived from it — `oteljetstream` wrappers, consumers, long-lived message iterators and batch forwarders — observes a revocation within the resolver's TTL without reconstruction. `WithTracingEnabled` SHALL NOT make a connection static: a connection carrying it still reads the relay verdict per operation and still stops when the relay revokes.

#### Scenario: First tier off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** all NATS/JetStream tracing is disabled regardless of `OTEL_NATS_TRACING_ENABLED` or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Module switch off skips the relay
- **WHEN** `gate1` is enabled and `OTEL_NATS_TRACING_ENABLED` is unset or falsy
- **THEN** no spans are created, no trace headers are injected, and no `Client.Boolean` call is made

#### Scenario: Module switch off constructs only the passthrough implementation
- **WHEN** a `Conn` is constructed while `gate1` is enabled and `OTEL_NATS_TRACING_ENABLED` is unset or falsy
- **THEN** only `directConn` is built, `oteljetstream.New` on that `Conn` returns `directJSImpl`, and no relay evaluation is ever performed for that connection

#### Scenario: Both environment tiers on and the relay does not interfere
- **WHEN** `gate1` is enabled, `OTEL_NATS_TRACING_ENABLED` is truthy, and the relay resolves `otel-nats-tracing` to `true` or has no such flag
- **THEN** `Conn` and JetStream operations create spans and propagate W3C trace context in message headers

#### Scenario: No provider installed reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed and `OTEL_NATS_TRACING_ENABLED` is set to an allow-list value
- **THEN** the resolved behavior is identical to the release preceding this change

#### Scenario: Relay revokes tracing on a running connection
- **WHEN** a `Conn` is publishing with tracing enabled and the relay subsequently resolves `otel-nats-tracing` to `false`
- **THEN** publishes issued after the resolver's TTL expires delegate natively, emit no spans, and inject no trace headers

#### Scenario: Relay cannot enable tracing the deployment left off
- **WHEN** `gate1` is enabled, `OTEL_NATS_TRACING_ENABLED` is unset, and the relay resolves `otel-nats-tracing` to `true`
- **THEN** no spans are created and no evaluation is performed

#### Scenario: Relay revocation reaches a long-lived JetStream consumer
- **WHEN** a `MessagesContext` iterator or a `MessageBatch` forwarder created before a revocation is still delivering messages after the resolver's TTL expires
- **THEN** messages delivered after the revocation are handled natively, with no spans and no header extraction, without the consumer being recreated

#### Scenario: MessageBatch does not freeze the flag at Fetch time
- **WHEN** `Fetch` / `FetchBytes` / `FetchNoWait` returns a `MessageBatch` while tracing is on, and the relay subsequently revokes `otel-nats-tracing` before the batch finishes delivering
- **THEN** subsequent messages from that same batch follow the new verdict, without recreating the consumer or the batch handle

#### Scenario: Traced hot path does not rebuild constant attrs per message
- **WHEN** a dynamic JetStream consume path is tracing on for consecutive messages on the same consumer
- **THEN** tracer, propagator, and receive base attributes that are constant for the traced connection are not reallocated solely to re-read the gate (the gate itself is still consulted per message to decide whether to emit)

#### Scenario: Option and environment variable together are rejected
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set to any value and `ConnectWithOptions`, `ConnectTLSWithOptions`, or `ConnectWithCredentialsWithOptions` is passed `WithTracingEnabled(v)` for either value of `v`
- **THEN** the call returns an error matching the module's tracing-conflict sentinel and no `Conn` is created

#### Scenario: Option supplies the first tier when the environment is silent
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset, `OTEL_NATS_TRACING_ENABLED` is truthy, and `ConnectWithOptions(url, nil, WithTracingEnabled(true))` is called
- **THEN** the connection creates spans and propagates trace context

#### Scenario: Option-carrying connection still obeys a revocation
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is tracing and the relay subsequently resolves `otel-nats-tracing` to `false`
- **THEN** operations issued after the resolver's TTL expires emit no spans, exactly as on a connection constructed from the environment variable
