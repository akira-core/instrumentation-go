# Changelog

All notable changes to the `otel-mongo` module (v1, `go.mongodb.org/mongo-driver`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy. For the `v2` sub-module (separate `go.mod`, `go.mongodb.org/mongo-driver/v2`), see `v2/CHANGELOG.md` — the two modules are versioned and changed in parity.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-mongo/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.7.0] - 2026-07-15

### Fixed

- `resolveDocumentPropagation` (internal) now takes the caller's already-resolved effective tracing state as a parameter instead of recomputing the env-only gate internally. This was a latent bug for the (previously unreachable) case of a per-client tracing override: without this fix, `WithTracingEnabled(true)` combined with `WithTracePropagationEnabled(true)` would have silently stayed disabled. The process-wide, env-only `ContextFromDocument`/`ContextFromRawDocument` gate is unaffected — it explicitly passes the plain env-derived value.

### Changed — BREAKING

- **Deliver spans removed.** The synthetic "deliver" span pattern (independent OTLP-gated `TracerProvider`, `StartDeliverSpan`/`DeliverTracer`/`DeliverAttributes`/`initMongoProvider`, and every call site) is gone. The package no longer reads `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission, and the Grafana service-graph broker node is no longer emitted.
- Change-stream read span kind corrected to the OTel spec: `CONSUMER` → `CLIENT`.

### Added

- `WithTracingEnabled(v bool) ClientOption` overrides the env-gate default (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_MONGO_TRACING_ENABLED`) for a single `Client`, in either direction. Applies to everything constructed from that `Client` — `Database`, `Collection` (including its strategy-split direct/traced impl selection), `Cursor`, `ChangeStream`. `WithTracePropagationEnabled` continues to govern only the propagation default and still requires the client's effective tracing to be on.

## [0.6.1] - 2026 (tagged, not separately GitHub-released — see `VERSIONING.md`)

- `server.address`/`server.port` are now captured per-command from the real connection via a `CommandMonitor`, instead of the static value parsed once from the connection URI at `Connect` time — accurate under DNS/SRV resolution and multi-host topologies.
- `ChangeStream` reader spans restore static `server.*` attributes (regression from the per-command capture work, fixed same range).
- `parseServerFromURI` hardened for multi-host replica-set URIs, IPv6 hosts, and stray whitespace picked up when a URI is assembled across config-file lines.

## [0.6.0] - 2026-07-08

Highlights for this module:

- Dependencies refreshed: `go.opentelemetry.io/otel` v1.44.0, `go.mongodb.org/mongo-driver` v1.17.9, `semconv` v1.39.0. Go toolchain floor raised to 1.25.
- Module path renamed from `Marz32onE/instrumentation-go` to `akira-core/instrumentation-go`.
