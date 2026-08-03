# Changelog

All notable changes to the `otel-mongo` module (v1, `go.mongodb.org/mongo-driver`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy. For the `v2` sub-module (separate `go.mod`, `go.mongodb.org/mongo-driver/v2`), see `v2/CHANGELOG.md` — the two modules are versioned and changed in parity.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-mongo/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.9.0] - unreleased

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

- `InjectTraceIntoDocument` / `InjectTraceIntoUpdate` now remove any existing `_oteltrace` before appending. They appended unconditionally, so the ordinary read-modify-write cycle — the field is never stripped on read — produced two copies. Because `ExtractMetadataFromRaw` resolves the field with `bson.Raw.LookupErr`, which returns the **first** match, such a loop pinned trace linkage to the original write permanently.

### Changed

- **BREAKING** `ContextFromDocument` and `ContextFromRawDocument` lose their feature-flag gate entirely. Neither emits a span, writes to a document, or touches the OTel SDK — they read a field out of a value the caller already holds. A process with every switch off now gets a valid span context from them where it previously got nothing, and **a revocation no longer stops trace-context extraction**. Deployments that switched an environment variable off specifically to stop trace linking must stop calling them instead. `Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace` stay gated, because each starts and ends a real span.
- **BREAKING** `OTEL_MONGO_PROPAGATION_ENABLED` and `WithTracePropagationEnabled` are mutually exclusive under the same presence rule as the tracing pair, with their own `ErrTracePropagationConfigConflict` sentinel. A call violating both rules gets a single `errors.Join`ed error matching both sentinels.

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

</details>

## [0.8.0] - 2026-07-21

### Changed — BREAKING

- Renamed the exported method `DecodeWithContext(ctx, val) (context.Context, error)` to `DecodeAndTrace(ctx, val) (context.Context, error)` on both `Cursor` and `ChangeStream`. Signature and behavior are unchanged — the new name states the trace side effect (emit a `mongo.cursor.decode` span, extract `_oteltrace`) that plain `Decode` does not have. **Migration**: replace `cursor.DecodeWithContext(...)` / `changeStream.DecodeWithContext(...)` with `DecodeAndTrace(...)`; arguments and returns are identical.

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
