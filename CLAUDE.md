# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Four independent Go modules providing OpenTelemetry instrumentation for MongoDB, NATS/JetStream, and gorilla/websocket, plus two supporting modules (`otel-sampler`, `otel-testkit`). Each module has its own `go.mod`, versioning, and CI job — they are developed and tagged separately.

| Module dir | Import path suffix | What it wraps |
|---|---|---|
| `otel-mongo/` | `.../otel-mongo/otelmongo` | MongoDB Go driver v1 |
| `otel-mongo/v2/` | `.../otel-mongo/v2` (separate `go.mod`) | MongoDB Go driver v2 |
| `otel-nats/` | `.../otel-nats/otelnats` + `oteljetstream` | NATS core + JetStream |
| `otel-gorilla-ws/` | `.../otel-gorilla-ws` | gorilla/websocket |

Three supporting modules sit alongside them (no instrumentation, no spans of their own):

| Module dir | Import path suffix | Purpose | Released? |
|---|---|---|---|
| `otel-flags/` | `.../otel-flags` | The shared feature-switch layer every wrapper `require`s: the precedence ladder, the `Resolver`, the OpenFeature provider install and its single-provider guarantee | **Yes**, `otel-flags/vX.Y.Z`; start at `0.1.0`. Tagged **before** the four wrappers — see `VERSIONING.md` |
| `otel-sampler/` | `.../otel-sampler/otelsampler` | Consistent probability sampler (`ProbabilitySampler`, `WithSingleLinkSeed`, exported `Threshold`/`Sampled`) — applications install it as their `sdktrace.Sampler` | **Yes**, `otel-sampler/vX.Y.Z`; start at `0.1.1` (`v0.1.0` is superseded) |
| `otel-testkit/` | `.../otel-testkit/harness` | Black-box E2E harness: in-process OTLP sink, collector container, span assertions | No — untagged, test-only |

Each instrumentation module also has `examples/` and `tests/integration/` sub-modules with their own `go.mod`. Integration tests use **testcontainers-go** (require Docker/Podman running). (`otel-mongo/v2` has no separate `examples/` of its own — the single `otel-mongo/examples/` module imports and demos the v2 package.) `otel-testkit/examples/httpdirect` and `httpdirect-stdlib` are reference templates with their own `go.mod`; they are linted in CI and run by the `http-direct-e2e` job, not by `test-and-lint`.

## Common Commands

All commands must be run **inside the module directory** being changed.

```bash
# Build
go build ./...

# Test (race detector enabled)
go test -v -race ./...

# Single test
go test -v -race -run TestFunctionName ./...

# Lint (golangci-lint v2 required)
golangci-lint run ./...
```

**Mandatory after any `.go` change:** run all three (`go build`, `go test`, `golangci-lint`) before considering work complete. All three must pass with 0 issues.

```bash
# Integration tests (require Docker; run inside tests/integration/)
cd otel-mongo/tests/integration && go test -v -race ./...
```

## Lint Rules to Know

Config is in `.golangci.yml` (v2 syntax). Common failure modes:

- **`goimports`**: stdlib imports must be in their own group, separated from third-party by a blank line. Local prefix is `github.com/akira-core/instrumentation-go`.
- **`errcheck`**: every returned error must be handled (disabled in `_test.go`).
- **`govet`**: includes shadow, printf format checks.
- **`staticcheck`**: full suite enabled.

## Architecture

### Wrapper Pattern

All packages wrap the upstream client type and expose the same API surface with trace instrumentation added:

```go
// caller creates upstream client, wraps it:
wsConn := otelgorillaws.NewConn(rawWebsocketConn, opts...)
nc, _ := otelnats.Connect(url)
client, _ := otelmongo.Connect(ctx, mongoOpts...)
```

### TracerProvider & Propagator

Packages **never** initialize a TracerProvider. They fall back to `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` by default. Override per-connection via functional options:

```go
WithTracerProvider(tp)
WithPropagators(p)
```

Applications call `otelsetup.Init()` at startup to configure the global provider.

### Trace Propagation by Transport

| Transport | Carrier | Where context lives |
|---|---|---|
| MongoDB | Document field `_oteltrace` | `{ traceparent, tracestate }` injected on every write. **Never stripped on read** — once written it is visible to the application on every subsequent read, and cleanup is a `$unset` migration. Injection removes any existing `_oteltrace` before appending, so a read-modify-write yields exactly one |
| NATS/JetStream | Message headers | `traceparent`, `tracestate` headers via `HeaderCarrier` |
| WebSocket | JSON message body | `{"header":{"traceparent":...,"tracestate":...},"data":<payload>}` envelope on write; non-JSON payloads are JSON-string-encoded (not base64) into `data`; legacy flat top-level `traceparent`/`tracestate` fields still accepted as a read-only fallback |

### The feature-switch ladder (all modules)

Every switch resolves down four rungs, first source with an opinion winning:

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

Ordered by how late each source is decided — compile, construct, deploy, run — so each later stage
overrides the earlier ones. The **relay is authoritative in both directions**: it can disable a
running module and enable one the deployment left off. Safety comes from the defaults, not from a
restriction on the relay.

