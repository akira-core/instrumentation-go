## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The package SHALL gate span creation and trace-context propagation behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global, environment-only) and the dynamic flag `otel-gorilla-ws-tracing` (module), the latter resolving through the module's `flags.Resolver` with `OTEL_GORILLA_WS_TRACING_ENABLED` as its OpenFeature default value. An unset environment variable SHALL be treated as disabled; values `0`/`false`/`no`/`off` (case-insensitive) SHALL disable; any other set value, including an empty string, SHALL enable. With no OpenFeature provider installed, **span on/off** behavior SHALL match the environment-only resolution of the preceding release, **except** that otel-ws **negotiation** follows the global switch alone (see negotiation requirement and design D9/R4) and may therefore differ from the preceding release when global is on and the module env is off.

The global switch SHALL be a hard kill switch: when it is disabled and no `WithTracingEnabled` option is present, no OpenFeature evaluation SHALL occur and no relay value SHALL enable tracing.

The dynamic value SHALL be read per `WriteMessage`/`ReadMessage` call rather than cached on the `Conn`, so a live connection observes a relay change within the resolver's TTL. When the caller passes the `WithTracingEnabled(v bool)` `Option` to `NewConn`, `Dial`, or an `Upgrader`-based construction path, that value SHALL be authoritative for the connection's **feature / span gate** (and SHALL suppress OpenFeature evaluation for that connection), per the shared `WithTracingEnabled` decision table in `shared-feature-flags`. The option SHALL NOT force the JSON envelope onto a peer that did not negotiate `otel-ws`.

Whether the connection writes the JSON envelope SHALL remain fixed for its lifetime and SHALL be determined solely by whether otel-ws was successfully negotiated (Dial/Upgrade) or proven for `NewConn` via `isOTelWireProtocol` on the raw connection's negotiated subprotocol. A connection whose peer negotiated `otel-ws` SHALL continue to write envelopes even while the dynamic value resolves to disabled; in that state it SHALL inject no trace context and SHALL create no spans. A connection that did not negotiate (or cannot prove negotiation) SHALL use raw passthrough for the wire; when capability and the dynamic feature gate are on it MAY still create local send/receive spans without inject/extract.

#### Scenario: Global flag off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the connection delegates directly to the underlying `*websocket.Conn` with no spans and no envelope handling, regardless of `OTEL_GORILLA_WS_TRACING_ENABLED` or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Both tiers on
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is truthy, the dynamic `otel-gorilla-ws-tracing` value resolves to enabled, and no `WithTracingEnabled` option is passed
- **THEN** `WriteMessage`/`ReadMessage` create send/receive spans

#### Scenario: No provider installed reproduces environment-only span behavior
- **WHEN** no OpenFeature provider is installed and `OTEL_GORILLA_WS_TRACING_ENABLED` is set to any value
- **THEN** span creation on/off matches the environment-only resolution of the preceding release (module env as the sole module-tier input), subject to the negotiation exception below

#### Scenario: No provider still changes negotiation when only global is on
- **WHEN** no OpenFeature provider is installed, `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is truthy, `OTEL_GORILLA_WS_TRACING_ENABLED` is falsy, and two peers both use this library's Dial/Upgrade
- **THEN** otel-ws MAY be negotiated and messages MAY carry the JSON envelope with empty headers and no spans — this is the documented D9/R4 exception to "no provider ⇒ identical wire to previous release"

#### Scenario: Relay disables tracing on a live connection
- **WHEN** an established connection that negotiated `otel-ws` is creating spans and the relay subsequently resolves `otel-gorilla-ws-tracing` to `false`
- **THEN** messages sent after the resolver's TTL expires create no spans and carry an envelope with no trace context, and the peer continues to parse them as envelopes

#### Scenario: Relay enables tracing on a live connection
- **WHEN** an established connection that negotiated `otel-ws` was not creating spans because the dynamic value was disabled, and the relay subsequently resolves it to `true`
- **THEN** messages sent after the resolver's TTL expires create spans and carry injected trace context, without the connection being re-established

#### Scenario: NewConn without otel-ws subprotocol stays raw on the wire
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is not an otel-ws protocol, the global switch is on, and the dynamic module flag resolves to disabled
- **THEN** `WriteMessage` sends the application payload bytes unchanged (no JSON envelope), and a non-instrumented peer observes the original payload

#### Scenario: NewConn with otel-ws subprotocol may envelope
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is `otel-ws` or `otel-ws+…` and the connection is capable
- **THEN** `WriteMessage`/`ReadMessage` use the JSON envelope for the connection lifetime, independent of later dynamic flag flips for the envelope decision

#### Scenario: Option enables spans with env off but does not force envelope without negotiation
- **WHEN** `NewConn(raw, WithTracingEnabled(true))` is called with both tracing env vars unset or explicitly falsy and the raw connection's subprotocol is not otel-ws
- **THEN** the connection MAY create send/receive spans, SHALL NOT write the JSON envelope, and no OpenFeature evaluation is performed for that connection

#### Scenario: Option enables spans and envelope when subprotocol proves otel-ws
- **WHEN** `NewConn(raw, WithTracingEnabled(true))` is called with env vars off and the raw connection's subprotocol is an otel-ws protocol
- **THEN** the connection creates send/receive spans and handles the JSON envelope, and no OpenFeature evaluation is performed for that connection

#### Scenario: Option disables tracing despite truthy env vars and a truthy relay flag
- **WHEN** both env gates are truthy, the relay resolves `otel-gorilla-ws-tracing` to `true`, and a connection is constructed with `WithTracingEnabled(false)`
- **THEN** that connection delegates directly to the native `*websocket.Conn` (no spans, no envelope) for its lifetime regardless of any subsequent relay change, while other connections without the option follow the dynamic value

### Requirement: otel-ws negotiation gated on the negotiation capability
`Dial` SHALL NOT offer, and `Upgrader.Upgrade` SHALL NOT confirm, the `otel-ws` subprotocol when the connection's **negotiation capability** resolves to disabled. That capability SHALL be the `WithTracingEnabled` option's value when the option is present, and `flags.GlobalTracingPossible()` / `flags.EnvEnabled("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED")` otherwise. It SHALL NOT consult the dynamic `otel-gorilla-ws-tracing` value, because negotiation cannot be revisited after the handshake and a connection that did not negotiate `otel-ws` could never begin propagating trace context when the relay later enables the flag.

The capability SHALL be resolved **before** the WebSocket handshake, so the negotiation outcome always reflects the connection's actual envelope capability — a capability-off side neither writes nor unwraps the JSON envelope, so letting it negotiate otel-ws would commit the peer to a wire format whose frames the capability-off side hands to the application unparsed (silent payload corruption). Construction SHALL clamp the negotiation outcome so `tracingEnabled` is false whenever capability is false. The reverse direction is unchanged: neither `WithTracingEnabled(true)` nor a truthy relay value can force the envelope onto a connection whose peer did not negotiate otel-ws — the negotiation outcome still requires both sides to agree, and `NewConn` proves agreement only via the raw connection's subprotocol. (Scenario tables including this gate live in `otel-ws.md` §5.)

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
