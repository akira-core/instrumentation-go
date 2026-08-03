# Changelog

All notable changes to the `otel-gorilla-ws` module are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-gorilla-ws/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.8.0] - unreleased

### Changed

- **BREAKING** The relay proxy is now a **revoke-only kill switch**. Its evaluation default is a literal `true` and the module's environment variable is ANDed separately rather than passed as that default, so a relay flag can turn this module **off** but can never turn it **on**. This replaces the model shipped in the previous release, in which the relay decided in both directions. Deployments that used the relay to *enable* instrumentation must move that decision into their environment configuration.
- **BREAKING** `flags.EnvEnabled` now uses a truthy **allow-list**: only `1`, `true`, `yes`, `on` (trimmed, case-insensitive) enable a switch. Every other set value — including the **empty string**, `enabled`, `2`, `y` and `t` — now reads as disabled, where previously anything outside a four-item falsy list enabled it. `export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=` no longer opens the gate. The direction is fail-safe (less instrumentation, never more), and a set-but-unrecognised value now emits one `slog.Warn` naming the variable, the observed value and the accepted set, so the change announces itself rather than presenting as "spans disappeared after upgrading".
- **BREAKING** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `WithTracingEnabled(v bool)` are now two spellings of **one** switch and are **mutually exclusive**. Supplying both — even with the same value — returns an error from the constructor, matchable with `errors.Is` against the module's exported sentinel. The check is on presence, not value.
- **BREAKING** `WithTracingEnabled` no longer makes a connection **static**. It supplies the first tier and nothing more: a connection carrying it still reads the module environment variable at construction and the relay verdict on **every operation**, and still stops when the relay revokes. There is no way to opt a connection out of a revocation.
- **BREAKING** Implementation selection now keys on `gate1 && OTEL_<MODULE>_TRACING_ENABLED` rather than on the global switch alone. A process with the global switch on and this module's switch off returns to the **zero-cost passthrough** — it no longer allocates the instrumented wrapper — which reverses the regression the previous release introduced. This is only safe because the relay can no longer enable: with the module switch off, no relay value could ever reach the instrumented path.
- Relay verdicts are **no longer cached**. The one-second TTL, its snapshot and its injectable clock are gone, so a revocation takes effect on the next operation rather than up to a TTL later. The cost is roughly 2 µs and 7 allocations per instrumented operation, paid only by wrappers that are actively instrumenting. End-to-end revocation latency is dominated by the provider's poll interval — 60 s by default — not by this library.

### Added

- **Relay control with no application code.** Setting `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` makes the module build a GO Feature Flag provider on first use and bind it to the OpenFeature domain `otel-instrumentation-go`, with `DataCollectorDisabled: true` and in-process evaluation hardcoded. `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` and `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` (Go duration strings, default `60s`) tune it; a malformed interval warns and falls back rather than removing the kill switch. An application that installs its own provider first keeps it and is unaffected — the auto-install stands down.
  - **This adds the GO Feature Flag provider's dependency tree to this module's `go.mod`** (roughly ten modules, including a full ANTLR runtime), for every consumer, whether or not they set the endpoint. It is the price of relay control without an application code change: Go cannot run code from `go.mod` alone.
  - The provider's polling goroutine has **no shutdown**. It lives for the process lifetime. Applications needing lifecycle control install their own provider.
- Setting `OTEL_SERVICE_NAME` supplies a `service.name` attribute on every evaluation, so a relay rule can revoke one service instead of the whole fleet. Supplied on the auto-install path only; an application that installs its own provider owns its evaluation context.

### Fixed

- `ReadMessage` on a connection whose peer negotiated `otel-ws`, in a process where tracing capability is off, now returns the **unwrapped payload** instead of the peer's raw `{"header":...,"data":...}` bytes. Capability clamps the *write* decision only: whether the peer envelopes is a fact of the handshake, not something this side's gate has authority over. Only `NewConn` could produce this state. **This changes the bytes the application receives** in that configuration, and is a fix.
- `tryUnmarshalWire` no longer re-marshals a JSON object that carries neither `traceparent` nor `tracestate`. Such a message is not a legacy flat envelope, and the legacy branch rebuilt its result from a map — which Go serialises with keys sorted — so ordinary payloads came back reordered and whitespace-normalised. Callers that hash or signature-verify a frame were affected. **This changes the bytes the application receives** for those payloads, and is a fix.

