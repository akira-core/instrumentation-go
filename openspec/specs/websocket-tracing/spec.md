# websocket-tracing Specification

## Purpose
TBD - created by archiving change document-otel-instrumentation. Update Purpose after archive.
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
The package SHALL gate span creation and trace-context propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global) and `OTEL_GORILLA_WS_TRACING_ENABLED` (module). Both SHALL default to disabled when unset; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value, including an empty string, SHALL enable. The env-derived result SHALL serve as the **default**: when the caller passes the `WithTracingEnabled(v bool)` `Option` to `NewConn`, `Dial`, or an `Upgrader`-based construction path, that value SHALL be authoritative for the resulting `Conn`, overriding both environment gates in either direction per the shared `WithTracingEnabled` decision table in `shared-feature-flags`. Connections constructed without the option SHALL behave exactly as before.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the connection delegates directly to the underlying `*websocket.Conn` with no spans and no envelope handling, regardless of `OTEL_GORILLA_WS_TRACING_ENABLED`

#### Scenario: Both flags enabled
- **WHEN** both tracing env vars are set to a truthy value and no `WithTracingEnabled` option is passed
- **THEN** `WriteMessage`/`ReadMessage` create send/receive spans

#### Scenario: Option enables tracing with env off (unset or falsy)
- **WHEN** `NewConn(raw, WithTracingEnabled(true))` is called with both tracing env vars unset or explicitly falsy
- **THEN** the connection creates send/receive spans and handles the JSON envelope — no environment configuration is required

#### Scenario: Option disables tracing despite truthy env vars
- **WHEN** both env gates are truthy and a connection is constructed with `WithTracingEnabled(false)`
- **THEN** that connection delegates directly to the native `*websocket.Conn` (no spans, no envelope), while other connections without the option still trace per the env gates

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

### Requirement: Consumer span kind on read, no broker deliver span
`ReadMessage` SHALL set `trace.WithSpanKind(trace.SpanKindConsumer)` on the read span. Unlike `otel-mongo` and `otel-nats`, `otel-gorilla-ws` SHALL NOT implement a separate deliver-span pattern or broker-service `TracerProvider`.

#### Scenario: Reading a message
- **WHEN** `ReadMessage` completes successfully with tracing enabled
- **THEN** the resulting span has `SpanKind == Consumer` and no additional broker-node span is created

### Requirement: otel-ws negotiation gated on the effective feature flag
`Dial` SHALL NOT offer, and `Upgrader.Upgrade` SHALL NOT confirm, the `otel-ws` subprotocol when the connection's effective tracing feature (the env gates, overridden by `WithTracingEnabled` when present) resolves to disabled. The flag SHALL be resolved **before** the WebSocket handshake, so the negotiation outcome always reflects the connection's actual envelope capability — a feature-off side neither writes nor unwraps the JSON envelope, so letting it negotiate otel-ws would commit the peer to a wire format whose frames the feature-off side hands to the application unparsed (silent payload corruption). The reverse direction is unchanged: `WithTracingEnabled(true)` cannot force the envelope onto a connection whose peer did not negotiate otel-ws — the negotiation outcome still requires both sides to agree. (Scenario tables including this gate live in `otel-ws.md` §5.)

#### Scenario: Tracing-off server does not confirm otel-ws
- **WHEN** a client proposes `otel-ws,json` and the server upgrades with `WithTracingEnabled(false)` (or with the env gates off)
- **THEN** the upgrade succeeds via normal application-protocol selection (`json`), otel-ws is not confirmed, and payloads round-trip between both sides without the envelope

#### Scenario: Tracing-off client does not offer otel-ws
- **WHEN** a client dials with `WithTracingEnabled(false)` (or with the env gates off) and a non-empty subprotocol list against an otel-ws-aware server
- **THEN** the handshake proposes only the application protocols, the server does not confirm otel-ws, and messages round-trip unwrapped
