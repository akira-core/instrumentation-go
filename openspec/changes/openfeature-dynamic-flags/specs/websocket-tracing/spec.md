## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The package SHALL gate span creation and trace-context propagation behind a conjunction of tiers:

```
capable  := gate1 && EnvEnabled(OTEL_GORILLA_WS_TRACING_ENABLED)   // static, resolved at construction
feature  := capable && resolver.Allowed(idxTracing)                 // re-read per WriteMessage/ReadMessage
```

`gate1` SHALL be `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` or the `WithTracingEnabled(v bool)` `Option`, which are mutually exclusive per the shared `shared-feature-flags` rule; supplying both SHALL make the constructor return an error.

`capable` SHALL be resolved once, before the WebSocket handshake, and SHALL remain fixed for the connection's lifetime, because it decides two things that cannot be revisited: whether to negotiate the `otel-ws` subprotocol, and whether to build a real tracer at all. A connection whose `capable` is false SHALL delegate directly to the underlying `*websocket.Conn`, SHALL create no spans, SHALL perform no envelope handling, and SHALL perform no OpenFeature evaluation.

The relay flag `otel-gorilla-ws-tracing` SHALL be resolved with an evaluation default of `true` and SHALL only ever subtract. No relay value SHALL make a non-`capable` connection trace. Because `capable` already carries both environment tiers, a connection whose environment tiers were off at construction performs no evaluation for its lifetime.

Environment truthiness SHALL follow the allow-list in `shared-feature-flags`: only `1`, `true`, `yes`, `on` (trimmed, case-insensitive) enable; every other value, including the empty string, disables.

`feature` SHALL be re-read per `WriteMessage`/`ReadMessage` call rather than cached on the `Conn`, so a live connection observes a revocation within the resolver's TTL. `WithTracingEnabled` SHALL NOT make a connection static: it supplies `gate1` only, and a connection carrying it still stops creating spans when the relay revokes.

Whether the connection writes the JSON envelope SHALL remain fixed for its lifetime and SHALL be determined solely by whether `otel-ws` was successfully negotiated (`Dial`/`Upgrade`) or proven for `NewConn` via `isOTelWireProtocol` on the raw connection's negotiated subprotocol. A connection whose peer negotiated `otel-ws` SHALL continue to write envelopes after a revocation; in that state it SHALL inject no trace context and SHALL create no spans. A connection that did not negotiate SHALL use raw passthrough for the wire; while `feature` is on it MAY still create local send/receive spans without inject/extract.

#### Scenario: First tier off
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or falsy and no `WithTracingEnabled` option is passed
- **THEN** the connection delegates directly to the underlying `*websocket.Conn` with no spans and no envelope handling, regardless of `OTEL_GORILLA_WS_TRACING_ENABLED` or any relay value, and no OpenFeature evaluation is performed

#### Scenario: Module switch off makes the connection incapable
- **WHEN** `gate1` is enabled and `OTEL_GORILLA_WS_TRACING_ENABLED` is unset or falsy
- **THEN** the connection is not capable, `otel-ws` is neither offered nor confirmed, the wire stays raw, no spans are created, and no `Client.Boolean` call is made

#### Scenario: Both environment tiers on and the relay does not interfere
- **WHEN** `gate1` is enabled, `OTEL_GORILLA_WS_TRACING_ENABLED` is truthy, and the relay resolves `otel-gorilla-ws-tracing` to `true` or has no such flag
- **THEN** `WriteMessage`/`ReadMessage` create send/receive spans

#### Scenario: No provider installed reproduces the previous release exactly
- **WHEN** no OpenFeature provider is installed and the two tracing environment variables are set to any combination of allow-list values
- **THEN** both span creation and subprotocol negotiation match the release preceding this change, with no wire-format exception

#### Scenario: Relay revokes tracing on a live connection
- **WHEN** an established connection that negotiated `otel-ws` is creating spans and the relay subsequently resolves `otel-gorilla-ws-tracing` to `false`
- **THEN** messages sent after the resolver's TTL expires create no spans and carry an envelope with no trace context, and the peer continues to parse them as envelopes

#### Scenario: Relay cannot enable tracing the deployment left off
- **WHEN** `gate1` is enabled, `OTEL_GORILLA_WS_TRACING_ENABLED` is unset, and the relay resolves `otel-gorilla-ws-tracing` to `true`
- **THEN** the connection creates no spans, writes no envelope, and performs no evaluation

#### Scenario: Option and environment variable together are rejected
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set to any value and `NewConn`, `Dial`, or `Upgrader.Upgrade` is passed `WithTracingEnabled(v)` for either value of `v`
- **THEN** the constructor returns an error matching the module's tracing-conflict sentinel and no `Conn` is produced

#### Scenario: Option-carrying connection still obeys a revocation
- **WHEN** a connection constructed with `WithTracingEnabled(true)` (environment variable unset) and a truthy `OTEL_GORILLA_WS_TRACING_ENABLED` is creating spans, and the relay resolves the module flag to `false`
- **THEN** messages sent after the resolver's TTL expires create no spans, while the envelope continues to be written if `otel-ws` was negotiated

