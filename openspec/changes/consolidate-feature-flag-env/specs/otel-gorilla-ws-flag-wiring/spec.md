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

`otelgorillaws.Conn` SHALL hold a `connImpl` interface field and delegate every public method through the impl. Constructor selects `internal/direct.Conn` or `internal/traced.Conn` exactly once, **after** WebSocket subprotocol negotiation completes.

#### Scenario: Gate off → direct impl

- **WHEN** `Dial(url, header, opts...)` or `Upgrade(w, r, header, opts...)` is called with `OTEL_GORILLA_WS_TRACING_ENABLED` unset
- **THEN** the returned `*Conn` SHALL hold an `impl` of concrete type `*internal/direct.Conn`
- **AND** `ReadMessage`, `WriteMessage`, `ReadJSON`, `WriteJSON`, `Close`, and all other public methods SHALL delegate to the underlying `*websocket.Conn` with no JSON envelope build and no span creation

#### Scenario: Gate on but subprotocol not negotiated → direct impl

- **WHEN** `Dial` is called with both gates on but the peer does not advertise the OTel `Sec-WebSocket-Protocol`
- **THEN** the returned `*Conn` SHALL hold a `*internal/direct.Conn`
- **AND** wire-level messages SHALL be passthrough — no `traceparent` / `tracestate` JSON envelope wrap on send

#### Scenario: Gate on and subprotocol negotiated → traced impl

- **WHEN** both gates are on AND the peer accepts the OTel subprotocol
- **THEN** the returned `*Conn` SHALL hold a `*internal/traced.Conn`
- **AND** `WriteMessage` SHALL wrap the payload in the JSON envelope containing `traceparent`, `tracestate`, and base64-encoded `payload`
- **AND** `ReadMessage` SHALL parse the envelope, extract trace context, and return the original payload bytes

#### Scenario: No `if tracingEnabled` in public methods

- **WHEN** a maintainer reads any public method body of `otelgorillaws.Conn`
- **THEN** the body SHALL NOT contain a runtime `if c.tracingEnabled` branch
- **AND** the body SHALL be a single delegation to `c.impl.<Method>(args...)` (modulo error wrapping or argument adaptation)

### Requirement: Subprotocol negotiation runtime override remains

The existing scenarios A through E for `Sec-WebSocket-Protocol` negotiation SHALL continue to behave identically. Strategy-split impls SHALL be selected in the same control flow that previously set `tracingEnabled bool`.

#### Scenario: Scenario A — both sides advertise OTel subprotocol

- **WHEN** client and server both list the OTel subprotocol and negotiation succeeds
- **THEN** the wrapper SHALL select the traced impl on both sides

#### Scenario: Scenario E — server downgrades to plain subprotocol

- **WHEN** server responds without the OTel subprotocol
- **THEN** the wrapper SHALL select the direct impl on the client side
- **AND** the connection SHALL function as a plain `*websocket.Conn`

### Requirement: `ReadMessage` return signature preserves trace context

`Conn.ReadMessage` SHALL return `(messageType int, p []byte, ctx context.Context, err error)` (or the existing equivalent shape) in **both** direct and traced modes. Caller code SHALL NOT need to branch on impl flavour.

#### Scenario: Direct mode returns background context

- **WHEN** a caller calls `ReadMessage` on a direct-impl connection
- **THEN** the returned `ctx` SHALL be a context without a remote span (e.g. `context.Background()` or the inherited reader context)
- **AND** `p` SHALL be the raw message bytes from the upstream `*websocket.Conn`

#### Scenario: Traced mode returns context carrying remote span

- **WHEN** a caller calls `ReadMessage` on a traced-impl connection and receives a wrapped message
- **THEN** the returned `ctx` SHALL carry the extracted remote trace (parented to the producer span)
- **AND** `p` SHALL be the decoded `payload` bytes (envelope stripped)