| Switch | Relay key | Option | Env | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| per-module tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` |
| Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

`tracing = master && moduleTracing`; `propagation = tracing && mongoPropagation`.

**The master is a veto, not an enabler.** Default `true` means "no objection"; setting it `true`
changes nothing, and only `false` has an effect. It accepts **no option** — a per-connection value
cannot spell a process-wide switch, and keeping it option-free is what guarantees one env var (or one
relay flag) stops everything.

**The option sits BELOW its env var.** This diverges from released `0.7.0`, deliberately, for three
reasons in order of weight: it gives the operator a per-module setting application code cannot
override; it stops `WithTracePropagationEnabled(true)` bypassing `OTEL_MONGO_PROPAGATION_ENABLED=false`
and writing permanent `_oteltrace` into the operator's documents; and it keeps the ladder monotonic.
Cost: a process that sets the module variable cannot differentiate two connections through options.
With the variable unset — the ordinary state under a default of `false` — the option decides.

**Environment values are a strict tri-state.** Unset ⇒ no opinion, fall through. `1`/`true`/`yes`/`on`
or `0`/`false`/`no`/`off` ⇒ decides. **Anything else, including the empty string, is a construction
error** wrapping `otelflags.ErrInvalidFlagValue`. There is no safe direction to guess in when one tier
defaults `true` and the rest default `false`. All of a constructor's bad values are joined into one
error. `flags.EnvEnabled`/`EnvSet` are gone; `otelflags.Lookup` replaces both.

`ContextFromDocument` / `ContextFromRawDocument` are **not** in this model — they carry no gate at
all; see below.

### Per-connection options (all four modules)

`WithTracingEnabled(v bool)` — `otelnats.ConnectWithOptions`/`ConnectTLSWithOptions`/
`ConnectWithCredentialsWithOptions`, `otelmongo.ConnectWithOptions` (v1 and v2),
`otelgorillaws.NewConn`/`Dial`/`Upgrader.Upgrade`. `otel-mongo` also has
`WithTracePropagationEnabled(v bool)`.

It supplies the **module** tier for that connection only: below its env var, above the hardcoded
default, and far below the relay. It does not pin anything — a connection carrying it resolves the
master switch and the relay on every operation. Supplying it alongside its env var is legal (the var
wins); the mutual-exclusion rule and both `Err*ConfigConflict` sentinels are **deleted**. An
unreadable env value is still an error even when an option was supplied.

Everything built from an option-configured wrapper inherits its `gateState` (e.g. `oteljetstream`
wrappers from an `otelnats.Conn`, `Database`/`Collection` from an `otelmongo.Client`).

All four option appliers skip nil `Option` values (a literal `nil` variadic arg once made
`ConnectTLS`/`ConnectWithCredentials` panic on every successful connection — pinned by
`TestNewConnConfig_SkipsNilOptions` and siblings).

Three module-specific pitfalls to know if you touch this:
- **otel-mongo**: `propagationGiven(tracing bool)` takes the caller's already-resolved tracing state as
  a parameter — it does **not** recompute it internally. Reintroducing an internal recompute would let
  one operation emit a CLIENT span on one verdict and decide `_oteltrace` on another (R5).
  Package-level `ContextFromDocument`/`ContextFromRawDocument` are ungated entirely.
- **otel-mongo/v2 only**: driver v2's `options.MergeClientOptions` returns the **caller's own**
  `*ClientOptions` when exactly one is passed (v1 always builds a fresh struct), so `ConnectWithOptions`
  merges through a fresh `options.Client()` base before `SetMonitor` — otherwise it would mutate
  caller-owned options in place. Pinned by `TestConnectWithOptions_DoesNotMutateCallerOptions` (both
  modules, for parity).
- **otel-gorilla-ws**: `Conn.capable` is `gate.tracedPossible()` (whether any OTel SDK path could ever
  run); `Conn.enveloped` is the otel-ws negotiation *outcome*; `Conn.tracingEnabled` is the clamped
  **write** decision (`enveloped && capable`). Three similarly-shaped booleans, three different
  meanings. `Dial` and `Upgrader.Upgrade` resolve the effective tracing value **once, before the
  handshake** — relay included — and never offer/confirm otel-ws when it is off. `NewConn` derives
  `enveloped` from the raw conn's negotiated subprotocol (`isOTelWireProtocol`), never forces it. The
  read path keys on the **unclamped** `enveloped`, which is what stops a feature-off wrapper handing
  raw `{"header":…,"data":…}` bytes to the application.

### The shared `otel-flags` module

The four vendored `internal/flags` copies are **gone**. Their contents live in one published module,
`github.com/akira-core/instrumentation-go/otel-flags`, which all four wrappers `require`.

**Why a module and not four copies.** The forcing requirement is a single OpenFeature provider per
binary. Four `internal/` packages share no state, so two could observe "no provider installed"
concurrently and both register one. Go resolves one module path to one version per build, so there is
one package instance, one install mutex, and exactly one provider. Deleted with the copies: the
byte-identical rule, its "maintained by review, not by a check" caveat, the drift table, and three
redundant `flags_test.go`.

Exported surface:

- `Lookup(name) (value, set bool, err error)` — the tri-state env read. Unset / recognised / error.
- `ErrInvalidFlagValue` — one sentinel for every module, matchable with `errors.Is`. Possible only
  because this module is published rather than `internal/`.
- `ResolveLocal(option *bool, envName string, def bool) (bool, error)` — the three rungs below the
  relay, applied in ladder order (default, then option, then env **last**, because env outranks the
  option). Returns the `Lookup` error even when an option was supplied.
- `MasterLocal() (bool, error)` / `MasterEnabled(local bool) bool` — the master switch. No option
  parameter, by design.
- `RelayPossible() bool` — endpoint set, or a provider bound to `FlagDomain`. **Resolve once per
  construction; never memoize process-wide** (a `sync.Once` would freeze the answer at whichever
  wrapper was built first, which in a test binary makes every relay test unreachable). A provider the
  application installed as the **default** does not count — see below.
- `InstallProvider(p) error` — the recommended way for an application to install its own provider:
  `SetNamedProviderAndWait(FlagDomain, p)` plus a record that this process did so. Waiting means no
  startup window. Raw `SetNamedProviderAndWait` stays supported.
- `Resolver` / `NewResolver` / `WithFlagKeys` / `Value(i int, local bool) bool` — per-module
  resolution. Caches nothing. Out-of-range index returns `false` rather than panicking.
- `EnvGlobalTracing`, `FlagKeyGlobalTracing`, `FlagDomain`, `EnvFlagsEndpoint`, `EnvFlagsAPIKey`,
  `EnvFlagsPollInterval`, `EnvServiceName`.

`FlagDomain` (`otel-instrumentation-go`) is the single domain **all** modules resolve through. One per
module is impossible: `InProcess.Init` is not idempotent, so one provider instance under N domains
leaks N−1 unstoppable pollers.

**The module-vocabulary rule survives.** `otel-flags` names only process-scoped things. Module flag
keys, module env var names and module defaults stay in each module's own `env_flags.go` and reach the
shared module through `WithFlagKeys` and `Value`'s `local` parameter. Adding an instrumentation module
must not require a change to `otel-flags`.

`Gate`/`NewGate`/`ResetForTest` were removed in 0.8.0 — a process-lifetime cache is incompatible
with runtime flag changes. So were the module-level reset hooks: with nothing cached there is nothing
to re-arm, and tests drive the real path by rebinding the provider.

**Release ordering.** `otel-flags` is tagged **first**; the four wrappers then `require` a published
version with **no `replace`** (consumers ignore a replace in a dependency's `go.mod`). A repo-root
`go.work` covers local development; CI sets `GOWORK=off` on every job so each module is verified as a
consumer resolves it. The release guard fails any module tag whose `go.mod` still carries an in-repo
`replace`. See `VERSIONING.md`.

### Dynamic feature flags (0.8.0+)

Tracing and Mongo propagation are resolved at **runtime** through [OpenFeature](https://openfeature.dev),
so an operator can flip them via a GO Feature Flag relay proxy without restarting the application —
**in either direction**.

| OpenFeature key | Paired env var | Default | Modules |
|---|---|---|---|
| `otel-instrumentation-go-tracing` | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` | all (veto) |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `false` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `false` | `otel-gorilla-ws` |

