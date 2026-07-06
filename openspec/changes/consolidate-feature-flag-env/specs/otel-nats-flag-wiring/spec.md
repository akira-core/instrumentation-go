## ADDED Requirements

### Requirement: `otel-nats` uses the shared `internal/flags/` helper

`otel-nats/otelnats/env_flags.go` SHALL replace its local `natsTracingEnabled` resolver and the duplicated `envEnabledByDefault` helper with calls to a per-module `internal/flags/` package. The two-tier gate (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_NATS_TRACING_ENABLED`) SHALL remain unchanged in observable behaviour.

#### Scenario: Gate composes the two env vars

- **WHEN** any code path needs to know whether NATS instrumentation is enabled
- **THEN** the result SHALL be obtained from a package-level `natsGate *flags.Gate` whose resolver is `func() bool { return flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED") && flags.EnvEnabled("OTEL_NATS_TRACING_ENABLED") }`

#### Scenario: Local `envEnabledByDefault` is removed

- **WHEN** the change lands
- **THEN** `otel-nats/otelnats/env_flags.go` SHALL NOT define a private `envEnabledByDefault` function
- **AND** the file SHALL only contain the gate composition

### Requirement: `otelnats.Conn` uses strategy-split impls

`otelnats.Conn` SHALL hold a `connImpl` interface field and delegate every public method through the impl. The constructor (`Connect`, `Wrap`) SHALL pick `internal/direct.Conn` or `internal/traced.Conn` exactly once based on `natsGate.Enabled()`.

#### Scenario: Disabled mode returns direct impl

- **WHEN** `Connect(url, opts...)` is called with `OTEL_NATS_TRACING_ENABLED` unset
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete type `*internal/direct.Conn`
- **AND** `Conn.Publish`, `Subscribe`, `PublishMsg`, `Request`, `Drain`, `Close`, `JetStream`, and all other public methods SHALL delegate to the underlying `*nats.Conn` with no header inject and no span creation

#### Scenario: Enabled mode returns traced impl

- **WHEN** `Connect(url, opts...)` is called with both gates on
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete type `*internal/traced.Conn`
- **AND** `Publish` SHALL inject `traceparent` / `tracestate` headers via the configured propagator
- **AND** `Subscribe` callbacks SHALL receive a `MsgWithContext` carrying the extracted remote trace

#### Scenario: No `if tracingEnabled` in public methods

- **WHEN** a maintainer reads `otelnats.Conn.Publish`, `.PublishMsg`, `.Subscribe`, `.Request`, `.Drain`, `.Close`, `.JetStream`, or any other public method body
- **THEN** the body SHALL NOT contain a runtime `if c.tracingEnabled` (or equivalent) branch
- **AND** the body SHALL be a single delegation to `c.impl.<Method>(args...)`

### Requirement: `oteljetstream.Consumer` and `MessageBatch` use strategy-split impls

`oteljetstream.Consumer`, `oteljetstream.MessageBatch`, and any other JetStream wrapper that today branches on `tracingEnabled` SHALL migrate to strategy-split impls following the same pattern as `otelnats.Conn`.

#### Scenario: Disabled mode `MessageBatch` is the direct variant

- **WHEN** `OTEL_NATS_TRACING_ENABLED` is unset and a consumer calls `consumer.Messages()` then `iter.Next()`
- **THEN** the returned `MessageBatch` SHALL be backed by `internal/direct.MessageBatch`
- **AND** the goroutine SHALL only perform `jetstream.Msg → Msg` type adaptation (no span, no attribute slice build)
- **AND** `batch.Stop()` SHALL be a safe no-op for the span aspect but SHALL still release the goroutine

#### Scenario: Enabled mode `MessageBatch` is the traced variant

- **WHEN** both gates are on
- **THEN** the returned `MessageBatch` SHALL be backed by `internal/traced.MessageBatch`
- **AND** an in-flight span SHALL be created and ended by `batch.Stop()` or by channel close

#### Scenario: Public iterator method has no `tracingEnabled` branch

- **WHEN** a maintainer reads `oteljetstream.Consumer.Messages`, `oteljetstream.MessageBatch.Messages`, `oteljetstream.MessageBatch.Stop`
- **THEN** the body SHALL NOT contain a runtime `if tracingEnabled` branch
- **AND** the body SHALL delegate to the impl

### Requirement: `WithTraceDestination` and other functional options work in both modes

Functional options (`WithTracerProvider`, `WithPropagators`, `WithTraceDestination`) SHALL be parsed and stored regardless of gate state but SHALL only take effect when the traced impl is selected.

#### Scenario: Option set in disabled mode is silently ignored

- **WHEN** a caller passes `WithTraceDestination("infra.traces")` while gates are off
- **THEN** `Connect` SHALL NOT publish to the trace destination subject
- **AND** the call SHALL NOT error

#### Scenario: Option set in enabled mode applies

- **WHEN** the same option is passed while gates are on
- **THEN** the traced impl SHALL emit infrastructure trace events to the configured subject

### Requirement: `MsgWithContext` shape unchanged

The public `MsgWithContext` type and `Conn.Subscribe` callback signature SHALL be unchanged. The disabled impl SHALL still return a `MsgWithContext` carrying `context.Background()` (or the inherited subscriber context) so caller code that threads context through downstream operations does not need conditional handling.

#### Scenario: Disabled mode subscriber callback

- **WHEN** a subscriber registered while gates are off receives a message
- **THEN** the callback SHALL be invoked with a `MsgWithContext` whose `Context()` returns `context.Background()` (or a context without a remote span)
- **AND** the callback SHALL still see all native NATS message fields (Subject, Data, Header, Reply)
