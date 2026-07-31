## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The package SHALL gate span creation and trace-context propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global, environment-only) and the dynamic flag `otel-gorilla-ws-tracing` (module), the latter resolving through the module's `flags.Resolver` with `OTEL_GORILLA_WS_TRACING_ENABLED` as its OpenFeature default value. An unset environment variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value, including an empty string, SHALL enable. With no OpenFeature provider installed, the resolved behavior SHALL be identical to the release preceding this change.

The global switch SHALL be a hard kill switch: when it is disabled and no `WithTracingEnabled` option is present, no OpenFeature evaluation SHALL occur and no relay value SHALL enable tracing.

The dynamic value SHALL be read per `WriteMessage`/`ReadMessage` call rather than cached on the `Conn`, so a live connection observes a relay change within the resolver's TTL. When the caller passes the `WithTracingEnabled(v bool)` `Option` to `NewConn`, `Dial`, or an `Upgrader`-based construction path, that value SHALL be authoritative for the resulting `Conn`, SHALL be resolved once at construction, and SHALL suppress OpenFeature evaluation for that connection entirely, per the shared `WithTracingEnabled` decision table in `shared-feature-flags`.

Whether the connection writes the JSON envelope SHALL remain fixed for its lifetime, because it is determined by subprotocol negotiation during the handshake and cannot be revisited. A connection whose peer negotiated `otel-ws` SHALL continue to write envelopes even while the dynamic value resolves to disabled; in that state it SHALL inject no trace context and SHALL create no spans.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the connection delegates directly to the underlying `*websocket.Conn` with no spans and no envelope handling, regardless of `OTEL_GORILLA_WS_TRACING_ENABLED` or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Both tiers on
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is truthy, the dynamic `otel-gorilla-ws-tracing` value resolves to enabled, and no `WithTracingEnabled` option is passed
- **THEN** `WriteMessage`/`ReadMessage` create send/receive spans

#### Scenario: No provider installed reproduces environment-only behavior
- **WHEN** no OpenFeature provider is installed and `OTEL_GORILLA_WS_TRACING_ENABLED` is set to any value
- **THEN** span creation behavior is identical to the release preceding this change

#### Scenario: Relay disables tracing on a live connection
- **WHEN** an established connection that negotiated `otel-ws` is creating spans and the relay subsequently resolves `otel-gorilla-ws-tracing` to `false`
- **THEN** messages sent after the resolver's TTL expires create no spans and carry an envelope with no trace context, and the peer continues to parse them as envelopes

#### Scenario: Relay enables tracing on a live connection
- **WHEN** an established connection that negotiated `otel-ws` was not creating spans because the dynamic value was disabled, and the relay subsequently resolves it to `true`
- **THEN** messages sent after the resolver's TTL expires create spans and carry injected trace context, without the connection being re-established

#### Scenario: Option enables tracing with env off (unset or falsy)
- **WHEN** `NewConn(raw, WithTracingEnabled(true))` is called with both tracing env vars unset or explicitly falsy
- **THEN** the connection creates send/receive spans and handles the JSON envelope, and no OpenFeature evaluation is performed for that connection

#### Scenario: Option disables tracing despite truthy env vars and a truthy relay flag
- **WHEN** both env gates are truthy, the relay resolves `otel-gorilla-ws-tracing` to `true`, and a connection is constructed with `WithTracingEnabled(false)`
- **THEN** that connection delegates directly to the native `*websocket.Conn` (no spans, no envelope) for its lifetime regardless of any subsequent relay change, while other connections without the option follow the dynamic value

### Requirement: otel-ws negotiation gated on the effective feature flag
`Dial` SHALL NOT offer, and `Upgrader.Upgrade` SHALL NOT confirm, the `otel-ws` subprotocol when the connection's **negotiation capability** resolves to disabled. That capability SHALL be the `WithTracingEnabled` option's value when the option is present, and `flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")` otherwise. It SHALL NOT consult the dynamic `otel-gorilla-ws-tracing` value, because negotiation cannot be revisited after the handshake and a connection that did not negotiate `otel-ws` could never begin propagating trace context when the relay later enables the flag.

The capability SHALL be resolved **before** the WebSocket handshake, so the negotiation outcome always reflects the connection's actual envelope capability — a capability-off side neither writes nor unwraps the JSON envelope, so letting it negotiate otel-ws would commit the peer to a wire format whose frames the capability-off side hands to the application unparsed (silent payload corruption). The reverse direction is unchanged: neither `WithTracingEnabled(true)` nor a truthy relay value can force the envelope onto a connection whose peer did not negotiate otel-ws — the negotiation outcome still requires both sides to agree. (Scenario tables including this gate live in `otel-ws.md` §5.)

#### Scenario: Capability-off server does not confirm otel-ws
- **WHEN** a client proposes `otel-ws,json` and the server upgrades with `WithTracingEnabled(false)` (or with the global env switch off)
- **THEN** the upgrade succeeds via normal application-protocol selection (`json`), otel-ws is not confirmed, and payloads round-trip between both sides without the envelope

#### Scenario: Capability-off client does not offer otel-ws
- **WHEN** a client dials with `WithTracingEnabled(false)` (or with the global env switch off) and a non-empty subprotocol list against an otel-ws-aware server
- **THEN** the handshake proposes only the application protocols, the server does not confirm otel-ws, and messages round-trip unwrapped

#### Scenario: Negotiation ignores the dynamic flag
- **WHEN** the global env switch is truthy, the relay resolves `otel-gorilla-ws-tracing` to `false`, and no `WithTracingEnabled` option is passed
- **THEN** `Dial` still offers and `Upgrader.Upgrade` still confirms `otel-ws`, so the connection retains the capability to propagate trace context if the relay later enables the flag

#### Scenario: Envelope is carried while tracing is dynamically off
- **WHEN** two peers both running this library with the global env switch on establish a connection while the dynamic flag resolves to `false`
- **THEN** `otel-ws` is negotiated, every message carries the JSON envelope with no trace context, no spans are created, and the receiving application observes the original payload
