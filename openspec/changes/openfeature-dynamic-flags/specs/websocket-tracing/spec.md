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

`feature` SHALL be re-read per `WriteMessage`/`ReadMessage` call rather than cached on the `Conn`, so a live connection observes a revocation on its next operation. `WithTracingEnabled` SHALL NOT make a connection static: it supplies `gate1` only, and a connection carrying it still stops creating spans when the relay revokes.

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
- **THEN** messages sent after the revocation create no spans and carry an envelope with no trace context, and the peer continues to parse them as envelopes

#### Scenario: Relay cannot enable tracing the deployment left off
- **WHEN** `gate1` is enabled, `OTEL_GORILLA_WS_TRACING_ENABLED` is unset, and the relay resolves `otel-gorilla-ws-tracing` to `true`
- **THEN** the connection creates no spans, writes no envelope, and performs no evaluation

#### Scenario: Option and environment variable together are rejected
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set to any value and `NewConn`, `Dial`, or `Upgrader.Upgrade` is passed `WithTracingEnabled(v)` for either value of `v`
- **THEN** the constructor returns an error matching the module's tracing-conflict sentinel and no `Conn` is produced

#### Scenario: Option-carrying connection still obeys a revocation
- **WHEN** a connection constructed with `WithTracingEnabled(true)` (environment variable unset) and a truthy `OTEL_GORILLA_WS_TRACING_ENABLED` is creating spans, and the relay resolves the module flag to `false`
- **THEN** messages sent after the revocation create no spans, while the envelope continues to be written if `otel-ws` was negotiated

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

#### Scenario: Revocation does not remove the envelope's cost
- **WHEN** `otel-gorilla-ws-tracing` is revoked on a negotiated connection
- **THEN** each write still marshals the envelope and each read still runs the probe, so this module alone does not return to the zero-cost path on revocation, and removing that wire overhead requires a redeploy — which the operational documentation SHALL state next to the instruction for revoking a module

### Requirement: NewConn always wraps envelopes
`NewConn(rawConn, opts...)` SHALL enable envelope wrapping **only** when the raw connection's negotiated subprotocol proves `otel-ws` (`isOTelWireProtocol`). It SHALL NOT force envelope wrapping on a connection whose subprotocol does not prove negotiation, because the peer would then receive `{"header":...,"data":...}` frames it never agreed to and hand them to its application unparsed.

Callers that manage the handshake themselves SHALL leave a correct negotiated subprotocol on the raw connection. There SHALL be no option that asserts negotiation without subprotocol evidence.

**Capability SHALL clamp the write decision only.** Whether the peer envelopes is a fact established by the handshake; this side's feature gate is a local policy, and applying the policy to the fact corrupts the read path. A non-capable process wrapping an `otel-ws` connection SHALL therefore write raw frames — safe, because a peer receiving a raw frame falls back to treating it as the payload — while `ReadMessage` SHALL still unwrap, because the peer envelopes every frame regardless of this side's gate. `Conn` SHALL record the negotiated wire fact in a field that capability does not clamp, and the read path SHALL key on that field rather than on capability.

Unwrapping under a disabled gate is `json.Unmarshal` with the extracted headers discarded — no span, no attribute construction, no propagator call — so the disabled-mode invariant is unaffected.

#### Scenario: NewConn without otel-ws subprotocol stays raw on the wire
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is not an otel-ws protocol and the connection is capable
- **THEN** `WriteMessage` sends the application payload bytes unchanged, and a non-instrumented peer observes the original payload

#### Scenario: NewConn with otel-ws subprotocol may envelope
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is `otel-ws` or `otel-ws+…` and the connection is capable
- **THEN** `WriteMessage`/`ReadMessage` use the JSON envelope for the connection lifetime, independent of later relay revocations for the envelope decision

#### Scenario: Incapable wrapper of a negotiated connection writes raw
- **WHEN** `NewConn` wraps a connection whose subprotocol is `otel-ws+json` while capability is off, and `WriteMessage` is called
- **THEN** the application payload is sent unchanged, and the peer's probe falls back to it

