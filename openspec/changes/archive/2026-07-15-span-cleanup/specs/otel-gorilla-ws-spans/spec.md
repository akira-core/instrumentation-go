## ADDED Requirements

### Requirement: WebSocket span kinds

`otel-gorilla-ws` SHALL keep messaging-style span kinds because a WebSocket frame is fire-and-forget (the writer does not await a response): `WriteMessage` → `PRODUCER`, `ReadMessage` → `CONSUMER`. It SHALL NOT use `CLIENT`/`SERVER` (which per the OTel SpanKind definition require the client to await a response) and SHALL NOT construct deliver spans or a broker `TracerProvider` (it never did).

#### Scenario: Write and read span kinds

- **WHEN** a caller invokes `WriteMessage`
- **THEN** the emitted span SHALL have `SpanKind == PRODUCER`

- **WHEN** a caller invokes `ReadMessage`
- **THEN** the emitted span SHALL have `SpanKind == CONSUMER`

### Requirement: WebSocket span attribute set

Because WebSocket is not a covered OTel messaging system, span attributes SHALL NOT use the `messaging.*` namespace. The genuinely-useful custom key `websocket.message.type` SHALL be retained. Message body size SHALL be carried under a WebSocket-scoped key (`websocket.message.body.size`), not `messaging.message.body.size`.

#### Scenario: Write span attributes use websocket namespace

- **WHEN** a caller writes a binary frame of 512 bytes
- **THEN** the span SHALL carry `websocket.message.type=<code>` and `websocket.message.body.size=512`
- **AND** the span SHALL NOT carry any `messaging.*` attribute

### Requirement: Disabled tracing emits no spans or SDK objects

When the connection's tracing gate is off (`tracingEnabled == false` — the otel-ws subprotocol was not negotiated, or global tracing env is disabled), `WriteMessage` / `ReadMessage` SHALL pass through to the native `*websocket.Conn` and run no OTel SDK code path — no real-tracer `Start`, no wire-envelope wrap/unwrap, no propagator inject/extract — consistent with the module-wide disabled-mode invariant. (`otel-gorilla-ws` never constructed a deliver `TracerProvider`, so none is removed here.)

#### Scenario: Tracing disabled passes through to native conn

- **WHEN** a connection has `tracingEnabled == false` and a caller invokes `WriteMessage` or `ReadMessage`
- **THEN** the call SHALL delegate to the native `*websocket.Conn` with the original payload unchanged (no JSON envelope written or parsed)
- **AND** no span SHALL be emitted
- **AND** the trace propagator SHALL NOT be invoked to inject or extract
