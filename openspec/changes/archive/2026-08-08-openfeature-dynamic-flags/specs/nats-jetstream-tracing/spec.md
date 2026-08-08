## MODIFIED Requirements

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
