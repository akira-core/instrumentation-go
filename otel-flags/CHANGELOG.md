# Changelog

All notable changes to the `otel-flags` module are documented here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). See `VERSIONING.md` at the repo root for the tagging scheme and the pre-1.0 semver policy.

> **Release ordering**: this module is tagged **before** the instrumentation modules that require it. A published `go.mod` cannot carry a `replace` directive — consumers ignore it — so `otel-mongo`, `otel-mongo/v2`, `otel-nats` and `otel-gorilla-ws` can only name a version that already exists. Local development and the repo-root `go.work` cover the gap; CI builds every module with `GOWORK=off` so each is verified exactly as a consumer resolves it.

## [0.2.0] - 2026-08-05

> Tagged before the consumers, as `VERSIONING.md` requires. Their `require` lines named `v0.1.0`
> until this tag existed, because a `require` on a version that has no tag fails module-graph
> loading for the entire workspace — `go.work` does not cover it, contrary to what the design
> document originally assumed — so the two-stage release is not merely tidy, it is the only order
> that builds.

Follows the August 2026 review (`docs/otel-flags-review-2026-08.md`). It fixes
the design that review exposed rather than the defects themselves; the reasoning
and the rejected alternatives are recorded in
`docs/otel-flags-error-handling-decisions.md`.

### BREAKING

- **`Value` takes a flag key, not an index**: `Value(key string, local bool) bool`.
  `WithFlagKeys`, `ResolverOption` and the per-resolver key list are gone, and
  `NewResolver()` takes no arguments. The index bought nothing — `client.Boolean`
  takes a key string, so `Value` converted it straight back — while coupling two
  modules' flags by position with nothing checking it: swapping the two lines in
  `otel-mongo`'s `WithFlagKeys` call compiled, passed the tests, and silently made
  the propagation flag control tracing. Out-of-range was the least dangerous
  member of that family; this deletes the family rather than deciding what
  out-of-range should return.
- **`InstallProvider` is now `SetNamedProvider`.** "Install" implies a one-time
  idempotent operation; the semantics are set-or-replace, which matters more now
  that sharing one provider between the application and this library is an
  explicitly supported story. Not `SetProvider`: that name is taken by
  `openfeature.SetProvider`, which writes the opposite slot.
- **An unreadable provider variable now fails construction.**
  `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` must be a positive Go duration and
  `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` a URL with a scheme and a host, both
  validated whether or not a relay is configured. The interval previously warned
  and fell back to `60s`. That was a carve-out keyed to how bad the consequence
  is, which has to be re-argued for every variable anyone adds; what decides now
  is whether the operator's intent can be read. Blank still means "not
  configured" for both — unlike a boolean, neither has a second reading. The API
  key is not validated: any string can be a legitimate key.
- **`ErrInvalidFlagValue`'s message is now "invalid configuration value"** and it
  covers all four variable shapes. One sentinel still serves every module.

### Added

- **`ValidateAndInstall() error`** — the process-level entry point every wrapper
  constructor calls. It validates this package's environment, reports every bad
  value at once, and performs the one-time provider install. It does **not** wait
  for the provider to initialise; an application that wants the startup window
  closed calls `SetNamedProvider`.
- **Evaluation diagnostics.** `Value` reads `BooleanValueDetails` and logs a flag
  key's error code when it *changes* — never per evaluation, so the steady state
  is silent. `FLAG_NOT_FOUND` and `PROVIDER_NOT_READY` mean the relay has no
  opinion and log at debug; `TARGETING_KEY_MISSING`, `TYPE_MISMATCH`,
  `PARSE_ERROR`, `INVALID_CONTEXT`, `PROVIDER_FATAL` and `GENERAL` log at warn; a
  code that clears logs the recovery at info. The two highest-severity findings of
  the review were both invisible because this code was populated the whole time
  and nothing read it. The returned value is unchanged — relay silence and relay
  failure stay indistinguishable to a caller, which is what makes the ladder
  total.