#### Scenario: Incapable wrapper of a negotiated connection still unwraps on read
- **WHEN** the same wrapper receives a frame the peer enveloped
- **THEN** `ReadMessage` returns the unwrapped payload, not the `{"header":…,"data":…}` bytes, and creates no span and performs no extraction

## ADDED Requirements

### Requirement: The otel-ws subprotocol token and negotiation test are exported
Because `NewConn` requires callers running their own handshake to leave a correct negotiated subprotocol and offers no escape hatch, the package SHALL export what is needed to satisfy that requirement:

- `const SubprotocolOTelWS = "otel-ws"` — the token to offer (client) or echo (server).
- `func IsOTelNegotiated(conn *websocket.Conn) bool` — reports whether `NewConn` will enable envelope handling on that connection.

Neither SHALL be able to force an envelope onto a peer that did not negotiate one, so the prohibition on a negotiation-asserting option is preserved. The documentation SHALL state that a stock `websocket.Dialer` or `Upgrader` can reach only the bare `otel-ws` form, since gorilla echoes exact matches, and that the `otel-ws+<app>` composite remains exclusive to this package's `Upgrader.Upgrade`.

#### Scenario: Hand-rolled client handshake opts in
- **WHEN** an application sets `Subprotocols: []string{otelgorillaws.SubprotocolOTelWS, "json"}` on its own `websocket.Dialer`, dials, and wraps the result with `NewConn`
- **THEN** envelope handling is enabled if the server echoed the token

#### Scenario: Caller can verify rather than assume
- **WHEN** an application calls `IsOTelNegotiated(raw)` before `NewConn`
- **THEN** the result equals whether `NewConn` will enable envelope handling on that connection

### Requirement: The envelope probe is byte-transparent for non-envelope payloads
`tryUnmarshalWire` SHALL return the caller's original bytes unchanged whenever the frame is not one of the two recognised trace-carrying formats.

Its legacy flat-format branch SHALL return `ok=false` when **neither** `traceparent` nor `tracestate` is present at the top level. A message carrying neither is by definition not a legacy envelope, and the branch's re-marshal of a `map[string]json.RawMessage` sorts keys and normalises whitespace, so returning it would hand the application a byte-different frame — breaking any caller that hashes or signature-verifies the payload. This path is reachable whenever a capability-off peer writes raw frames onto a negotiated connection.

The envelope structure `{"header":…,"data":…}` SHALL be documented in `otel-ws.md` as **reserved** on an `otel-ws` connection: an application payload of that shape is unwrapped and its outer structure discarded. Tightening the match to require `header` to contain only the two trace keys SHALL NOT be done, because a future header member added by the JS packages would then fall into the legacy branch instead.

#### Scenario: JSON object without trace keys survives byte-identically
- **WHEN** a frame containing a JSON object with neither `traceparent` nor `tracestate` is read on a negotiated connection
- **THEN** the application receives the original bytes, key order and whitespace included

#### Scenario: Legacy flat format is still recognised
- **WHEN** a frame contains a top-level `traceparent`
- **THEN** the trace context is extracted and the remaining fields are returned as the payload

### Requirement: NewConn reports configuration errors
`NewConn` SHALL have the signature `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)` so that a configuration conflict detected at construction can be reported, in line with every other option-accepting constructor in the repository (`Dial`, `Upgrader.Upgrade`, and the Mongo and NATS connect variants, all of which already return an error).

#### Scenario: NewConn rejects a conflicting configuration
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is set and `NewConn(raw, WithTracingEnabled(true))` is called
- **THEN** `NewConn` returns a nil `*Conn` and an error matching the module's tracing-conflict sentinel

#### Scenario: NewConn returns a nil error on success
- **WHEN** `NewConn(raw)` is called with no conflicting configuration
- **THEN** it returns a usable `*Conn` and a nil error
