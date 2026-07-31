# Changelog

All notable changes to the `otel-gorilla-ws` module are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-gorilla-ws/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.8.0] - 2026-07-31

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

  With no provider installed, behavior is identical to the previous release.
- `github.com/open-feature/go-sdk` is a new dependency. The GO Feature Flag provider is an application-side dependency, not a library one.

- **BREAKING** `Dial` now offers, and `Upgrader.Upgrade` now confirms, the `otel-ws` subprotocol whenever `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is on — independent of the dynamic flag. Negotiation happens during the handshake and cannot be revisited, so gating it on a value that may flip a second later would leave every connection established during an "off" window permanently unable to propagate trace context. Consequence: two peers that both run this library with the global switch on now exchange the JSON envelope on every message even while tracing is off. Peers that do not negotiate `otel-ws` — including all non-library clients — see no change on the wire.
- A connection that negotiated `otel-ws` and is then dynamically disabled keeps writing envelopes (the peer expects them) with an empty header and creates no spans.

### Flag keys

| OpenFeature key | Fallback environment variable |
|---|---|
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` |

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` has **no** relay counterpart: it is an out-of-band kill switch that works when the relay is unreachable or misconfigured.

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
