# websocket-tracing Delta Specification

## ADDED Requirements

### Requirement: otel-ws negotiation gated on the effective feature flag
`Dial` SHALL NOT offer, and `Upgrader.Upgrade` SHALL NOT confirm, the `otel-ws` subprotocol when the connection's effective tracing feature (the env gates, overridden by `WithTracingEnabled` when present) resolves to disabled. The flag SHALL be resolved **before** the WebSocket handshake, so the negotiation outcome always reflects the connection's actual envelope capability — a feature-off side neither writes nor unwraps the JSON envelope, so letting it negotiate otel-ws would commit the peer to a wire format whose frames the feature-off side hands to the application unparsed (silent payload corruption). The reverse direction is unchanged: `WithTracingEnabled(true)` cannot force the envelope onto a connection whose peer did not negotiate otel-ws — the negotiation outcome still requires both sides to agree. (Scenario tables including this gate live in `otel-ws.md` §5.)

#### Scenario: Tracing-off server does not confirm otel-ws
- **WHEN** a client proposes `otel-ws,json` and the server upgrades with `WithTracingEnabled(false)` (or with the env gates off)
- **THEN** the upgrade succeeds via normal application-protocol selection (`json`), otel-ws is not confirmed, and payloads round-trip between both sides without the envelope

#### Scenario: Tracing-off client does not offer otel-ws
- **WHEN** a client dials with `WithTracingEnabled(false)` (or with the env gates off) and a non-empty subprotocol list against an otel-ws-aware server
- **THEN** the handshake proposes only the application protocols, the server does not confirm otel-ws, and messages round-trip unwrapped

## MODIFIED Requirements

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
