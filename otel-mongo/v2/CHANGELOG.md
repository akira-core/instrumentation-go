# Changelog

All notable changes to the `otel-mongo/v2` module (`go.mongodb.org/mongo-driver/v2`) are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy. This module is versioned and changed in parity with the v1 `otel-mongo` module — see `../CHANGELOG.md`.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-mongo/v2/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [2.8.0] - unreleased

> **Release candidates.** `v2.8.0-rc.2` is the first one that carries the `otel-flags` 0.2.0 flag layer.
> `rc.1` builds and runs, but against `otel-flags` 0.1.0, where flag keys are bound to a resolver
> **by index**, the OpenFeature provider is installed lazily at the first evaluation, and an
> unreadable `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` or `_POLL_INTERVAL` warns instead of failing
> construction. Test the relay against `2.8.0-rc.2` or later.
>
> What `2.8.0-rc.2` adds over `rc.1`: flag keys passed **by name**, which removes a coupling in which
> swapping two registration lines compiled, passed, and silently made one flag control another; the
> provider installed at construction through `otelflags.ValidateAndInstall`; the two
> relay-connection variables validated at construction; evaluation error codes logged once per
> transition, two-tier, so relay silence is distinguishable from relay failure in the log even
> though it is not in the value; a failed provider install retried rather than latched; and the
> switch matrix covered end to end against a real GO Feature Flag relay proxy — including the master switch's relay key, which no test in this module previously exercised.

### Changed

