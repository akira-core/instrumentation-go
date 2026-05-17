## ADDED Requirements

### Requirement: `otel-nats` uses the shared `internal/flags/` helper

`otel-nats/otelnats/env_flags.go` SHALL replace its local `natsTracingEnabled` resolver and the duplicated `envEnabledByDefault` helper with calls to a per-module `internal/flags/` package. The gate SHALL be extended from two-tier (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_NATS_TRACING_ENABLED`) to three-tier by adding a new `OTEL_NATS_PROPAGATION_ENABLED` flag that controls W3C header inject/extract independently of the wrapper-span gate.

#### Scenario: Tracing gate composes the two tracing env vars

- **WHEN** any code path needs to know whether NATS wrapper spans should be emitted
- **THEN** the result SHALL be obtained from a package-level `natsGate *flags.Gate` whose resolver is `func() bool { return flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED") && flags.EnvEnabled("OTEL_NATS_TRACING_ENABLED") }`

#### Scenario: Propagation gate composes all three env vars with default-off semantics

- **WHEN** any code path needs to know whether the NATS wrapper should inject `traceparent` / `tracestate` headers on publish and extract them on subscribe
- **THEN** the result SHALL be obtained from a package-level `natsPropagationGate *flags.Gate` whose resolver returns `true` only when `natsGate.Enabled()` is `true` AND the `OTEL_NATS_PROPAGATION_ENABLED` env var is explicitly set to a truthy value (any string other than the falsy set `"0"`, `"false"`, `"no"`, `"off"`, case-insensitive and whitespace-trimmed)
- **AND** the resolver SHALL return `false` when `OTEL_NATS_PROPAGATION_ENABLED` is unset (default-OFF posture, consistent with the rest of the universal flag surface)
- **AND** the resolver SHALL return `false` when `OTEL_NATS_PROPAGATION_ENABLED` is explicitly set to a falsy value
- **AND** the resolver SHALL return `false` whenever `natsGate.Enabled()` is `false`, regardless of the propagation env var value (the tracing gate is a hard prerequisite)

#### Scenario: Local `envEnabledByDefault` is removed

- **WHEN** the change lands
- **THEN** `otel-nats/otelnats/env_flags.go` SHALL NOT define a private `envEnabledByDefault` function
- **AND** the file SHALL only contain the two gate compositions (tracing + propagation) and any thin accessor wrappers required for the strategy-split constructor

### Requirement: Propagation gate honours all four on/off combinations

The combined `(natsGate, natsPropagationGate)` decision matrix SHALL produce one of three observable behaviours on publish and one matching behaviour on subscribe, summarised below.

| Tracing | Propagation env | Resulting wrapper-span behaviour | Resulting wire-propagation behaviour |
|---|---|---|---|
| off | any value (unset / true / false) | no wrapper span, direct delegate to upstream | no header inject, no header extract |
| on | unset (default OFF) | wrapper span created | NO header inject on publish, NO extract on subscribe |
| on | truthy | wrapper span created | `traceparent` / `tracestate` injected on publish, extracted on subscribe |
| on | falsy | wrapper span created | NO header inject on publish, NO extract on subscribe |

#### Scenario: Tracing off, propagation env unset

- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset (or falsy) and `OTEL_NATS_PROPAGATION_ENABLED` is unset
- **THEN** `Conn.Publish(ctx, subject, data)` SHALL delegate directly to `*nats.Conn.Publish` with the caller-supplied `Header` unchanged
- **AND** the headers SHALL NOT contain a `traceparent` key added by the wrapper
- **AND** no wrapper span SHALL be created

#### Scenario: Tracing off, propagation env explicitly on

- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset and `OTEL_NATS_PROPAGATION_ENABLED=true`
- **THEN** propagation SHALL still be disabled (tracing gate is the hard prerequisite)
- **AND** the behaviour SHALL match the "both off" row of the table exactly

#### Scenario: Tracing on, propagation env unset (default OFF)

- **WHEN** both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` and `OTEL_NATS_TRACING_ENABLED=true` are set, and `OTEL_NATS_PROPAGATION_ENABLED` is unset
- **THEN** `Conn.Publish(ctx, ...)` SHALL emit a PRODUCER span via the configured `TracerProvider`
- **AND** the outgoing `Header` SHALL NOT have `traceparent` / `tracestate` keys added by the wrapper (existing caller-supplied keys SHALL be preserved verbatim)
- **AND** `Conn.Subscribe` callbacks SHALL receive a `Msg` whose `Ctx` is the subscriber's parent context (no extract attempted, no link added to the consumer span)

#### Scenario: Tracing on, propagation env explicitly truthy

- **WHEN** both tracing gates are on and `OTEL_NATS_PROPAGATION_ENABLED=true` (or any other truthy value)
- **THEN** `Conn.Publish(ctx, ...)` SHALL emit a PRODUCER span via the configured `TracerProvider`
- **AND** the outgoing `Header` SHALL contain `traceparent` (and `tracestate` if present in the active span context) injected by the configured `propagation.TextMapPropagator`
- **AND** `Conn.Subscribe` callbacks SHALL receive a `Msg` whose `Ctx` carries the extracted remote span context when the incoming `Header` contains a `traceparent`

#### Scenario: Tracing on, propagation env explicitly falsy

- **WHEN** both tracing gates are on but `OTEL_NATS_PROPAGATION_ENABLED=false` (or `"0"`, `"no"`, `"off"`)
- **THEN** the wrapper SHALL behave identically to the "tracing on, propagation env unset" scenario above (PRODUCER span emitted, no header inject, no consumer-side extract)
- **AND** the explicit-falsy and unset cases SHALL be observationally indistinguishable

#### Scenario: Both gates flip from off → on at runtime

- **WHEN** a process starts with both gates off, then env vars are mutated and `natsPropagationGate.ResetForTest()` is called from a test
- **THEN** subsequent calls SHALL observe the new gate values
- **AND** in production (no ResetForTest call), the cached value from process start SHALL be retained — env mutations after the first gate read SHALL be ignored, consistent with `natsGate` semantics

### Requirement: `otelnats.Conn` uses strategy-split impls

`otelnats.Conn` SHALL hold a `connImpl` interface field and delegate every public method through the impl. The constructor (`Connect`, `Wrap`) SHALL pick a direct impl or a traced impl exactly once based on `natsGate.Enabled()`.

**Layout variants** — two are acceptable; the choice is structural, not behavioural:

| Variant | Layout | Disabled-mode invariant enforcement |
|---|---|---|
| **Package-level split** | `internal/direct.Conn` (separate Go package) + `internal/traced.Conn` (separate Go package). Used by `otel-mongo` Collection / Cursor / SingleResult / ChangeStream and `otel-gorilla-ws` Conn. | Compiler-enforced by `internal/` package boundary — `internal/direct/` has zero `otel/sdk` / `otel/exporters` imports. |
| **File-level split** | `directConn` + `tracedConn` types in the same `otelnats` package, defined in `conn_direct.go` / `conn_traced.go`. Used by `otel-nats` core Conn and JetStream consumer paths. | Review-enforced + CI grep on `conn_direct.go` / `*_direct.go` files to ensure they import zero `otel/sdk` / `otel/exporters`. Same package boundary as the facade, but the file-name convention plus the CI check approximate the same isolation guarantee. |

Rationale for the file-level variant on `otel-nats`: the package surface is large (`otelnats.Conn` + `oteljetstream.{JetStream, Stream, Consumer, MessageBatch}` ≈ 1500+ LOC across 6 strategy pairs). Promoting all six pairs to package-level split would multiply the package count by 3 (`internal/{shared,direct,traced}` per pair) and require moving shared types (`HeaderCarrier`, `MsgHandler`, `Msg`) into a fourth package to avoid cycles. The file-level split achieves the same functional intent — one constructor-time impl selection, zero per-call gate reads, public methods are single-line delegates — with materially less ceremony. The compiler-checked package-boundary guarantee is replaced with a CI grep on `*_direct.go` files; together with code review this is the same level of structural backing that `instrumentation-feature-flags` Scenario "Compiler-enforced isolation" accepts for the `internal/direct/` case (which itself says the SDK-import check is "encouraged in follow-up").

#### Scenario: Disabled mode returns direct impl

- **WHEN** `Connect(url, opts...)` is called with `OTEL_NATS_TRACING_ENABLED` unset
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete type `*directConn` (file-level variant) or `*internal/direct.Conn` (package-level variant)
- **AND** `Conn.Publish`, `Subscribe`, `PublishMsg`, `Request`, `Drain`, `Close`, `JetStream`, and all other public methods SHALL delegate to the underlying `*nats.Conn` with no header inject and no span creation

#### Scenario: Enabled mode with explicit propagation returns traced impl with propagation

- **WHEN** `Connect(url, opts...)` is called with both tracing gates on and `OTEL_NATS_PROPAGATION_ENABLED` explicitly truthy
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete type `*tracedConn` (file-level) or `*internal/traced.Conn` (package-level) whose internally-cached `propagationEnabled` is `true`
- **AND** `Publish` SHALL inject `traceparent` / `tracestate` headers via the configured propagator
- **AND** `Subscribe` callbacks SHALL receive a `MsgWithContext` carrying the extracted remote trace

#### Scenario: Tracing on, propagation default off returns traced impl without propagation

- **WHEN** `Connect(url, opts...)` is called with both tracing gates on and `OTEL_NATS_PROPAGATION_ENABLED` unset (or explicitly falsy)
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete traced type whose internally-cached `propagationEnabled` is `false`
- **AND** `Publish` SHALL emit a PRODUCER span but SHALL NOT inject `traceparent` / `tracestate` headers
- **AND** `Subscribe` SHALL emit a CONSUMER span but SHALL NOT call `propagator.Extract` (the consumer span SHALL have no remote-parent link from header propagation)

#### Scenario: No `if tracingEnabled` in public methods

- **WHEN** a maintainer reads `otelnats.Conn.Publish`, `.PublishMsg`, `.Subscribe`, `.Request`, `.Drain`, `.Close`, `.JetStream`, or any other public method body
- **THEN** the body SHALL NOT contain a runtime `if c.tracingEnabled` (or equivalent) branch
- **AND** the body SHALL be a single delegation to `c.impl.<Method>(args...)`

#### Scenario: File-level variant — direct files import zero OTel SDK

- **WHEN** the file-level variant is used (any file matching `*_direct.go` in `otel-nats/otelnats/` or `otel-nats/oteljetstream/`)
- **THEN** the file SHALL NOT contain a top-level import string matching `"go.opentelemetry.io/otel/sdk/...` or `"go.opentelemetry.io/otel/exporters/...`
- **AND** the same `drift-check` CI job that gates `internal/direct/` package-level isolation SHALL include this file-level grep step

### Requirement: `oteljetstream.Consumer` and `MessageBatch` use strategy-split impls

`oteljetstream.Consumer`, `oteljetstream.MessageBatch`, and any other JetStream wrapper that today branches on `tracingEnabled` SHALL migrate to strategy-split impls following the same pattern (and the same file-level vs package-level variant choice) as `otelnats.Conn`.

#### Scenario: Disabled mode `MessageBatch` is the direct variant

- **WHEN** `OTEL_NATS_TRACING_ENABLED` is unset and a consumer calls `consumer.Messages()` then `iter.Next()`
- **THEN** the returned `MessageBatch` SHALL be backed by the direct variant (`directMessageBatch` for file-level layout or `internal/direct.MessageBatch` for package-level)
- **AND** the goroutine SHALL only perform `jetstream.Msg → Msg` type adaptation (no span, no attribute slice build)
- **AND** `batch.Stop()` SHALL be a safe no-op for the span aspect but SHALL still release the goroutine

#### Scenario: Enabled mode `MessageBatch` is the traced variant

- **WHEN** both gates are on
- **THEN** the returned `MessageBatch` SHALL be backed by the traced variant (`tracedMessageBatch` or `internal/traced.MessageBatch`)
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
