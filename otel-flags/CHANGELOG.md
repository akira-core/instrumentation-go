# Changelog

All notable changes to the `otel-flags` module are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Release ordering**: this module is tagged **before** the instrumentation modules that require it. A published `go.mod` cannot carry a `replace` directive — consumers ignore it — so `otel-mongo`, `otel-mongo/v2`, `otel-nats` and `otel-gorilla-ws` can only name a version that already exists. Local development and the repo-root `go.work` cover the gap; CI builds every module with `GOWORK=off` so each is verified exactly as a consumer resolves it.

## [0.1.0] - unreleased

First release. Extracted from the four vendored `internal/flags` copies that
`otel-mongo`, `otel-mongo/v2`, `otel-nats` and `otel-gorilla-ws` each carried.

### Why this module exists

A single OpenFeature provider per binary. Four `internal/` packages in four
modules share no state, so two of them can observe "no provider installed"
concurrently and both register one — the SDK replaces the loser and shuts it
down, leaving one live provider *eventually* but two briefly, plus a duplicated
relay fetch. Go resolves one module path to one version per build, so one shared
module means one package instance, one install mutex, and exactly one provider.

Deleted along the way: the byte-identical vendoring rule, its "maintained by code
review, not by a check" caveat, the drift table, and three redundant copies of
`flags_test.go`.

### Added

- **The precedence ladder.** Every switch resolves `relay > env > option > default`, first source with an opinion winning. It is one `client.Boolean(ctx, key, local, evalCtx)` call: `local` is the already-resolved env-or-option-or-default value, and the SDK returns it on every path where the relay has no usable answer. Relay silence and relay failure are deliberately indistinguishable — both mean "the next rung down decides".
- `Lookup(name) (value, set bool, err error)` — the environment read, as a strict tri-state. Unset means this source has no opinion. `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off` (trimmed, case-insensitive) decide. **Everything else, including the empty string, is an error** wrapping `ErrInvalidFlagValue` and naming the variable and the value.
- `ErrInvalidFlagValue` — one sentinel for every module, matchable with `errors.Is`. Possible only because this module is published rather than `internal/`.
- `MasterLocal()` / `MasterEnabled(local)` — the process-wide master switch, `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and the relay key `otel-instrumentation-go-tracing`. It defaults to **enabled**, which makes it a veto rather than an enabler: setting it truthy changes nothing, and setting it falsy stops every module in the process, including connections whose Go code passed an option. It takes no option parameter, and must not: a per-connection value cannot express a process-wide switch.
- `RelayPossible()` — endpoint configured, or a provider bound to `FlagDomain`. When false the relay is structurally incapable of returning anything but the value passed to it, so callers resolve statically, allocate no instrumented implementation unless the local answer is on, and never touch the OpenFeature SDK. **Resolve it once per construction; do not memoize it process-wide.** A provider the application installed in the **default** slot for its own feature flags does not count: it does not make this true, it never has an instrumentation key evaluated against it, and it does not stand the auto-install down.
- `InstallProvider(p) error` — the recommended way for an application or embedding SDK to install its own provider. Binds `p` to `FlagDomain` with `SetNamedProviderAndWait`, so there is no startup window, and records the install so detection is exact rather than heuristic. Writes no other OpenFeature state. Raw `SetNamedProviderAndWait(FlagDomain, p)` remains supported and is still detected.
- `Resolver` / `NewResolver` / `WithFlagKeys` / `Value(i, local)` — per-module flag resolution. Caches nothing: no snapshot, no TTL, no clock, no refresh. An out-of-range index returns `false` rather than panicking.
- Environment-driven provider auto-install. Setting `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` (plus `_API_KEY` and `_POLL_INTERVAL`) gives an application relay control with no Go code and no import. It fires only when the application has bound no provider to `otel-instrumentation-go`, registers a **named** provider on that domain, and hardcodes `DataCollectorDisabled: true` and in-process evaluation so the zero-code path cannot be misconfigured into a stall. A malformed poll interval warns, falls back to `60s` and still installs.
- Polling jitter on the auto-installed provider. The interval from `_POLL_INTERVAL` sets the centre of the polling period, not an exact one: it is deviated by at most ±10%, drawn once per process and fixed for its lifetime, so processes started from one deployment do not keep polling the relay on a shared period. The arithmetic mirrors upstream's `newBackgroundUpdater` (`go-feature-flag`, `retriever/background_updater.go`) — uniform magnitude in `[0, 10%)`, sign from that draw's parity in nanoseconds — which is what the relay proxy's `enablePollingJitter` applies one hop further up, between the relay and the flag storage. An interval configured on a provider the application installed itself is untouched. **The provider's first fetch is deliberately not jittered**: it happens unconditionally inside `Init`, so a simultaneous fleet restart still reaches the relay together, and delaying it would lengthen the startup window in which every switch resolves to its local value — fail-safe for enabling, not for disabling.
- `Version()` and the `instrumentationVersion` constant, for the release-tag CI guard.

### Notes for callers

- **Install your own provider before constructing any wrapper.** `RelayPossible` is resolved at construction, so a wrapper built earlier resolves statically for the rest of its life.
- **A flag change is not immediate**, and a remote *disable* does not survive a restart. End-to-end latency is the provider's poll interval — 60 s by default, up to 66 s once jittered — and until the first successful fetch every switch resolves to its local value, in both directions. The relay is runtime control; durable state belongs in the environment variable. A not-ready provider is deliberately never read as `false`: that would apply to the master key too, whose local default is `true`, and would veto every relay-configured process on every restart.
- **Two or three evaluations per instrumented operation**, paid whatever the flag's value; only a process where no relay is possible skips the pipeline. That cost is the SDK's evaluation pipeline, not the flag lookup, so an in-memory provider does not make it cheaper. Order of magnitude on developer hardware: single-digit microseconds each — no benchmark ships with this module, so measure your own workload. Caching is a permitted future optimisation and fits inside `Resolver` without changing `Value`'s signature.
- This module never calls `SetProvider`, `SetEvaluationContext`, `AddHooks` or `Shutdown`. Nothing it does can change how the application's own feature flags resolve.