- **`SetNamedProvider` warns when wrappers already exist.** `RelayPossible` is a
  construction-time snapshot, so a wrapper built before the provider was bound
  can never consult the relay. Nothing can repair that from here, which is why the
  warning is the remedy. A raw `openfeature.SetNamedProviderAndWait` cannot be
  detected and remains an accepted blind spot.

### Changed

- The install no longer runs inside the first evaluation. It held `installMu` —
  the mutex `SetNamedProvider` holds across a blocking provider initialisation —
  so a first evaluation concurrent with an application's own install could park an
  instrumented operation for the length of that provider's HTTP timeout.
- The evaluation context moved from per-`Resolver` to package level. A resolver
  created before the install used to cache an empty one for the life of the
  process, so one module would carry the `serviceName` targeting attributes and
  another would not.

### Fixed

- **The startup window is no longer reported as a fault.** `Client.evaluate`
  short-circuits a domain in `NOT_READY` or `FATAL` state *before* it builds any
  resolution detail, so `BooleanValueDetails` returns an empty `ErrorCode` next to
  a sentinel error — and `recordEvaluation` folded every codeless error into
  `GENERAL`, which is a warn tier. The commonest state this module has, the window
  between a non-blocking install and the provider's first fetch, therefore warned
  in every relay-configured process. `codeFromError` reads the sentinel back, so
  `PROVIDER_NOT_READY` lands at debug where the design says it does. The
  `codeProvider` tests could not see this: they resolve *through* a provider, which
  is the one path the SDK populates the code on.
- **A failed auto-install no longer latches.** `installOnce` marked the install
  done even when building or registering the provider had failed, and neither a
  later constructor nor `watchProviderInit` — which is started only on success —
  would try again. One transient failure pinned the process to a dead relay for
  its whole life, with a single warn as the evidence. The latch now closes only on
  an install that reached a decision.
- **`SetNamedProvider` drops the auto-install's evaluation context** along with its
  provider. The `service.name`/`serviceName` attributes are confined to the
  auto-install path precisely so they can never override a context the application
  owns; leaving them published sent them to the application's own provider on every
  evaluation.
- **`RelayPossible` reads the endpoint through `endpointFromEnv`** instead of
  re-deriving "is it non-blank", so it cannot disagree with the validation
  `ValidateAndInstall` enforces — `relay:1031` is non-blank but unbuildable.

### Performance

- `recordEvaluation` loads the remembered code before swapping it. `sync.Map.Swap`
  boxes the code into an interface — a heap allocation and an atomic store on the
  hot path of every instrumented operation, two or three per operation — only to
  discover that nothing had changed.
- `providerBound` short-circuits on `explicitBind`/`autoInstalled`. Either latch
  means this module bound `FlagDomain` itself and the SDK offers no way to unbind a
  domain, so the three metadata reads — each taking the SDK's registry lock, on
  every evaluation — were answering a question already settled.

### Unchanged, deliberately

- **The startup window still resolves to the local value.** `PROVIDER_NOT_READY`
  keeps passing `local` as the evaluation default, so a relay *disable* does not
  survive a restart while a relay *enable* can never appear during the window.
  Durable state belongs in the environment variable; persisting the last-known-good
  verdict to disk is the only design that changes this, and it stays on the shelf.
- **No health API.** An application that wants to alarm on a dead relay reads
  `openfeature.NewClient(otelflags.FlagDomain).State()`. It must not gate startup
  on it — that is the availability dependency this module refuses to become.