The whole ladder is **one call**: `client.Boolean(ctx, key, local, evalCtx)`, where `local` is the
option-or-env-or-default value fixed at construction. `Client.Boolean` returns that default on every
path where the relay has no usable answer — no provider, not ready, key absent, evaluation error, type
mismatch — so relay silence and relay failure are deliberately indistinguishable, and both mean "the
next rung down decides". Do **not** use `BooleanValueDetails`: the distinction has no reader.

Three further **process-scoped** variables configure the relay connection rather than any module:
`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` (unset ⇒ no provider is installed **and** `RelayPossible()`
is false), `_API_KEY`, and `_POLL_INTERVAL` (Go duration strings only, default `60s`, and the centre
of the period rather than an exact one — see `jitterInterval` below).
`OTEL_SERVICE_NAME` supplies `serviceName` and `service.name` targeting attributes, on the auto-install
path only — relay rules must key on the dot-free spelling, because a dot is a nested-path separator in
both query languages the relay supports. A targeting key of `<hostname>-<pid>` is supplied on every
path, without which every `percentage`/`progressiveRollout` rule fails with `TARGETING_KEY_MISSING`
and silently resolves to the local value.

**Flag-change latency is the provider's poll interval — 60 s by default, up to 66 s — in both
directions.** The resolver adds nothing beyond `jitterInterval` (`otel-flags/flags.go`), which
deviates the auto-installed provider's interval by at most ±10%, drawn once per process, so a fleet
does not poll the relay on a shared period. It mirrors upstream's `newBackgroundUpdater`
(`retriever/background_updater.go`) — uniform magnitude in `[0, 10%)`, sign from that draw's parity
in nanoseconds — which is what the relay proxy's `enablePollingJitter` runs one hop further up,
between the relay and the flag storage. An interval the application configures on its own provider is
untouched.

