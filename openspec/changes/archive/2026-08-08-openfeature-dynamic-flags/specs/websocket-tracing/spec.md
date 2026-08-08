## MODIFIED Requirements

### Requirement: Two-tier tracing feature-flag gating
The package SHALL gate span creation and trace-context propagation behind two switches, composed by conjunction:

```
feature := master && wsTracing        // re-read per WriteMessage/ReadMessage
```

Each switch SHALL be resolved down the precedence ladder defined in `shared-feature-flags` — `relay > env > option > default` — implemented as a single `Boolean` call whose evaluation default is the env-or-option-or-default value fixed at construction:

| Switch | Relay key | Option | Environment variable | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| tracing | `otel-gorilla-ws-tracing` | `WithTracingEnabled` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `false` |

A relay value SHALL override the local value in **either** direction. Supplying `WithTracingEnabled` and `OTEL_GORILLA_WS_TRACING_ENABLED` together SHALL NOT be an error; the **environment variable** wins, and the option decides only when the variable is unset. `WithTracingEnabled` SHALL supply the module tier only, never the master.

An environment value outside the recognised truthy and falsy lists, including the empty string, SHALL make `NewConn`, `Dial` or `Upgrader.Upgrade` return an error wrapping `otelflags.ErrInvalidFlagValue`, per `shared-feature-flags`.

`feature` SHALL be re-read per `WriteMessage`/`ReadMessage` call rather than cached on the `Conn`, so a live connection observes a relay change on its next operation. `WithTracingEnabled` SHALL NOT make a connection static.

Whether the connection uses the JSON envelope SHALL be a separate, fixed fact: it is decided by the handshake and SHALL NOT change for the connection's lifetime. `feature` governs spans and inject/extract; the negotiated wire fact governs the envelope. The two SHALL be recorded in distinct fields and SHALL NOT be conflated.

A connection that did not negotiate `otel-ws` SHALL use raw passthrough for the wire; while `feature` is on it MAY still create local send/receive spans without inject/extract. A connection whose peer negotiated `otel-ws` SHALL continue to write envelopes after `feature` goes off; in that state it SHALL inject no trace context and SHALL create no spans.

Whether any OTel SDK path can run at all SHALL be decided at construction by `relayPossible || (masterLocal && wsTracingLocal)`, matching the other three modules. When it is false no OpenFeature client SHALL be created and no evaluation SHALL be performed for that connection's lifetime.

#### Scenario: Master off disables everything
- **WHEN** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is falsy, or the relay resolves `otel-instrumentation-go-tracing` to `false`
- **THEN** the connection creates no spans and performs no inject/extract, regardless of `OTEL_GORILLA_WS_TRACING_ENABLED`, `WithTracingEnabled` or `otel-gorilla-ws-tracing`

#### Scenario: Nothing configured traces nothing
- **WHEN** no environment variable is set, no option is passed, and no relay flag exists
- **THEN** the master resolves to `true`, the tracing switch resolves to its default of `false`, `otel-ws` is neither offered nor confirmed, the wire stays raw, no spans are created, and no evaluation is performed

#### Scenario: Environment variable alone enables tracing and negotiation
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is truthy, no option is passed and no relay flag exists
- **THEN** `Dial` offers and `Upgrader.Upgrade` confirms `otel-ws`, and `WriteMessage`/`ReadMessage` create send/receive spans, because the master defaults to `true`

#### Scenario: No relay configured reproduces the previous release exactly
- **WHEN** no OpenFeature provider is installed, no endpoint variable is set, and the two tracing switches are configured through environment variables and options only
- **THEN** both span creation and subprotocol negotiation match the release preceding this change, with no wire-format exception

#### Scenario: Relay disables tracing on a live connection
- **WHEN** an established connection that negotiated `otel-ws` is creating spans and the relay subsequently resolves `otel-gorilla-ws-tracing` to `false`
- **THEN** messages sent after the change create no spans and carry an envelope with no trace context, and the peer continues to parse them as envelopes

#### Scenario: Relay enables spans on a connection that did not negotiate
- **WHEN** a connection was opened while the module was off, so `otel-ws` was not negotiated, and the relay later resolves `otel-gorilla-ws-tracing` to `true`
- **THEN** the connection MAY create local send/receive spans, but the wire stays raw and no trace context is injected or extracted, because negotiation cannot be revisited

#### Scenario: Option and environment variable together are legal
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is set to any recognised value and `NewConn`, `Dial`, or `Upgrader.Upgrade` is passed `WithTracingEnabled(v)`
- **THEN** construction succeeds and the tracing switch takes the variable's value, whatever the option said