- **The 250 ms evaluation timeout stays hardcoded.** It applies only to a provider
  the application installed; the auto-installed one evaluates in process and skips
  the deadline entirely.

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
- **Recovery from a relay that is unreachable at startup.** The asynchronous bind discards the SDK's init result, and the provider's in-process evaluator returns from `Init` before starting its ticker when the first fetch fails — so a relay that was briefly down during a rollout left a bound provider that polls nothing, recovers from nothing, and reports `ERROR` for the life of the process. A background watch now rebinds a freshly built provider whenever it reads that state, backing off from one second to the poll interval, standing down if anything else takes the domain over, and logging the first failure at warn and the recovery at info. The bind itself is unchanged: still asynchronous, still immediate, so the startup window keeps the semantics documented above.
- **A targeting key on every path**, `<hostname>-<pid>`. Without one, every relay rule using `percentage` or `progressiveRollout` — how a kill switch is canaried or ramped — failed with `TARGETING_KEY_MISSING`, which `Client.Boolean` turns into the local value, so the rollout appeared to do nothing on every process with no diagnostic anywhere. Per process rather than per service, so a percentage is not all-or-nothing across a fleet, and stable across a restart of the same container.
- **`serviceName` alongside `service.name`.** Both carry `OTEL_SERVICE_NAME` and both are still confined to the auto-install path, but only the dot-free spelling can be matched: a dot is a nested-path separator in both query languages the relay supports, so the documented rule `service.name eq "checkout-api"` matched no process at all.

### Fixed

- **`InstallProvider` now takes `installMu`.** It was the one entry point that bound `FlagDomain` without the lock the module has for exactly that purpose, so an application calling it concurrently with the first instrumented operation raced the environment auto-install: either its provider was silently replaced, or the SDK's `shutdownOld`/`initNew` pair interleaved such that the auto-installed provider started a polling goroutine with no reachable handle. It also latches `installDone`, so an application that installs its own provider is never followed by an auto-install, in either order.
- **Nothing is evaluated unless a provider is bound to `FlagDomain`.** `ForEvaluation` falls back to the DEFAULT provider for an unbound domain, so `Value` and `MasterEnabled` — both exported, and reachable without the `RelayPossible` short-circuit the four wrappers hand-roll — could resolve instrumentation keys against the application's own flag backend: a network call per instrumented operation if that backend evaluates remotely, and a wrong answer outright if it defines a key by the same name.
- **An evaluation against an application-installed provider is bounded at 250 ms.** `InstallProvider` accepts any provider, including one that evaluates over HTTP, and that sits on the hot path of every instrumented Mongo command and NATS publish. The auto-installed provider evaluates in process and skips the deadline. The caller's context is still deliberately not threaded through: cancelling a Mongo operation must not change what an instrumentation switch resolves to.
- **`providerBound` re-reads the default provider around the named read** and only trusts an answer that did not move. The two SDK calls take the same lock separately, so a concurrent `SetProvider` between them made an unbound domain look bound — `RelayPossible()` true with nothing bound, and the auto-install standing down over an endpoint the operator configured.
- `Value` no longer reads `r.evalCtx` as an operand of the same call expression that initialises it, an ordering Go does not guarantee.
- Documentation: the package doc named `installOnce`, which does not exist, and `installProviderFromEnv` recommended `SetProviderAndWait`, which has not stood the auto-install down since `providerBound` was narrowed to a `FlagDomain` binding.

### Notes for callers

- **Install your own provider before constructing any wrapper.** `RelayPossible` is resolved at construction, so a wrapper built earlier resolves statically for the rest of its life.
- **A flag change is not immediate**, and a remote *disable* does not survive a restart. End-to-end latency is the provider's poll interval — 60 s by default, up to 66 s once jittered — and until the first successful fetch every switch resolves to its local value, in both directions. The relay is runtime control; durable state belongs in the environment variable. A not-ready provider is deliberately never read as `false`: that would apply to the master key too, whose local default is `true`, and would veto every relay-configured process on every restart.
- **Two or three evaluations per instrumented operation**, paid whatever the flag's value; only a process where no relay is possible skips the pipeline. That cost is the SDK's evaluation pipeline, not the flag lookup, so an in-memory provider does not make it cheaper. Order of magnitude on developer hardware: single-digit microseconds each — no benchmark ships with this module, so measure your own workload. Caching is a permitted future optimisation and fits inside `Resolver` without changing `Value`'s signature.
- This module never calls `SetProvider`, `SetEvaluationContext`, `AddHooks` or `Shutdown`. Nothing it does can change how the application's own feature flags resolve.