- **BREAKING** Feature switches now resolve down a **four-step ladder** — `relay > env > option > default`, first source with an opinion winning — and the relay proxy is **authoritative in both directions**: it can turn this module off, and it can turn it on when the deployment left it off. This replaces the revoke-only model that was developed but never released. Safety now comes from the defaults rather than from a restriction on the relay: every per-module switch defaults to **off**, and the process-wide master switch defaults to **on** only because it is a veto rather than an enabler.
- **BREAKING** `OTEL_MONGO_TRACING_ENABLED` now takes effect **without** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` also being set. The master switch defaults to enabled, so the module variable alone decides. Previously it was inert unless the global one was set too — a common "I set the flag and nothing happened" report. Deployments that set the module variable expecting it to be ignored will now see this module trace.
- **BREAKING** `WithTracingEnabled` / `WithTracePropagationEnabled` now sits **below** its environment variable, reversing `0.7.0`, where the option won. `OTEL_MONGO_TRACING_ENABLED` overrides the option, so a deployment can disable this module even where the application's Go code asked for tracing. With the variable unset the option still decides, so two connections in one process can still differ. Three reasons, in order of weight: the operator gets a per-module setting code cannot override; `WithTracePropagationEnabled(true)` can no longer bypass `OTEL_MONGO_PROPAGATION_ENABLED=false` and write permanent `_oteltrace` fields into the operator's documents; and the ladder stays monotonic in deployment order.
- **BREAKING** The option no longer supplies the process-wide master switch. It supplies this module's tier for one connection or client. A per-connection value cannot coherently spell a process-wide switch, and keeping the master option-free is what guarantees a single environment variable still stops everything.
- **BREAKING** Environment values are now a strict **tri-state**, and an unreadable one **fails construction**. Unset means "no opinion" and falls through to the option, then the default. `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off` (trimmed, case-insensitive) decide. **Everything else — including the empty string, `enabled`, `2`, `y` and `t` — returns an error** wrapping `otelflags.ErrInvalidFlagValue`, naming the variable and the observed value. Under a ladder there is no safe direction to guess in: the master tier defaults to `true` and every other tier to `false`, so a value silently read as `false` would stop a whole fleet on one tier and change nothing on the others. **Before upgrading, grep your deployment configuration for `OTEL_*_ENABLED` and confirm every value is in one of those two lists** — an unexpanded `${SOMETHING}` in a Kubernetes manifest reaches exactly the empty-string case, and this is the one change that can stop a process from starting.
- **BREAKING** A constructor that reads several switches reports **all** the bad values in one joined error, so one run tells you everything to fix.
- **BREAKING** The two relay-connection variables `otel-flags` owns are now validated at construction, and an unreadable one **fails it**: `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` must be a positive Go duration and `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` a URL with a scheme and a host, both checked whether or not a relay is configured. The interval previously warned and fell back to `60s`. Blank still means "not configured" for both, and the API key is never validated. **Before upgrading, grep your deployment configuration for `=60` on the poll interval** — it was never read as seconds and is now rejected outright rather than ignored. Requires `otel-flags v0.2.0`, which also renames `InstallProvider` to `SetNamedProvider` for applications that install their own provider.
- A failing relay is no longer silent. `otel-flags v0.2.0` logs a flag key's OpenFeature error code when it **changes** — `FLAG_NOT_FOUND` and `PROVIDER_NOT_READY` at debug, everything else at warn, a recovery at info — so a provider stuck in `ERROR` or a relay rule that never matches is visible in the application's own logs. The resolved value is unchanged: relay silence and relay failure both still mean "the next rung down decides".
- **BREAKING** This module now requires `github.com/akira-core/instrumentation-go/otel-flags`, which replaces the vendored `internal/flags` package. That module carries the OpenFeature SDK and the GO Feature Flag provider, so their dependency trees enter your build transitively — roughly ten modules including a full ANTLR runtime, a JSONLogic evaluator and a rules engine. The cost lands on `go.sum`, vulnerability-scanning surface and licence review, not on runtime; the linker drops unreached code. It exists to guarantee **exactly one** OpenFeature provider per binary, which four independent vendored copies could not.
- **BREAKING** The master switch gains a relay key, `otel-instrumentation-go-tracing`. Setting it to `true` has **no effect** — that is already the default. Setting it to `false` stops every module in every process the relay serves, including connections whose Go code passed an option. Do not create it expecting an enable; it will read as a broken flag.
- The relay is resolved on **every operation** and nothing is cached, so a flag change takes effect on the next one. An instrumented operation now makes two (read) or three (write) evaluations at roughly 2 µs and 7 allocations each. A process with **no relay configured** pays none of it and allocates no instrumented implementation it cannot reach — the pre-dynamic zero-cost path is preserved exactly.
- **New ordering requirement.** Whether a relay can exist is resolved once, when a wrapper is constructed. An application that installs its own OpenFeature provider must do so **before** constructing any wrapper; one built earlier resolves statically for its whole life and never consults the relay. Applications using the zero-code path (`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`) are unaffected.
- The non-blocking startup window is now **fail-safe**: until the provider's first fetch every switch resolves to its local value, so the window can delay a relay-driven enable but can never introduce one.
- **BREAKING** `ContextFromDocument` and `ContextFromRawDocument` lose their feature-flag gate entirely — not the master switch, not the module variables, not the options, not the relay. They start no span, build no attributes, initialise nothing in the OTel SDK, write nothing and perform no OpenFeature evaluation; they read a field out of a value you already hold. A process with every switch off now gets a valid span context from them where it previously got nothing, so **turning this module off does not stop trace-context extraction**. That is deliberate: `Decode` + `ContextFromDocument` is the supported way to keep trace linking while the library is silenced. A deployment that switched a variable off specifically to stop trace linking must stop calling them instead.
- **BREAKING** `OTEL_MONGO_PROPAGATION_ENABLED` can no longer be overridden by `WithTracePropagationEnabled`. This is the switch the ordering change exists for: `_oteltrace` is roughly 90 bytes appended to your own documents, never stripped on read, undone only by a `$unset` migration, and a hard write failure against a collection using `$jsonSchema` with `additionalProperties: false`. An operator must be able to stop it without a code change.
- The relay **can** now start `_oteltrace` writes, which the revoke-only model could not. Four things bound that: the master veto, `OTEL_MONGO_PROPAGATION_ENABLED=false` (which code cannot override), the tier's hardcoded default of `false` so absence never enables it, and the fact that a process with no relay configured cannot be reached at all.
- `InjectTraceIntoDocument` removes any existing `_oteltrace` before appending. It appended unconditionally, so an ordinary read-modify-write produced two copies — and because extraction resolves the field with `bson.Raw.LookupErr`, which returns the **first** match, such a loop pinned the trace linkage to the original write permanently.
- `shared.NewCommandMonitor` — which runs on every MongoDB command — is registered on the same condition that allocates the instrumented implementation, so a process with no relay and tracing off does not pay for it.
- **BREAKING** Renamed the exported method `DecodeWithContext(ctx, val) (context.Context, error)` to `DecodeAndTrace(ctx, val) (context.Context, error)` on both `Cursor` and `ChangeStream` (parity with v1 `0.8.0`). Signature and behavior are unchanged — the new name states the trace side effect (emit a `mongo.cursor.decode` span, extract `_oteltrace`) that plain `Decode` does not have. **Migration**: replace `cursor.DecodeWithContext(...)` / `changeStream.DecodeWithContext(...)` with `DecodeAndTrace(...)`; arguments and returns are identical. This was staged on 2026-07-21 as its own `2.8.0`, but no tag was ever pushed, so it reaches consumers for the first time here.

### Removed

- **BREAKING** `ErrTracingConfigConflict` and `ErrTracePropagationConfigConflict` and the mutual-exclusion rule they reported. Supplying an option alongside its paired environment variable is now ordinary configuration with a defined winner (the variable). Roughly 89 in-repo call sites that combined the two become legal again.
- **BREAKING (internal)** The vendored `internal/flags` package, including `Gate`, `NewGate`, `ResetForTest`, `EnvEnabled` and `EnvSet`. `otelflags.Lookup` replaces the last two; the rest have no successor, because nothing is cached and therefore nothing needs re-arming.

### Notes

- **Test coverage.** The master switch's **relay** key (`otel-instrumentation-go-tracing`) is now unit-tested here; before this it was covered only in `otel-nats`, so "one relay flag stops every module" was unverified for this one. A new `tests/integration/relayflags_test.go` runs a real GO Feature Flag relay proxy against a real MongoDB and asserts the switches on the **stored documents**: the relay revoking what the environment enabled, the relay **enabling** propagation a deployment explicitly disabled, propagation revoked while tracing stays on, the master veto, and a relay that defines neither key leaving the environment in charge. Kept in parity with the v1 module.
- **Flag changes are not immediate.** End-to-end latency is the provider's poll interval — 60 s by default — in **both** directions. This module adds none of its own.
- The library still never touches the **default** OpenFeature provider, the global evaluation context, hooks or shutdown. The one piece of state it may write is a **named** provider on `otel-instrumentation-go`, and only when the environment asks for one and the application installed none. `DataCollectorDisabled: true` and in-process evaluation are hardcoded on that path.
- Full reference: [`docs/feature-flags.md`](../../docs/feature-flags.md) ([繁體中文](../../docs/feature-flags.zh-TW.md)).

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