**The provider's first fetch is deliberately not jittered.** `InProcess.Init` fetches the whole
configuration once, unconditionally, then starts a plain ticker; later polls are ETag-conditional and
answered 304. So the expensive request is at process start and stays correlated across a fleet
restart. Delaying the install to scatter it would lengthen the startup window in which every switch
resolves to its local value — fail-safe for enabling, **not** for disabling — which is exactly the
case an operator reaches for the relay to stop, `_oteltrace` writes included.

**Never touch the DEFAULT provider from library code** — never `openfeature.SetProvider`,
`SetEvaluationContext`, `AddHooks` or `Shutdown`, the same rule as never initializing a
`TracerProvider`. The one deliberate exception is the environment-driven auto-install: when
`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is set **and**
`openfeature.NamedProviderMetadata(FlagDomain).Name == "NoopProvider"`, `otel-flags` builds a GO
Feature Flag provider and registers it with the non-blocking `SetNamedProvider(FlagDomain, p)`.
`DataCollectorDisabled: true` and in-process evaluation are hardcoded there. An application that
installs its own provider first keeps it.

Applications may install a provider themselves, which also closes the non-blocking startup window:

```go
provider, _ := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint:              "http://relay:1031",
    DataCollectorDisabled: true, // required — see feature-flags.md
})
// Blocking install; on error log and continue rather than failing startup.
_ = otelflags.InstallProvider(provider)
```

**"A provider is bound" means bound to `FlagDomain`, not to the default slot.** `NamedProviderMetadata`
falls back to the default provider's metadata when the domain is unbound, so reading it alone made an
application's own business provider look like a relay: wrappers allocated the instrumented
implementation and evaluated instrumentation keys against it, and — worse — the auto-install stood
down, leaving an operator's configured endpoint silently inert. `providerBound()` therefore rejects an
answer that merely echoes the default, and `InstallProvider` records the fact exactly. Do **not** reach
for `GetNamedProviders()`: it returns the live map without copying, so reading it races
`SetNamedProvider` and can crash the process.

**They must do it BEFORE constructing any wrapper.** `RelayPossible()` is resolved at construction, so
a wrapper built earlier resolves statically for its whole life and never consults the relay. This is a
documented ordering requirement, and the same rule binds the tests.

The startup window is **fail-safe for enabling**: until the provider's first fetch, every switch
resolves to its local value, so the window can delay a relay-driven enable but can never introduce one
— and for `otel-mongo` can never write an `_oteltrace` field the deployment did not configure.

**It is deliberately not fail-safe for disabling: a relay `false` does not survive a restart.** With
the env var enabled and the relay disabling, a restarted process traces again until its first fetch,
and indefinitely while the relay is down. Reading not-ready as `false` is refused — it applies per key
and the master's local default is `true`, so every restart of every relay-configured process would be
fully vetoed, turning the control plane into an availability dependency. Relay is runtime control;
durable state belongs in the environment variable. Pinned by
`TestValue_NotReadyProviderLeavesLocalInCharge`.

**Tests install their in-memory provider with `openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, …)`,
never `SetProviderAndWait`** — a named provider outranks the default for these clients. There is no
reset hook: the resolver caches nothing, so a rebound provider is observed on the very next operation.
Tests **must not call `t.Parallel`** (the provider and the environment are process-global), **must
install the provider before constructing the wrapper**, and **must leave
`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` unset** except in the auto-install tests. A **module** env var
is read once at construction, so a test that changes one must reconstruct the wrapper.

### Consumer Context

Subscribers always receive a `Msg` (NATS/JetStream; call `.Context()` for the extracted trace) or a new `context.Context` return value (WebSocket `ReadMessage`) carrying the extracted remote trace. This context must be threaded into downstream calls to continue the trace chain.

### Span Links vs. Parent-Child

Async consumers (NATS subscribers, MongoDB change stream readers, WebSocket readers) use **span links** rather than parent-child relationships to connect to the producer span. This is intentional — preserves causality without implying synchronous nesting.

### Disabled-mode invariant (0.3.0+)

When any feature flag returns false, **no OTel SDK code path may run** inside a **gated** path: no `tracer.Start` on a real tracer, no `sdktrace.NewTracerProvider`, no `otlptracegrpc.New` / `otlptracehttp.New`, no `[]attribute.KeyValue` build, and no trace-context **injection**.

The clause is scoped to code the flags govern. Two things are deliberately outside it: the explicitly-invoked read-only helpers `ContextFromDocument` / `ContextFromRawDocument`, which emit nothing and are called only when the caller wants extraction; and `otelgorillaws`'s envelope unwrap on a negotiated connection, which is `json.Unmarshal` with the headers discarded. Neither touches the OTel **SDK** — `propagation` is API — so the `internal/direct` package boundary and the CI grep are unaffected. Two enforcement patterns coexist:

**1. Strategy split (preferred — otel-mongo Collection / Cursor / SingleResult / ChangeStream).** The facade type holds an `impl` interface satisfied by either `internal/direct.X` (passthrough) or `internal/traced.X` (instrumented). Construction picks the impl once; per-method runtime gates disappear. `internal/direct/*.go` imports no `go.opentelemetry.io/otel/sdk/*` and no `otel/exporters/*` — the disabled path is **compiler-enforced** by package boundary.

**2. Per-call gate (otel-gorilla-ws).** Every public method starts with `if !c.featureEnabled() { /* delegate to native */ }`, which re-resolves the relay verdict. Reviewer-enforced: `Conn` embeds `*websocket.Conn` (which is what gives callers the whole gorilla API for free) and only `WriteMessage`/`ReadMessage` need wrapping, so splitting two methods would cost an interface over roughly thirty.

`otel-nats` — both `otelnats.Conn` and the `oteljetstream` wrappers — is a **strategy split**, not a per-call gate: `directConn`/`tracedConn` and `directJSImpl`/`tracedJSImpl` are selected per operation by `impl()`. It keeps both implementations in one package, so its equivalent of the boundary is reviewer-enforced rather than compiler-enforced.

**Which switch selects the implementation.** `relayPossible || (masterLocal && tracingLocal)`, resolved once at construction and carried on `gateState`. It cannot key on the environment alone: the relay can **enable**, so a wrapper whose environment says off must still be able to start tracing later, and construction happens once. `relayPossible` — an endpoint is configured, or a provider is already bound — is the sound static approximation: with no relay possible, `Client.Boolean` can only ever return the value passed to it, so the static answer is final. Every configuration that took the zero-cost passthrough path before this change still takes it, including skipping `shared.NewCommandMonitor`, which otherwise runs on every MongoDB command.

Independent of pattern:
- For otel-mongo, `Connect` substitutes `noop.NewTracerProvider()` when disabled so any stray `tracer.Start` is inert.

**Adding a new public method to a strategy-split wrapper** (otel-mongo Collection/Cursor/SingleResult/ChangeStream) — touch THREE files in lockstep per module, mirror in v1↔v2 sibling:
1. Add signature to the facade's `collectionImpl` interface (in `collection.go`) or extend `shared.CursorImpl` / `shared.SingleResultImpl` / `shared.ChangeStreamImpl` in `internal/shared/impls.go`.
2. Implement passthrough in `internal/direct/<file>.go` — no `otel/sdk` or `otel/exporters` imports.
3. Implement instrumented version in `internal/traced/<file>.go`.

Compile-time `var _ shared.CursorImpl = (*traced.Cursor)(nil)` assertions in facade `cursor.go` / `results.go` (and `var _ collectionImpl = (*traced.Collection)(nil)` in `collection.go`) fail the build if any impl misses a method.

**Adding a new public method to a per-call-gate wrapper** (otel-nats `oteljetstream`, otel-gorilla-ws) — fast-path gate is the first statement, calling the **method** not a cached field: `if !c.on() { return c.js.Publish(...) }`. Examples to copy: `tracedJSImpl.Publish`, `tracedConsumer.Fetch`, `otelgorillaws.Conn.WriteMessage`.

**Methods that RETURN a wrapper must not be gated** (`tracedJSImpl.Consumer`, `tracedStream.CreateConsumer`, …). They always hand back the instrumented wrapper, which gates its own methods. Returning a passthrough wrapper because the flag happened to be off would pin that consumer or stream forever — exactly what per-call resolution exists to avoid.

### Strategy-split layout (otel-mongo)

Per module (`otelmongo/` v1 and `v2/`), the facade package contains thin wrappers + the `collectionImpl` interface; impls live under `internal/`:

```
otelmongo/
├── collection.go cursor.go results.go database.go client.go    # facade
├── tracing.go env_flags.go version.go                          # facade helpers
└── internal/
    ├── shared/    # impls.go (CursorImpl/SingleResultImpl/ChangeStreamImpl interfaces),
    │              # semconv.go, tracing.go, bulkwrite.go, monitor.go, hostport.go — helpers used by both paths
    ├── direct/    # collection.go cursor.go singleresult.go changestream.go
    │              # NO go.opentelemetry.io/otel/sdk/* or otel/exporters/* imports
    └── traced/    # collection.go cursor.go singleresult.go changestream.go
                   # full OTel SDK access