### Requirement: otel-ws negotiation gated on the effective feature flag
`Dial` SHALL NOT offer, and `Upgrader.Upgrade` SHALL NOT confirm, the `otel-ws` subprotocol when the connection's **static capability** resolves to disabled. That capability SHALL be `gate1 && flags.EnvEnabled("OTEL_GORILLA_WS_TRACING_ENABLED")`, resolved **before** the handshake.

The capability SHALL NOT consult the relay verdict. Excluding it is free rather than a compromise: because the relay can only revoke (see `dynamic-feature-flags`), a connection whose environment tiers are off at handshake time can never be switched on later, so there is no future state in which it would need the envelope. This removes the wire-format exception that a relay capable of enabling would have forced, and restores the property that upgrading without a provider changes nothing on the wire.

Letting a capability-off side negotiate `otel-ws` would commit the peer to a wire format whose frames that side hands to the application unparsed (silent payload corruption), so construction SHALL clamp the negotiation outcome such that `tracingEnabled` is false whenever capability is false. The reverse direction is unchanged: neither `WithTracingEnabled(true)` nor any relay value can force the envelope onto a connection whose peer did not negotiate `otel-ws` — the negotiation outcome still requires both sides to agree, and `NewConn` proves agreement only via the raw connection's subprotocol. (Scenario tables including this gate live in `otel-ws.md` §5.)

#### Scenario: Capability-off server does not confirm otel-ws
- **WHEN** a client proposes `otel-ws,json` and the server upgrades with capability off (either tier)
- **THEN** the upgrade succeeds via normal application-protocol selection (`json`), otel-ws is not confirmed, and payloads round-trip between both sides without the envelope

#### Scenario: Capability-off client does not offer otel-ws
- **WHEN** a client dials with capability off and a non-empty subprotocol list against an otel-ws-aware server
- **THEN** the handshake proposes only the application protocols, the server does not confirm otel-ws, and messages round-trip unwrapped

#### Scenario: Module switch off suppresses negotiation
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is truthy, `OTEL_GORILLA_WS_TRACING_ENABLED` is unset or falsy, and two peers both use this library's Dial/Upgrade
- **THEN** `otel-ws` is not negotiated and the wire carries raw payloads, matching the release preceding this change

#### Scenario: Negotiation ignores the relay verdict
- **WHEN** both environment tiers are truthy and the relay resolves `otel-gorilla-ws-tracing` to `false` at handshake time
- **THEN** `Dial` still offers and `Upgrader.Upgrade` still confirms `otel-ws`, so a later restoration of the flag resumes trace propagation on the same connection

#### Scenario: Envelope is carried while tracing is revoked
- **WHEN** two capable peers establish a connection and the relay then revokes `otel-gorilla-ws-tracing`
- **THEN** every message still carries the JSON envelope with no trace context, no spans are created, and the receiving application observes the original payload

### Requirement: NewConn always wraps envelopes
`NewConn(rawConn, opts...)` SHALL enable envelope wrapping **only** when the raw connection's negotiated subprotocol proves `otel-ws` (`isOTelWireProtocol`). It SHALL NOT force envelope wrapping on a connection whose subprotocol does not prove negotiation, because the peer would then receive `{"header":...,"data":...}` frames it never agreed to and hand them to its application unparsed.

Callers that manage the handshake themselves SHALL leave a correct negotiated subprotocol on the raw connection. There SHALL be no option that asserts negotiation without subprotocol evidence. Construction SHALL clamp the outcome with capability, so a raw connection carrying an `otel-ws` subprotocol wrapped by a non-capable process still uses raw passthrough.

#### Scenario: NewConn without otel-ws subprotocol stays raw on the wire
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is not an otel-ws protocol and the connection is capable
- **THEN** `WriteMessage` sends the application payload bytes unchanged, and a non-instrumented peer observes the original payload

#### Scenario: NewConn with otel-ws subprotocol may envelope
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is `otel-ws` or `otel-ws+…` and the connection is capable
- **THEN** `WriteMessage`/`ReadMessage` use the JSON envelope for the connection lifetime, independent of later relay revocations for the envelope decision

#### Scenario: Incapable wrapper of a negotiated connection stays raw
- **WHEN** `NewConn` wraps a connection whose subprotocol is `otel-ws+json` while capability is off
- **THEN** the wrapper delegates directly to the native connection and performs no envelope handling

## ADDED Requirements

### Requirement: NewConn reports configuration errors
`NewConn` SHALL have the signature `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)` so that a configuration conflict detected at construction can be reported, in line with every other option-accepting constructor in the repository (`Dial`, `Upgrader.Upgrade`, and the Mongo and NATS connect variants, all of which already return an error).

#### Scenario: NewConn rejects a conflicting configuration
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set and `NewConn(raw, WithTracingEnabled(true))` is called
- **THEN** `NewConn` returns a nil `*Conn` and an error matching the module's tracing-conflict sentinel

#### Scenario: NewConn returns a nil error on success
- **WHEN** `NewConn(raw)` is called with no conflicting configuration
- **THEN** it returns a usable `*Conn` and a nil error