#### Scenario: Option-carrying connection still observes a relay disable
- **WHEN** a connection constructed with `WithTracingEnabled(true)` is creating spans and the relay resolves the module flag to `false`
- **THEN** messages sent afterwards create no spans, while the envelope continues to be written if `otel-ws` was negotiated

#### Scenario: Invalid environment value fails construction
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is set to `enabled`, `2` or the empty string
- **THEN** the constructor returns an error wrapping `otelflags.ErrInvalidFlagValue` naming the variable and the value, and no `Conn` is produced

### Requirement: otel-ws negotiation gated on the effective feature flag
`Dial` SHALL NOT offer, and `Upgrader.Upgrade` SHALL NOT confirm, the `otel-ws` subprotocol when the connection's effective tracing value resolves to disabled. That value SHALL be `master && wsTracing`, with **both** switches resolved down the full precedence ladder — relay included — **once, immediately before** the handshake.

Including the relay is required because it can enable: excluding it would leave a connection unable to carry trace context in a process the operator has just switched on. Resolving it once, before the handshake, is required because negotiation cannot be revisited.

The consequence SHALL be documented in both directions, because operators will otherwise expect symmetry that does not exist:

- **Enabling reaches only connections opened afterwards.** A long-lived connection opened while the module was off never gains the envelope, and `WithTracingEnabled(true)` cannot restore it, since a peer that did not negotiate `otel-ws` will not parse one. An operator who needs an existing connection instrumented SHALL cycle it.
- **Disabling reaches every connection immediately for spans and inject/extract, but not for the envelope.** A disabled connection that negotiated `otel-ws` keeps marshalling the envelope on every write and running the probe on every read, so this module alone does not return to the zero-cost path. Removing that wire overhead requires cycling the connection.

The alternative — negotiating whenever a relay is merely configured — SHALL NOT be adopted: it would place `marshalWire` and the read probe on every frame of every relay-configured deployment permanently, including deployments that configure a relay for a different module entirely.

Letting a feature-off side negotiate `otel-ws` would commit the peer to a wire format whose frames that side hands to the application unparsed (silent payload corruption), so construction SHALL clamp the negotiation outcome such that the wire fact is false whenever the pre-handshake value was false. The reverse direction is unchanged: neither `WithTracingEnabled(true)` nor any relay value can force the envelope onto a connection whose peer did not negotiate `otel-ws`. (Scenario tables including this gate live in `otel-ws.md` §5.)

#### Scenario: Feature-off server does not confirm otel-ws
- **WHEN** a client proposes `otel-ws,json` and the server upgrades with its effective tracing value off
- **THEN** the upgrade succeeds via normal application-protocol selection (`json`), otel-ws is not confirmed, and payloads round-trip between both sides without the envelope

#### Scenario: Feature-off client does not offer otel-ws
- **WHEN** a client dials with its effective tracing value off and a non-empty subprotocol list against an otel-ws-aware server
- **THEN** the handshake proposes only the application protocols, the server does not confirm otel-ws, and messages round-trip unwrapped

#### Scenario: Module switch off suppresses negotiation
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is unset, no option is passed, no relay flag exists, and two peers both use this library's Dial/Upgrade
- **THEN** `otel-ws` is not negotiated and the wire carries raw payloads, matching the release preceding this change

#### Scenario: Relay enable at handshake time negotiates
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is unset, the relay resolves `otel-gorilla-ws-tracing` to `true`, and a connection is dialled afterwards
- **THEN** `otel-ws` is offered and, if the peer agrees, the connection carries the envelope and propagates trace context

#### Scenario: Relay disable at handshake time suppresses negotiation
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is truthy and the relay resolves `otel-gorilla-ws-tracing` to `false` at handshake time
- **THEN** `otel-ws` is neither offered nor confirmed, and a later relay restoration does not envelope that connection

#### Scenario: Envelope is carried while tracing is disabled
- **WHEN** two peers establish a negotiated connection and the relay then disables `otel-gorilla-ws-tracing`
- **THEN** every message still carries the JSON envelope with no trace context, no spans are created, and the receiving application observes the original payload

#### Scenario: Disabling does not remove the envelope's cost
- **WHEN** `otel-gorilla-ws-tracing` is disabled on a negotiated connection
- **THEN** each write still marshals the envelope and each read still runs the probe, so this module alone does not return to the zero-cost path, and removing that wire overhead requires cycling the connection — which the operational documentation SHALL state next to the instruction for disabling a module