### Added

- `SubprotocolOTelWS` and `IsOTelNegotiated(*websocket.Conn) bool` are exported, so a caller running their own handshake can write it correctly and verify the outcome rather than hardcoding an internal string. Additive; neither can force an envelope onto a peer that did not negotiate one. A stock `websocket.Dialer`/`Upgrader` reaches only the bare `otel-ws` form — the `otel-ws+<app>` composite remains exclusive to this package's `Upgrader.Upgrade`.
- **BREAKING** `NewConn` becomes `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)`, so it can report a configuration conflict. It is the entry point most likely to be misconfigured, being the path for callers who run their own handshake.

### Note

- Revoking `otel-gorilla-ws-tracing` stops spans and trace-context injection but **not** the per-message envelope on an already-negotiated connection: the peer parses every frame as one, so dropping it would desynchronise the wire. This module alone does not return to the zero-cost path on revocation; removing the wire overhead requires a redeploy with `OTEL_GORILLA_WS_TRACING_ENABLED` off.

<details>
<summary>Superseded within this same unreleased version</summary>

The entries below describe the first implementation of dynamic flags, in which the
relay decided in both directions and `WithTracingEnabled` pinned a connection static.
That model was replaced during design review before release; where the two disagree,
the entries above win. Kept as the record of what changed and why it changed again.

### Changed

- **BREAKING** The module's tracing environment variable is demoted from *final say* to the **default value** used when the relay proxy has no opinion. A relay flag can now turn this module on when the environment variable leaves it off, and off when the environment variable turns it on. Operators who relied on setting it to `false` as a hard guarantee must use `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` (whole process) or `WithTracingEnabled(false)` (one connection) instead — neither can be crossed by the relay.
- **BREAKING** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` alone now decides whether the instrumented or the passthrough implementation is constructed. A process running with the global switch on and this module's switch off previously took the zero-cost passthrough path; it now allocates the instrumented wrapper and performs one atomic load plus one monotonic clock read per operation. It still emits no spans.

### Added

- Tracing is resolved at runtime through [OpenFeature](https://openfeature.dev) instead of once at process start, so an operator can turn it on or off through a GO Feature Flag relay proxy without restarting the application. Values are cached in a per-module snapshot with a fixed one-second TTL; hot paths never enter the OpenFeature evaluation pipeline.
- Applications opt in by installing a provider at startup — the library never calls `openfeature.SetProvider`, exactly as it never initializes a `TracerProvider`:

  ```go
  provider, _ := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{Endpoint: "http://relay:1031"})
  _ = openfeature.SetProviderAndWait(provider)
  ```

  With no provider installed, **span on/off** still follows the environment variables as before.
  **Exception:** otel-ws negotiation is gated on the global kill switch alone (not
  `GLOBAL && OTEL_GORILLA_WS_TRACING_ENABLED`), so env-only global-on + module-off
  may negotiate the envelope between library peers. Non-negotiating peers and
  `NewConn` without an otel-ws subprotocol still see raw wire payloads.
- `github.com/open-feature/go-sdk` is a new dependency. The GO Feature Flag provider is an application-side dependency, not a library one.

- **BREAKING** `NewConn` no longer forces the wire envelope on. It previously wrapped any `*websocket.Conn` with `tracingEnabled = true`, so two peers that both used `NewConn` exchanged the JSON envelope and propagated trace context even though neither had negotiated `otel-ws`. It now derives the envelope decision from the raw connection's *negotiated subprotocol* (`otel-ws` / `otel-ws+<proto>`), and construction additionally clamps it to false whenever the connection is not capable. Consequences:
  - Callers that manage the handshake themselves and do **not** negotiate `otel-ws` lose WebSocket trace propagation entirely — `ReadMessage` no longer unwraps and `WriteMessage` no longer writes the envelope. Spans are still created while the feature gate is on, but carry no remote parent/link. To keep propagation, negotiate `otel-ws` in your own handshake, or switch to `Dial` / `Upgrader.Upgrade`.
  - `WithTracingEnabled(true)` does **not** restore the envelope: it sets capability only, and the negotiation outcome still requires a proven `otel-ws` subprotocol.
  - Conversely, on a connection whose peer *did* negotiate `otel-ws`, a capability-off wrapper (`WithTracingEnabled(false)`, or the global kill switch off) hands the peer's envelope bytes to the application unparsed. Do not wrap an `otel-ws` connection with tracing disabled.

- **BREAKING** `Dial` now offers, and `Upgrader.Upgrade` now confirms, the `otel-ws` subprotocol whenever `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is on — independent of the dynamic flag. Negotiation happens during the handshake and cannot be revisited, so gating it on a value that may flip a second later would leave every connection established during an "off" window permanently unable to propagate trace context. Consequence: two peers that both run this library with the global switch on now exchange the JSON envelope on every message even while tracing is off. Peers that do not negotiate `otel-ws` — including all non-library clients — see no change on the wire.
- A connection that negotiated `otel-ws` and is then dynamically disabled keeps writing envelopes (the peer expects them) with an empty header and creates no spans.

