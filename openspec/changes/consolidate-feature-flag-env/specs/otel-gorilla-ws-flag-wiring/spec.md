## ADDED Requirements

### Requirement: `otel-gorilla-ws` uses the shared `internal/flags/` helper

`otel-gorilla-ws/env_flags.go` SHALL replace its local `wsTracingEnabled` resolver and the duplicated `envEnabledByDefault` helper with calls to a per-module `internal/flags/` package. The two-tier gate (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_GORILLA_WS_TRACING_ENABLED`) SHALL remain unchanged in observable behaviour.

#### Scenario: Gate composes the two env vars

- **WHEN** any code path needs to know whether WebSocket instrumentation is enabled
- **THEN** the result SHALL be obtained from a package-level `wsGate *flags.Gate` whose resolver is `func() bool { return flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED") && flags.EnvEnabled("OTEL_GORILLA_WS_TRACING_ENABLED") }`

#### Scenario: Local `envEnabledByDefault` is removed

- **WHEN** the change lands
- **THEN** `otel-gorilla-ws/env_flags.go` SHALL NOT define a private `envEnabledByDefault` function

### Requirement: `otelgorillaws.Conn` uses strategy-split impls

`otelgorillaws.Conn` SHALL hold a `shared.ConnImpl` interface field and delegate every public method through the impl. Constructor selects `internal/direct.Conn` (when the env feature flag is off) or `internal/traced.Conn` (when on) exactly once, **after** WebSocket subprotocol negotiation completes. The traced impl carries an internal `PropagationEnabled bool` field (set at construction time from subprotocol negotiation) that gates envelope wrap/unwrap; spans are still emitted when this field is false. The three observable states map cleanly to two impl flavours:

| Env gate | Subprotocol negotiated | Impl picked | Spans | Envelope on wire |
|---|---|---|---|---|
| off | any | `direct.Conn` | none | none |
| on | no | `traced.Conn` with `PropagationEnabled=false` | emitted | none |
| on | yes | `traced.Conn` with `PropagationEnabled=true` | emitted | wrapped |

This mirrors the otel-nats pattern where tracing-on + propagation-off produces a span without modifying the wire (see `otel-nats-flag-wiring`).

#### Scenario: Gate off → direct impl

- **WHEN** `Dial(url, header, opts...)` or `Upgrade(w, r, header, opts...)` is called with `OTEL_GORILLA_WS_TRACING_ENABLED` unset (or the global master switch off)
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete type `*internal/direct.Conn`
- **AND** `ReadMessage`, `WriteMessage`, `ReadJSON`, `WriteJSON`, `Close`, and all other public methods SHALL delegate to the underlying `*websocket.Conn` with no JSON envelope build and no span creation

#### Scenario: Gate on but subprotocol not negotiated → traced impl with envelope disabled

- **WHEN** `Dial` (or `Upgrade`) is called with both gates on but the peer does not advertise the OTel `Sec-WebSocket-Protocol`
- **THEN** the returned `*Conn` SHALL hold a `*internal/traced.Conn` whose internal `PropagationEnabled` is `false`
- **AND** `WriteMessage` SHALL emit a `websocket.send` PRODUCER span via the configured tracer
- **AND** `WriteMessage` SHALL pass the raw payload through to `*websocket.Conn.WriteMessage` with NO envelope wrap and NO `traceparent` / `tracestate` injection
- **AND** `ReadMessage` SHALL emit a `websocket.receive` CONSUMER span
- **AND** `ReadMessage` SHALL return the raw bytes from `*websocket.Conn.ReadMessage` with NO envelope parsing and NO `propagator.Extract` call (the returned ctx carries no remote trace)

#### Scenario: Gate on and subprotocol negotiated → traced impl with envelope enabled

- **WHEN** both gates are on AND the peer accepts the OTel subprotocol (server replies with `otel-ws` or `otel-ws+<app-proto>`)
- **THEN** the returned `*Conn` SHALL hold a `*internal/traced.Conn` whose internal `PropagationEnabled` is `true`
- **AND** `WriteMessage` SHALL wrap the payload in the JSON envelope containing `traceparent`, `tracestate`, and the original `payload`
- **AND** `ReadMessage` SHALL parse the envelope, extract trace context, and return the original payload bytes plus a ctx carrying the extracted remote trace

#### Scenario: No `if tracingEnabled` in public methods

- **WHEN** a maintainer reads any public method body of `otelgorillaws.Conn`
- **THEN** the body SHALL NOT contain a runtime `if c.tracingEnabled` branch
- **AND** the body SHALL be a single delegation to `c.impl.<Method>(args...)` (modulo error wrapping or argument adaptation)

### Requirement: Subprotocol negotiation runtime override remains

The existing scenarios A through E for `Sec-WebSocket-Protocol` negotiation SHALL continue to behave identically. Strategy-split impls SHALL be selected in the same control flow that previously set `tracingEnabled bool`.

#### Scenario: Scenario A — both sides advertise OTel subprotocol

- **WHEN** client and server both list the OTel subprotocol and negotiation succeeds (env gate on)
- **THEN** the wrapper SHALL select the traced impl on both sides
- **AND** the traced impl's `PropagationEnabled` SHALL be `true` (envelope wrap/unwrap active)

#### Scenario: Scenario E — server downgrades to plain subprotocol

- **WHEN** server responds without the OTel subprotocol and the client env gate is on
- **THEN** the wrapper SHALL select the traced impl on the client side with `PropagationEnabled=false`
- **AND** the connection SHALL emit spans locally but use plain (envelope-free) wire framing — interoperable with a legacy non-otel server
- **AND** when the client env gate is off, the wrapper SHALL fall through to the direct impl and function as a plain `*websocket.Conn`

### Requirement: `ReadMessage` return signature preserves trace context

`Conn.ReadMessage` SHALL return `(messageType int, p []byte, ctx context.Context, err error)` (or the existing equivalent shape) in **both** direct and traced modes. Caller code SHALL NOT need to branch on impl flavour.

#### Scenario: Direct mode returns background context

- **WHEN** a caller calls `ReadMessage` on a direct-impl connection
- **THEN** the returned `ctx` SHALL be a context without a remote span (e.g. `context.Background()` or the inherited reader context)
- **AND** `p` SHALL be the raw message bytes from the upstream `*websocket.Conn`

#### Scenario: Traced impl with envelope disabled returns inherited context

- **WHEN** a caller calls `ReadMessage` on a `traced.Conn` whose `PropagationEnabled` is `false` (env gate on, subprotocol not negotiated)
- **THEN** the returned `ctx` SHALL be the caller-supplied ctx (no `propagator.Extract` performed, no remote span attached)
- **AND** `p` SHALL be the raw message bytes (envelope NOT parsed)
- **AND** a `websocket.receive` CONSUMER span SHALL still be emitted

#### Scenario: Traced impl with envelope enabled returns context carrying remote span

- **WHEN** a caller calls `ReadMessage` on a `traced.Conn` whose `PropagationEnabled` is `true` and receives a wrapped message
- **THEN** the returned `ctx` SHALL carry the extracted remote trace (parented to the producer span)
- **AND** `p` SHALL be the decoded `payload` bytes (envelope stripped)