### Requirement: NewConn always wraps envelopes
`NewConn(rawConn, opts...)` SHALL enable envelope wrapping **only** when the raw connection's negotiated subprotocol proves `otel-ws` (`isOTelWireProtocol`). It SHALL NOT force envelope wrapping on a connection whose subprotocol does not prove negotiation, because the peer would then receive `{"header":...,"data":...}` frames it never agreed to and hand them to its application unparsed.

Callers that manage the handshake themselves SHALL leave a correct negotiated subprotocol on the raw connection. There SHALL be no option that asserts negotiation without subprotocol evidence.

**The effective feature value SHALL clamp the write decision only.** Whether the peer envelopes is a fact established by the handshake; this side's feature gate is a local policy, and applying the policy to the fact corrupts the read path. A feature-off process wrapping an `otel-ws` connection SHALL therefore write raw frames — safe, because a peer receiving a raw frame falls back to treating it as the payload — while `ReadMessage` SHALL still unwrap, because the peer envelopes every frame regardless of this side's gate. `Conn` SHALL record the negotiated wire fact in a field that the feature gate does not clamp, and the read path SHALL key on that field rather than on the gate.

Unwrapping under a disabled gate is `json.Unmarshal` with the extracted headers discarded — no span, no attribute construction, no propagator call — so the disabled-mode invariant is unaffected.

#### Scenario: NewConn without otel-ws subprotocol stays raw on the wire
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is not an otel-ws protocol and the effective feature value is on
- **THEN** `WriteMessage` sends the application payload bytes unchanged, and a non-instrumented peer observes the original payload

#### Scenario: NewConn with otel-ws subprotocol may envelope
- **WHEN** `NewConn` wraps a connection whose negotiated subprotocol is `otel-ws` or `otel-ws+…` and the effective feature value is on
- **THEN** `WriteMessage`/`ReadMessage` use the JSON envelope for the connection lifetime, independent of later relay changes for the envelope decision

#### Scenario: Feature-off wrapper of a negotiated connection writes raw
- **WHEN** `NewConn` wraps a connection whose subprotocol is `otel-ws+json` while the effective feature value is off, and `WriteMessage` is called
- **THEN** the application payload is sent unchanged, and the peer's probe falls back to it

#### Scenario: Feature-off wrapper of a negotiated connection still unwraps on read
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

Its legacy flat-format branch SHALL return `ok=false` when **neither** `traceparent` nor `tracestate` is present at the top level. A message carrying neither is by definition not a legacy envelope, and the branch's re-marshal of a `map[string]json.RawMessage` sorts keys and normalises whitespace, so returning it would hand the application a byte-different frame — breaking any caller that hashes or signature-verifies the payload. This path is reachable whenever a feature-off peer writes raw frames onto a negotiated connection.

The envelope structure `{"header":…,"data":…}` SHALL be documented in `otel-ws.md` as **reserved** on an `otel-ws` connection: an application payload of that shape is unwrapped and its outer structure discarded. Tightening the match to require `header` to contain only the two trace keys SHALL NOT be done, because a future header member added by the JS packages would then fall into the legacy branch instead.

#### Scenario: JSON object without trace keys survives byte-identically
- **WHEN** a frame containing a JSON object with neither `traceparent` nor `tracestate` is read on a negotiated connection
- **THEN** the application receives the original bytes, key order and whitespace included

#### Scenario: Legacy flat format is still recognised
- **WHEN** a frame contains a top-level `traceparent`
- **THEN** the trace context is extracted and the remaining fields are returned as the payload

### Requirement: NewConn reports configuration errors
`NewConn` SHALL have the signature `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)` so that an invalid environment value detected at construction can be reported, in line with every other option-accepting constructor in the repository (`Dial`, `Upgrader.Upgrade`, and the Mongo and NATS connect variants, all of which already return an error).

`NewConn` is the entry point most likely to be reached by a caller who ran their own handshake and never touched the rest of the configuration, so it is where an unreported configuration error would be least visible.

#### Scenario: NewConn rejects an invalid environment value
- **WHEN** `OTEL_GORILLA_WS_TRACING_ENABLED` is set to `enabled` and `NewConn(raw)` is called
- **THEN** `NewConn` returns a nil `*Conn` and an error wrapping `otelflags.ErrInvalidFlagValue`

#### Scenario: NewConn returns a nil error on success
- **WHEN** `NewConn(raw)` is called with every configured environment value recognised
- **THEN** it returns a usable `*Conn` and a nil error