### Flag keys

| OpenFeature key | Fallback environment variable |
|---|---|
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` |

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` has **no** relay counterpart: it is an out-of-band kill switch that works when the relay is unreachable or misconfigured.

</details>

## [0.7.0] - 2026-07-15

### Fixed

- **Wire-format corruption when negotiation and the feature flag disagreed.** `Dial` no longer offers, and `Upgrader.Upgrade` no longer confirms, the `otel-ws` subprotocol when the connection's effective tracing feature is off (env gates, or `WithTracingEnabled(false)`). Previously a feature-off side could still negotiate otel-ws, committing the peer to the JSON envelope wire format that the feature-off side neither writes nor unwraps — the application then received raw `{"header":...,"data":...}` envelope bytes instead of the payload. Negotiation now always reflects actual envelope capability.

### Changed — BREAKING

- Attribute set right-sized: send/receive spans no longer carry the `messaging.*` namespace (this package is not a messaging-system wrapper); `websocket.message.type` and `websocket.message.body.size` remain.
- As part of the negotiation fix above: with the env gates off (their default), `Dial` no longer advertises `otel-ws` in the handshake. Deployments that relied on negotiating otel-ws while running with tracing disabled (a corrupting combination when one side was enabled) must enable the feature via env or `WithTracingEnabled(true)`.
- `Upgrader.Upgrade` gained a variadic `opts ...Option` parameter. Ordinary 3-argument calls (`up.Upgrade(w, r, header)`) are source-compatible, but this changes the method's Go type — any existing method-value assignment (e.g. `f := upgrader.Upgrade`) or interface satisfying the old 3-arg signature will fail to compile. **Migration:** wrap the call in a 3-arg closure, e.g. `f := func(w http.ResponseWriter, r *http.Request, h http.Header) (*Conn, error) { return upgrader.Upgrade(w, r, h) }`.

### Added

- `WithTracingEnabled(v bool) Option` overrides the env-gate default (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_GORILLA_WS_TRACING_ENABLED`) for a single `Conn`, in either direction. Applies to `NewConn`, `Dial`, and `Upgrader.Upgrade`. In `Dial`/`Upgrade` the effective flag also gates otel-ws subprotocol negotiation (see Fixed above); negotiation outcome (`Conn.tracingEnabled`) still requires both sides to agree — `WithTracingEnabled(true)` cannot force the envelope onto a peer that did not negotiate it.

## [0.6.0] - 2026-07-08

Highlights for this module:

- Dependency currency only in this release: `go.opentelemetry.io/otel` v1.44.0. Go toolchain floor raised to 1.25. Public API unchanged.
- Module path renamed from `Marz32onE/instrumentation-go` to `akira-core/instrumentation-go`.
