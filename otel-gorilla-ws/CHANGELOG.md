# Changelog

All notable changes to the `otel-gorilla-ws` module are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-gorilla-ws/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.7.0] - Unreleased

### Fixed

- **Wire-format corruption when negotiation and the feature flag disagreed.** `Dial` no longer offers, and `Upgrader.Upgrade` no longer confirms, the `otel-ws` subprotocol when the connection's effective tracing feature is off (env gates, or `WithTracingEnabled(false)`). Previously a feature-off side could still negotiate otel-ws, committing the peer to the JSON envelope wire format that the feature-off side neither writes nor unwraps — the application then received raw `{"header":...,"data":...}` envelope bytes instead of the payload. Negotiation now always reflects actual envelope capability.

### Changed — BREAKING

- Attribute set right-sized: send/receive spans no longer carry the `messaging.*` namespace (this package is not a messaging-system wrapper); `websocket.message.type` and `websocket.message.body.size` remain.
- As part of the negotiation fix above: with the env gates off (their default), `Dial` no longer advertises `otel-ws` in the handshake. Deployments that relied on negotiating otel-ws while running with tracing disabled (a corrupting combination when one side was enabled) must enable the feature via env or `WithTracingEnabled(true)`.

### Added

- `WithTracingEnabled(v bool) Option` overrides the env-gate default (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_GORILLA_WS_TRACING_ENABLED`) for a single `Conn`, in either direction. Applies to `NewConn`, `Dial`, and `Upgrader.Upgrade`. In `Dial`/`Upgrade` the effective flag also gates otel-ws subprotocol negotiation (see Fixed above); negotiation outcome (`Conn.tracingEnabled`) still requires both sides to agree — `WithTracingEnabled(true)` cannot force the envelope onto a peer that did not negotiate it.
- `Upgrader.Upgrade` gained a variadic `opts ...Option` parameter (backward-compatible — it previously had none, so `WithTracerProvider`/`WithPropagators` could not reach server-side connections either).

## [0.6.0] - 2026-07-08

See [`RELEASE-NOTES-0.6.0.md`](../RELEASE-NOTES-0.6.0.md) at the repo root for the full cross-module notes. Highlights for this module:

- Dependency currency only in this release: `go.opentelemetry.io/otel` v1.44.0. Go toolchain floor raised to 1.25. Public API unchanged.
- Module path renamed from `Marz32onE/instrumentation-go` to `akira-core/instrumentation-go`.
