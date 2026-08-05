# Changelog

All notable changes to the `otel-gorilla-ws` module are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Coverage note**: this file starts at `0.6.0`. Earlier history lives only in git tags (`otel-gorilla-ws/vX.Y.Z`) — see the repo root `VERSIONING.md` for the root cause and the release-tag CI guard that now keeps the version constant and tag in sync going forward.

## [0.8.0] - unreleased

### Changed

- **BREAKING** Feature switches now resolve down a **four-step ladder** — `relay > env > option > default`, first source with an opinion winning — and the relay proxy is **authoritative in both directions**: it can turn this module off, and it can turn it on when the deployment left it off. This replaces the revoke-only model that was developed but never released. Safety now comes from the defaults rather than from a restriction on the relay: every per-module switch defaults to **off**, and the process-wide master switch defaults to **on** only because it is a veto rather than an enabler.
- **BREAKING** `OTEL_GORILLA_WS_TRACING_ENABLED` now takes effect **without** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` also being set. The master switch defaults to enabled, so the module variable alone decides. Previously it was inert unless the global one was set too — a common "I set the flag and nothing happened" report. Deployments that set the module variable expecting it to be ignored will now see this module trace.
- **BREAKING** `WithTracingEnabled` now sits **below** its environment variable, reversing `0.7.0`, where the option won. `OTEL_GORILLA_WS_TRACING_ENABLED` overrides the option, so a deployment can disable this module even where the application's Go code asked for tracing. With the variable unset the option still decides, so two connections in one process can still differ. Three reasons, in order of weight: the operator gets a per-module setting code cannot override; the rule is uniform across the repository, and the case that forces it is `otel-mongo`'s document propagation, where an option must not be able to bypass an operator's `OTEL_MONGO_PROPAGATION_ENABLED=false` and write permanent fields into their documents; and the ladder stays monotonic in deployment order.
- **BREAKING** The option no longer supplies the process-wide master switch. It supplies this module's tier for one connection or client. A per-connection value cannot coherently spell a process-wide switch, and keeping the master option-free is what guarantees a single environment variable still stops everything.
- **BREAKING** Environment values are now a strict **tri-state**, and an unreadable one **fails construction**. Unset means "no opinion" and falls through to the option, then the default. `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off` (trimmed, case-insensitive) decide. **Everything else — including the empty string, `enabled`, `2`, `y` and `t` — returns an error** wrapping `otelflags.ErrInvalidFlagValue`, naming the variable and the observed value. Under a ladder there is no safe direction to guess in: the master tier defaults to `true` and every other tier to `false`, so a value silently read as `false` would stop a whole fleet on one tier and change nothing on the others. **Before upgrading, grep your deployment configuration for `OTEL_*_ENABLED` and confirm every value is in one of those two lists** — an unexpanded `${SOMETHING}` in a Kubernetes manifest reaches exactly the empty-string case, and this is the one change that can stop a process from starting.
- **BREAKING** A constructor that reads several switches reports **all** the bad values in one joined error, so one run tells you everything to fix.
- **BREAKING** The two relay-connection variables `otel-flags` owns are now validated at construction, and an unreadable one **fails it**: `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` must be a positive Go duration and `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` a URL with a scheme and a host, both checked whether or not a relay is configured. The interval previously warned and fell back to `60s`. Blank still means "not configured" for both, and the API key is never validated. **Before upgrading, grep your deployment configuration for `=60` on the poll interval** — it was never read as seconds and is now rejected outright rather than ignored. Requires `otel-flags v0.2.0`, which also renames `InstallProvider` to `SetNamedProvider` for applications that install their own provider.
- A failing relay is no longer silent. `otel-flags v0.2.0` logs a flag key's OpenFeature error code when it **changes** — `FLAG_NOT_FOUND` and `PROVIDER_NOT_READY` at debug, everything else at warn, a recovery at info — so a provider stuck in `ERROR` or a relay rule that never matches is visible in the application's own logs. The resolved value is unchanged: relay silence and relay failure both still mean "the next rung down decides".
- **BREAKING** This module now requires `github.com/akira-core/instrumentation-go/otel-flags`, which replaces the vendored `internal/flags` package. That module carries the OpenFeature SDK and the GO Feature Flag provider, so their dependency trees enter your build transitively — roughly ten modules including a full ANTLR runtime, a JSONLogic evaluator and a rules engine. The cost lands on `go.sum`, vulnerability-scanning surface and licence review, not on runtime; the linker drops unreached code. It exists to guarantee **exactly one** OpenFeature provider per binary, which four independent vendored copies could not.
- **BREAKING** The master switch gains a relay key, `otel-instrumentation-go-tracing`. Setting it to `true` has **no effect** — that is already the default. Setting it to `false` stops every module in every process the relay serves, including connections whose Go code passed an option. Do not create it expecting an enable; it will read as a broken flag.
- The relay is resolved on **every operation** and nothing is cached, so a flag change takes effect on the next one. An instrumented operation now makes two evaluations at roughly 2 µs and 7 allocations each. A process with **no relay configured** pays none of it and allocates no instrumented implementation it cannot reach — the pre-dynamic zero-cost path is preserved exactly.
- **New ordering requirement.** Whether a relay can exist is resolved once, when a wrapper is constructed. An application that installs its own OpenFeature provider must do so **before** constructing any wrapper; one built earlier resolves statically for its whole life and never consults the relay. Applications using the zero-code path (`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`) are unaffected.
- The non-blocking startup window is now **fail-safe**: until the provider's first fetch every switch resolves to its local value, so the window can delay a relay-driven enable but can never introduce one.
- **BREAKING** `NewConn` becomes `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)`, so it can report an unreadable environment value. It is the entry point most likely to be reached by a caller who ran their own handshake and never touched the rest of the configuration.
- **BREAKING** `otel-ws` subprotocol negotiation now follows the connection's effective tracing value **including the relay**, resolved once immediately before the handshake. A handshake cannot be revisited, which produces an asymmetry that must be planned around: **enabling reaches only connections opened afterwards** (an existing connection never gains the envelope, and `WithTracingEnabled(true)` cannot restore it — such a connection can still emit local spans but cannot inject or extract), while **disabling reaches every connection immediately for spans and inject/extract but not for the envelope**, which the peer is still parsing. This is the one module of the four that does not return to the zero-cost path when you turn it off; removing that wire overhead requires cycling the connection.
- With **no relay configured**, negotiation resolves to exactly what `0.7.0` computed, so such deployments see the previous release's wire byte for byte.
- Two behaviour changes that alter returned bytes without a signature change, both fixes: `ReadMessage` on a connection that proved `otel-ws` while the feature is off now returns the **unwrapped payload** instead of the peer's envelope bytes, and a JSON-object payload carrying neither trace key is returned **byte-identical** instead of re-marshalled with sorted keys.

### Removed

- **BREAKING** `ErrTracingConfigConflict` and the mutual-exclusion rule they reported. Supplying an option alongside its paired environment variable is now ordinary configuration with a defined winner (the variable). Roughly 89 in-repo call sites that combined the two become legal again.
- **BREAKING (internal)** The vendored `internal/flags` package, including `Gate`, `NewGate`, `ResetForTest`, `EnvEnabled` and `EnvSet`. `otelflags.Lookup` replaces the last two; the rest have no successor, because nothing is cached and therefore nothing needs re-arming.

### Notes

- **Flag changes are not immediate.** End-to-end latency is the provider's poll interval — 60 s by default — in **both** directions. This module adds none of its own.
- The library still never touches the **default** OpenFeature provider, the global evaluation context, hooks or shutdown. The one piece of state it may write is a **named** provider on `otel-instrumentation-go`, and only when the environment asks for one and the application installed none. `DataCollectorDisabled: true` and in-process evaluation are hardcoded on that path.
- Full reference: [`docs/feature-flags.md`](../docs/feature-flags.md) ([繁體中文](../docs/feature-flags.zh-TW.md)).

### Added

- `SubprotocolOTelWS` and `IsOTelNegotiated(conn *websocket.Conn) bool`, so a caller running their own handshake can offer the correct token and verify the outcome rather than hardcoding an internal string. Neither can force an envelope onto a peer that did not negotiate one.

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