```

Key rules:
- `internal/shared/impls.go` declares the polymorphic interfaces (`CursorImpl`, `SingleResultImpl`, `ChangeStreamImpl`) satisfied by both `internal/direct.X` and `internal/traced.X`.
- **Dual implementation (0.8.0+).** Facade `Collection`, `Cursor` and `ChangeStream` hold **both** a `direct` and a `traced` field plus a `tracing func() bool`, and select per operation via `impl()`. `traced` is nil when `gate.tracedPossible()` was false at construction — no relay possible and the local answer off — so no OTel path is reachable. `SingleResult` is the **documented exception**: `traced.SingleResult` holds the live `FindOne` span (ended once on the first `Decode`/`TraceContext`/`Raw`), so a passthrough `FindOne` leaves nothing to wrap and a mid-flight flip would strand an unended span — its implementation stays fixed by the path that ran the `FindOne`.
- `traced.Collection.PropagationEnabled` is a `func() bool` (not a `bool`), read per call via the nil-safe `t.propagationOn()`. Same for `traced.Cursor` / `traced.ChangeStream`'s `propagationEnabled`.
- Facade `collectionImpl` interface returns raw driver types (`*mongo.Cursor`, `*mongo.SingleResult`, `*mongo.ChangeStream`) + `shared.XImpl` — the impl packages never need to import the facade, preventing any facade ↔ internal cycle. Facade methods wrap raw types into facade wrappers (`&Cursor{Cursor: raw, impl: cImpl}`).
- `internal/traced.Collection` has **exported fields** (`Coll`, `Tracer`, `Propagator`, `PropagationEnabled`, `ServerAddr`, `ServerPort`) so facade-package tests can build literals and call them directly.
- v1/v2 parity extends to `internal/{direct,traced,shared}/`. The helpers in `internal/shared/{bulkwrite.go,semconv.go,tracing.go,impls.go,monitor.go,hostport.go}` are intentionally duplicated across modules (separate `internal/` trees cannot share). A drift-check CI step to catch divergence between the two copies is planned but not yet implemented.
- `internal/shared/monitor.go` builds the `event.CommandMonitor` (`shared.NewCommandMonitor`) that captures the real per-command server address from `CommandStartedEvent.ConnectionID` into a context-scoped holder (`shared.WithAddrCapture`/`*shared.AddrCapture.Resolve`), chaining any caller-supplied monitor rather than replacing it. `client.go`'s `ConnectWithOptions` registers it (tracing-enabled branch only, via `options.MergeClientOptions`); `internal/traced/collection.go` call sites read it back after the raw driver call to overwrite `server.address`/`server.port` on the span, falling back to the static URI-parsed value when nothing was captured. `internal/shared/hostport.go` (`SplitHostPort`) is the shared IPv6-aware host:port parser used by both `monitor.go` and `client.go`'s `parseServerFromURI`.

### The ungated document helpers (otel-mongo)

`ContextFromDocument` / `ContextFromRawDocument` (`tracing.go`, both v1 and v2) carry **no feature-flag gate at all** — not the master switch, not the module env vars, not the options, not the relay. They start no span, build no attributes, initialise no part of the OTel SDK, write nothing, and perform no OpenFeature evaluation: they read `_oteltrace` out of a value the caller already holds and return what it encodes, and you only call them when you want extraction.

`Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace` look similar and **are** gated, because each starts and ends a real `mongo.cursor.decode` span. So **turning a module off stops those but does not stop extraction here** — this pair is the supported way to keep trace linking while the library is silenced, and `feature-flags.md` says so in those words. **BREAKING in 0.8.0:** a fully-disabled process now gets a valid span context from them where it previously got nothing.

Consequences worth knowing: they cost no relay evaluation, so a per-document change-stream loop pays none of the 2–3 evaluations an instrumented operation does; and there is nothing for them to misread when a deployment configures tracing through `WithTracingEnabled` rather than the environment variable.

**What protects the write side now.** `_oteltrace` is not observability-only: roughly 90 bytes of BSON appended by `InsertOne`, `InsertMany`, `UpdateOne`/`UpdateMany`, `UpdateByID`, `ReplaceOne` and `BulkWrite`, never stripped on read, undone only by a `$unset` migration, and a hard write failure against `$jsonSchema` + `additionalProperties: false`. Under the superseded revoke-only model the relay could not start it. Now it can, and four things bound that instead: the master veto; `OTEL_MONGO_PROPAGATION_ENABLED=false`, which application code cannot override (the reason the option sits below the env var); the tier's hardcoded default of `false`, so absence in every source never enables it; and `relayPossible`, which keeps a no-relay process out of reach entirely.

### `oteljetstream.MessageBatch.Stop()`

`MessageBatch` interface (`oteljetstream/consumer.go`) includes a `Stop()` method (added 0.3.0; **breaking** for custom implementations). Callers that drain `Messages()` to channel close need not call it; callers that `break` / `return` early **must** `defer batch.Stop()` to release the internal goroutine (0.7.0+: `Stop()` no longer ends an in-flight span, since receive spans now end at handover — see below). The disabled-tracing path uses `directMessageBatch` (no spans, no attributes, but still 1 goroutine for `jetstream.Msg → Msg` type adaptation). Both the direct and traced forwarding goroutines (0.7.0+) select on the stop signal while waiting to **receive** from the native batch as well as while waiting to **send** to the wrapper channel, so `Stop()` is prompt regardless of which side the goroutine is parked on — before 0.7.0 it was only observed on the send side.

### Receive-span lifecycle: end at handover (0.7.0+)

Across all three JetStream consume paths — single-shot `Consumer.Next`, `MessagesContext.Next`, and batch (`Fetch`/`FetchBytes`/`FetchNoWait`) — the receive span is already ended by the time the caller observes the message, not when the next message arrives. In the batch forwarder the span ends **before** the channel send, deliberately: an unbuffered-channel rendezvous lets the receiver run concurrently with the sender, so ending after the send would race the receiver's `IsRecording()` check (the spec's ended-at-delivery contract; trade-off: `Stop()` winning the send-select can leave one emitted span for a never-delivered message). This replaced a `lastSpan`-deferred-end pattern in the batch forwarder and in `tracedMessagesContext` (which also removed a now-unnecessary mutex — with no cross-call span state, there's nothing for a concurrent `Stop`/`Drain` to race against). Span durations for these paths measure receive-to-handover only; callers wanting to measure processing time use their own child spans from the returned context.

`Consumer.Next` also gained live ctx-cancellation support in the same release, via `jetstream.FetchContext(ctx)` rather than only converting a ctx deadline to `FetchMaxWait`. Three rules encoded in `applyCtxToFetchOpts` (`consumer_direct.go`, shared by traced):
- Guard on `ctx.Done() == nil` (not `ctx == nil`) before wiring — `context.Background()` is non-nil but inert, and is a common companion to an explicit `FetchMaxWait`.
- The wrapper's `FetchContext` is appended **after** caller opts: jetstream applies opts in order and `FetchContext` overwrites the request ctx, so appending last keeps the method-parameter ctx authoritative over a caller-supplied `FetchContext(otherCtx)`.
- A **cancelable** ctx + caller `FetchMaxWait` surfaces jetstream's native `ErrInvalidOption` (upstream mutual exclusion; 0.6.0 silently ignored cancellation in that combination — documented as BREAKING in the 0.7.0 CHANGELOG, pinned by `TestNextCancelableCtxWithFetchMaxWaitErrors`).

## Versioning

Each module is tagged independently as `<module>/v<x.y.z>` — **except `otel-mongo/v2`**, whose module path ends in the `/v2` major-version suffix and is therefore tagged `otel-mongo/v2.x.y` (Go strips the suffix from the tag prefix and requires version major 2; `v2.MINOR.PATCH` tracks the siblings' `0.MINOR.PATCH`; the historical `otel-mongo/v2/v0.x.y` tags were never `go get`-resolvable and that shape is now rejected by the release guard). See `VERSIONING.md` at the repo root for the full policy (pre-1.0 breaking→minor rule, where release notes live, per-module `CHANGELOG.md`). Version strings live in:

- `otel-nats/otelnats/conn.go` — `instrumentationVersion` const
- `otel-mongo/otelmongo/version.go` — `instrumentationVersion` const
- `otel-mongo/v2/version.go` — `instrumentationVersion` const
- `otel-gorilla-ws/version.go` — return literal from `Version()`
- `otel-sampler/otelsampler/version.go` — `instrumentationVersion` const (not an instrumentation scope — this module emits no spans; the constant exists for the release guard and for callers recording which sampler build they run). `otel-sampler/v0.1.0` is published but points at a pre-rebase commit, so it is superseded and unusable — releases start at `0.1.1`.
- `otel-flags/version.go` — `instrumentationVersion` const. Same reasoning: no spans, constant exists for the guard.
- `otel-testkit` is deliberately untagged.

**`otel-flags` releases FIRST.** It is the one module here with an ordering constraint: the four wrappers `require` it, and a published `go.mod` cannot carry a `replace` (consumers ignore it), so a wrapper can only name a version that already exists. Every release touching the flag layer is two stages — tag `otel-flags/vX.Y.Z`, then bump the four `require` lines, **remove the development-time `replace`**, `GOWORK=off go mod tidy`, and tag the wrappers. The release guard fails any module tag whose `go.mod` still carries an in-repo `replace`, because such a tag is unbuildable for anyone outside this repo and nothing else would notice.

A release-tag CI guard (`.github/workflows/release-guard.yml`) fails the push if a tag's version doesn't match the corresponding constant above — see the **CI** section.

Bump on any code change to a module before pushing release tag. Module pre-1.0 (`0.x.y`): minor bump allowed for breaking changes. (`otel-mongo/v2`'s constant is `2.x.y` — same minor/patch discipline, fixed major.)

## Module-Specific Notes

### otel-mongo

- `_oteltrace` field adds ~100–120 bytes per document.
- Use `Cursor.DecodeAndTrace(ctx, v)` (not `Decode`) when reading in a change-stream context — it extracts the trace from the document and links spans correctly.
- `ContextFromDocument(ctx, doc)` extracts trace from an already-decoded document map; it respects the same propagation env gates as the Collection wrapper (not a bypass).
- **Strategy-split layout:** Collection / Cursor / SingleResult / ChangeStream all live in `internal/{direct,traced}/` (see *Strategy-split layout (otel-mongo)* above). Client and Database are **gate carriers**: neither creates a span, so there is nothing to split and nothing to gate — they resolve the static tiers once into a shared `gateState` and hand it down.
- **v1 and v2 parity rule:** `otelmongo/` (v1) and `v2/` are parallel implementations. All logic changes — new flags, new fields, new inject/extract paths, new strategy methods — must be applied to **both** sub-packages identically, including their `internal/{direct,traced,shared}/` trees. Run lint and tests for both when either is touched.

### otel-nats

- `otelnats` wraps core NATS; `oteljetstream` wraps JetStream. Both live in the same `go.mod` (`otel-nats/`).
- `Conn.Subscribe` handler signature is `MsgHandler` (`func(Msg)`) — not the native `func(*nats.Msg)`.
- JetStream `Consumer.Messages()` returns an iterator; call `.Context()` on each item for the trace context.
- `WithTraceDestination(subject)` enables NATS 2.11+ infrastructure trace events.
- JetStream consumer spans carry the durable/consumer name under the semconv v1.39.0 key `messaging.consumer.group.name` (0.7.0+; was the non-semconv literal `messaging.consumer.name` before).
- `HeaderCarrier` (`otelnats/propagation.go`) implements `propagation.ValuesGetter` and falls back to the MIME-canonical header form on read (0.7.0+) — `nats.Header` is case-sensitive, unlike `http.Header`, so a canonicalizing producer's messages (including ones already sitting in a durable stream) still extract. The fallback triggers on key **absence**, not value emptiness (a verbatim key with an empty value wins over a canonical entry), identically in `Get` and `Values`.
- Core-NATS request/reply spans carry `messaging.message.conversation_id` (0.7.0+) = the reply inbox subject, set on the requester's send span, reply-receive span, and responder's process span. On the send span it's a late `SetAttributes` in `recordReply` (the inbox isn't observable until the reply arrives) and is omitted entirely on timeout/error. `oteljetstream` spans never carry it — a JetStream message's `Reply` field is the native `$JS.ACK.…` subject, not a conversation ID.

### otel-gorilla-ws

- `NewConn` wraps an already-dialed `*websocket.Conn`; the package-level `Dial` function dials and wraps in one step. `NewConn` only handles the envelope when the raw conn's negotiated subprotocol proves `otel-ws` — prefer `Dial`/`Upgrader.Upgrade` when you want trace propagation.
- The JSON envelope is an internal wire format — applications see the original payload from `ReadMessage`.
- Subprotocol negotiation scenarios (client/server × otel-ws-aware/unaware, including the empty-subprotocol edge case) are documented in `otel-ws.md` at the repo root — consult it when touching `Dial`'s or `Upgrader.Upgrade`'s negotiation logic.
- Negotiation is feature-gated (0.7.0+): `Dial` only offers, and `Upgrader.Upgrade` only confirms, otel-ws when the connection's effective tracing feature (env gates or `WithTracingEnabled`) is on — resolved **before** the handshake via `resolveConnOptions`/`effectiveFeatureEnabled` (`options.go`). See the per-connection-override pitfalls above for why (wire corruption otherwise).

## CI

`.github/workflows/ci.yml` runs on every push/PR to `main`, `master`, or `feat/*`, Go 1.25 on Ubuntu, with two jobs:

- `test-and-lint` — matrix over all seven modules (`otel-flags`, `otel-sampler`, `otel-testkit`, `otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`): `go build`, `go test -race`, `golangci-lint`. **Every job sets `GOWORK: off`** so each module resolves exactly as a consumer would, from its own `go.mod`; the repo-root `go.work` is for local development only, and using it in CI would let a missing or wrong `require` pass here and fail for everyone else. For `otel-mongo` and `otel-mongo/v2` only, an additional "Verify direct/ has no OTel SDK imports" step greps `internal/direct/` for `go.opentelemetry.io/otel` imports and fails the build if any are found — this is the CI-enforced half of the disabled-mode invariant described above (the strategy-split package boundary is the compiler-enforced half).
- `integration-test` — gated on `needs: test-and-lint`; matrix over `otel-nats/tests/integration`, `otel-mongo/tests/integration`, `otel-mongo/v2/tests/integration`, and `otel-gorilla-ws/tests/integration`, running `go test -v -race -timeout 300s` over `go list ./...` **minus `/sampling`** (testcontainers-based, requires Docker). The `./sampling` packages are excluded on purpose — they belong to the dedicated flag-matrix jobs below, and running them here too doubles the Docker cost and squeezes a 600s suite into a 300s budget. The same exclusion is mirrored in the Makefile's `test-integration`.
- `sampling-e2e` / `nats-sampling-e2e` — the feature-flag × `OTEL_TRACES_SAMPLER_ARG` matrices, the only place `./sampling` runs (600s budget).
- `http-direct-e2e` — lints and runs both `otel-testkit/examples/httpdirect` (consistent sampler) and `httpdirect-stdlib` (sampler-agnostic baseline).

`.github/workflows/release-guard.yml` (0.7.0+) runs only on pushed tags matching one of the six module shapes (`otel-mongo/v[0-9]*`, `otel-mongo/v2/v[0-9]*`, `otel-nats/v[0-9]*`, `otel-gorilla-ws/v[0-9]*`, `otel-sampler/v[0-9]*`, `otel-flags/v[0-9]*`) — see `VERSIONING.md`. It runs two checks: the tag's version must match that module's version constant (table above), and the module's `go.mod` must carry no `replace` pointing at another module in this repository. Routing details: `otel-mongo/v2.*` tags validate against `otel-mongo/v2/version.go` (the v2 module's Go-resolvable shape); the deprecated `otel-mongo/v2/v*` shape fails immediately with a pointer to `otel-mongo/v2.x.y` (its trigger pattern is kept so the mistake fails loudly). `otel-mongo`/`otel-mongo/v2`'s constant is a standalone `const instrumentationVersion = "..."` statement; `otel-nats`'s is inside a `const (...)` block with no per-line `const` keyword — the guard's extraction regex tolerates both shapes (`^\s*(const\s+)?instrumentationVersion\s*=`).
