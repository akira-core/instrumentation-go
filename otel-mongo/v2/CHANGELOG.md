# Changelog

All notable changes to the `otel-mongo/v2` module (`go.mongodb.org/mongo-driver/v2`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy. This module is versioned and changed in parity with the v1 `otel-mongo` module — see `../CHANGELOG.md`.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-mongo/v2/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [2.9.0] - 2026-07-31

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

  With no provider installed, span/propagation on/off still follows the environment variables as before.
- `github.com/open-feature/go-sdk` is a new dependency. The GO Feature Flag provider is an application-side dependency, not a library one.

- **BREAKING** `ContextFromDocument` and `ContextFromRawDocument` now resolve through the same snapshot as the `Collection` path instead of a permanently cached, environment-only gate. A relay flag that disables Mongo propagation now also stops change-stream readers from extracting trace context, matching the `Collection` path in the same loop. They still ignore per-connection options, as documented.
- `Collection`, `Cursor` and `ChangeStream` now hold both the passthrough and the instrumented implementation and select between them per operation, so a long-lived change stream follows a flag change without being reopened. `SingleResult` is the exception: it holds the live `FindOne` span, so its implementation stays fixed by whichever path executed the `FindOne`.

### Flag keys

| OpenFeature key | Fallback environment variable |
|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` |

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` has **no** relay counterpart: it is an out-of-band kill switch that works when the relay is unreachable or misconfigured.

## [2.8.0] - 2026-07-21

### Changed — BREAKING

- Renamed the exported method `DecodeWithContext(ctx, val) (context.Context, error)` to `DecodeAndTrace(ctx, val) (context.Context, error)` on both `Cursor` and `ChangeStream` (parity with v1 `0.8.0`). Signature and behavior are unchanged — the new name states the trace side effect (emit a `mongo.cursor.decode` span, extract `_oteltrace`) that plain `Decode` does not have. **Migration**: replace `cursor.DecodeWithContext(...)` / `changeStream.DecodeWithContext(...)` with `DecodeAndTrace(...)`; arguments and returns are identical.

## [2.7.0] - 2026-07-15

Re-versioning of the `0.7.0` content (below) under the module's Go-resolvable `v2.x.y` tag line — the module path ends in the `/v2` major-version suffix, so Go requires version major 2 and the tag shape `otel-mongo/v2.x.y`; every old `otel-mongo/v2/v0.x.y` tag was never resolvable by `go get`. `v2.MINOR.PATCH` tracks the sibling modules' `0.MINOR.PATCH` — see `VERSIONING.md`. No code change relative to `0.7.0` other than the version constant: `otel.scope.version` on emitted spans now reports `2.7.0`.

## [0.7.0] - 2026-07-15 (tag `otel-mongo/v2/v0.7.0` — not resolvable by Go tooling; use `v2.7.0`)

### Fixed

- `resolveDocumentPropagation` (internal) now takes the caller's already-resolved effective tracing state as a parameter instead of recomputing the env-only gate internally. This was a latent bug for the (previously unreachable) case of a per-client tracing override: without this fix, `WithTracingEnabled(true)` combined with `WithTracePropagationEnabled(true)` would have silently stayed disabled. The process-wide, env-only `ContextFromDocument`/`ContextFromRawDocument` gate is unaffected — it explicitly passes the plain env-derived value.
- `ConnectWithOptions` no longer mutates a caller-supplied `*options.ClientOptions`: driver v2's `MergeClientOptions` returns the caller's own struct when exactly one is passed (unlike v1, which always builds a fresh one), so registering the command monitor used to overwrite the caller's `Monitor` field in place — and re-wrap it on every reuse of the same options value. Merging now goes through a fresh base.

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

- Dependencies refreshed: `go.opentelemetry.io/otel` v1.44.0, `go.mongodb.org/mongo-driver/v2` v2.7.0, `semconv` v1.39.0. Go toolchain floor raised to 1.25.
- Module path renamed from `Marz32onE/instrumentation-go` to `akira-core/instrumentation-go`.
