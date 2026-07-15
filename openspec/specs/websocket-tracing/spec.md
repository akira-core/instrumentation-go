# websocket-tracing Specification

## Purpose
Defines the tracing behavior of `otel-gorilla-ws`: provider/propagator fallback, feature-flag gating, subprotocol negotiation and envelope wire format, span kinds/attributes, and the disabled-mode invariant.

## Requirements
### Requirement: Provider and propagator fallback
`otel-gorilla-ws` SHALL NOT construct or own a global `TracerProvider`. The propagator always falls back to `otel.GetTextMapPropagator()` unless `WithPropagators(p)` is supplied. The tracer falls back to `otel.GetTracerProvider()` unless `WithTracerProvider(tp)` is supplied, **but only when the two-tier tracing feature flags (see below) are enabled** — when the flags are disabled (the default, unset state), the connection uses a `noop.NewTracerProvider()`-derived tracer instead, regardless of `WithTracerProvider` or the process-global provider, so that no OTel SDK call ever reaches the caller's real `TracerProvider` while tracing is off.

#### Scenario: Wrapping an already-dialed connection, tracing enabled
- **WHEN** an application dials a raw `*websocket.Conn` itself, wraps it with `NewConn(raw)` without options, and both tracing feature flags are enabled
- **THEN** the wrapped connection uses the process-global `TracerProvider` and `TextMapPropagator`

#### Scenario: Wrapping a connection with tracing flags disabled (default)
- **WHEN** `NewConn(raw)` is called while `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` or `OTEL_GORILLA_WS_TRACING_ENABLED` is unset or falsy
- **THEN** the connection's tracer is a `noop` tracer, not the process-global `TracerProvider` — no spans reach the caller's configured provider even if one is set

### Requirement: Two-tier tracing feature-flag gating
The package SHALL gate span creation and trace-context propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global) and `OTEL_GORILLA_WS_TRACING_ENABLED` (module). Both SHALL default to disabled when unset; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value, including an empty string, SHALL enable.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy
- **THEN** the connection delegates directly to the underlying `*websocket.Conn` with no spans and no envelope handling, regardless of `OTEL_GORILLA_WS_TRACING_ENABLED`

#### Scenario: Both flags enabled
- **WHEN** both tracing env vars are set to a truthy value
- **THEN** `WriteMessage`/`ReadMessage` create send/receive spans

### Requirement: JSON envelope wire format
When tracing is enabled and envelope wrapping applies (see `NewConn` vs. `Dial`/`Upgrade` requirement), outgoing messages SHALL be wrapped as `{"header": {"traceparent": ..., "tracestate": ...}, "data": <payload>}`, where `data` is the original payload verbatim if it is valid JSON, or a JSON-encoded string if it is not.

#### Scenario: JSON payload
- **WHEN** `WriteMessage` sends a payload that is valid JSON
- **THEN** the wire message is the envelope with `data` set to that JSON value unmodified

#### Scenario: Non-JSON payload
- **WHEN** `WriteMessage` sends raw bytes that are not valid JSON
- **THEN** the wire message is the envelope with `data` set to a JSON-encoded string of those bytes

### Requirement: Incoming message format support
`ReadMessage` SHALL accept both the envelope format (`{"header": {...}, "data": ...}`) and a legacy flat format (`{"traceparent": ..., "tracestate": ..., ...fields}`) for backward compatibility with old Go-only deployments, extracting trace context from whichever format is present.

#### Scenario: Envelope format received
- **WHEN** a peer sends the envelope format
- **THEN** `ReadMessage` extracts `traceparent`/`tracestate` from `header` and returns `data` as the payload

#### Scenario: Legacy flat format received
- **WHEN** a peer sends the legacy flat format with top-level `traceparent`/`tracestate` fields
- **THEN** `ReadMessage` extracts the trace context from those top-level fields as a read-only fallback

### Requirement: NewConn always wraps envelopes
`NewConn(rawConn, opts...)` SHALL always enable envelope wrapping when the feature flags are on, regardless of any subprotocol negotiated on the underlying connection, for backward compatibility with callers that manage their own handshake.

#### Scenario: NewConn with an arbitrary subprotocol
- **WHEN** a raw connection negotiated via any subprotocol (or none) is wrapped with `NewConn` and tracing flags are enabled
- **THEN** outgoing messages are still wrapped in the envelope format

### Requirement: Dial/Upgrader spec-compliant negotiation
`Dial(ctx, urlStr, requestHeader, subprotocols, opts...)`, **when `subprotocols` is non-empty**, SHALL inject the `otel-ws` subprotocol ahead of the caller-supplied subprotocols during the handshake, and SHALL enable envelope wrapping only if the server responds with an `otel-ws` or `otel-ws+<proto>` subprotocol. When `subprotocols` is empty, no `otel-ws` token is injected (see *Passthrough fallback* below). `Upgrader.Upgrade(w, r, responseHeader)` SHALL detect an `otel-ws`-prefixed subprotocol among the client's proposed subprotocols and, on that acceptance path, respond with `otel-ws`/`otel-ws+<proto>` and enable envelope wrapping.

#### Scenario: Both sides support otel-ws
- **WHEN** a client calls `Dial` with subprotocols `["json"]` against a server using `Upgrader`
- **THEN** the client sends `otel-ws,json`, the server responds `otel-ws+json`, both sides enable envelope wrapping, and the application-visible `Subprotocol()` reports `json` (the `otel-ws+` prefix is stripped)

#### Scenario: Server does not support otel-ws
- **WHEN** a client calls `Dial` against a plain `gorilla/websocket` server that only supports `json`
- **THEN** the server responds with `json` (not `otel-ws+json`), and the client falls back to passthrough mode: send/receive spans are still created (if tracing flags are on) but no envelope is written or read on the wire

### Requirement: Passthrough fallback preserves send/receive spans
When `Dial` or `Upgrade` negotiation does not result in `otel-ws` acceptance, the connection SHALL silently fall back to passthrough mode rather than failing the handshake: send/receive spans continue to be created as long as the feature flags are enabled, but no envelope is written or read on the wire.

#### Scenario: Peer offers no subprotocol
- **WHEN** a client dials with an empty subprotocol list against a server, or a server receives a client that proposed no subprotocol
- **THEN** the connection is established, tracing spans are still created if flags are enabled, and no envelope is applied

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

