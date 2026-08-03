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

Two supporting modules sit alongside them (no instrumentation, no spans of their own):

| Module dir | Import path suffix | Purpose | Released? |
|---|---|---|---|
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

### Feature Flags (otel-mongo)

Five switches. The relay ones can only **revoke**; every env var is read once, at construction.

| Switch | Scope |
|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **or** `WithTracingEnabled` (`gate1`, never both) | process-wide kill switch. Off ⇒ no OpenFeature evaluation, no relay value can enable anything, and only the passthrough implementations are constructed |
| `OTEL_MONGO_TRACING_ENABLED` (env only) | ANDed with `gate1` to give `gate.tracedBuilt`. Off ⇒ the instrumented implementations are **not allocated** and the resolver is never consulted |
| `OTEL_MONGO_PROPAGATION_ENABLED` **or** `WithTracePropagationEnabled` (never both) | the `_oteltrace` tier, one level below tracing |
| relay `otel-mongo-tracing` | resolved per operation; revokes wrapper CLIENT spans and, through them, `_oteltrace` |
| relay `otel-mongo-propagation` | resolved per operation; revokes `_oteltrace` alone |

Both relay keys live in **one** `flags.Resolver`, but they are two consecutive `Boolean` calls, not one atomic read — the microsecond window is R19, accepted, and fail-safe because a false tracing verdict short-circuits propagation.

When tracing resolves off, this package emits no wrapper spans **and** writes no `_oteltrace` — Mongo tracing and Mongo trace propagation share a single kill switch. `WithTracePropagationEnabled` only supplies the propagation tier; it **cannot** enable propagation when `gate1`, `OTEL_MONGO_TRACING_ENABLED` or the relay says no.

`ContextFromDocument` / `ContextFromRawDocument` are **not** in this table — they carry no gate at all; see below.

`WithTracingEnabled(v bool)` (0.7.0+) overrides all of the above for a single `Client`, in either direction, and makes that client fully static — see **Per-connection tracing override** below.

### Per-connection tracing override (all four modules, 0.7.0+)

Every wrapper constructor accepts `WithTracingEnabled(v bool)` — `otelnats.ConnectWithOptions`/`ConnectTLSWithOptions`/`ConnectWithCredentialsWithOptions`, `otelmongo.ConnectWithOptions` (v1 and v2), `otelgorillaws.NewConn`/`Dial`/`Upgrader.Upgrade`. When present, it is authoritative for that connection/client, overriding the module's env-gate default in either direction; when absent, behavior is unchanged (env gates decide, exactly as before). The override composes **at the wrapper layer only** — `internal/flags` itself is untouched (still byte-identical across the four copies) and gains no new exported reset hooks. Everything constructed from an option-configured connection inherits its effective tracing state automatically (e.g. `oteljetstream` wrappers built from an `otelnats.Conn`, or `Database`/`Collection` built from an `otelmongo.Client`).

All four option appliers skip nil `Option` values (a literal `nil` variadic arg once made `ConnectTLS`/`ConnectWithCredentials` panic on every successful connection — pinned by `TestNewConnConfig_SkipsNilOptions` and siblings).

Three module-specific pitfalls to know if you touch this:
- **otel-mongo**: `resolveDocumentPropagation` (in `env_flags.go`) takes the caller's already-resolved effective tracing state as a parameter — it does **not** recompute `mongoTracingEnabled()` internally. If a future change reintroduces an internal recompute there, `WithTracingEnabled(true)` + `WithTracePropagationEnabled(true)` (env gates off) will silently stay disabled again. Package-level `ContextFromDocument`/`ContextFromRawDocument` follow the relay within the resolver's TTL (same three-tier gates as Collection) and intentionally ignore per-connection options.
- **otel-mongo/v2 only**: driver v2's `options.MergeClientOptions` returns the **caller's own** `*ClientOptions` when exactly one is passed (v1 always builds a fresh struct), so `ConnectWithOptions` merges through a fresh `options.Client()` base before `SetMonitor` — otherwise it would mutate caller-owned options in place. Pinned by `TestConnectWithOptions_DoesNotMutateCallerOptions` (both modules, for parity).
- **otel-gorilla-ws**: `WithTracingEnabled` controls `Conn.featureEnabled` (whether any OTel SDK code path runs); `Conn.tracingEnabled` is the otel-ws subprotocol *negotiation outcome* — two distinct, similarly-named booleans. Since the negotiation-gating fix (0.7.0), `Dial` and `Upgrader.Upgrade` resolve the effective feature flag **before** the handshake and never offer/confirm otel-ws when it is off — otherwise the peer envelopes every frame and the feature-off side hands raw `{"header":...,"data":...}` bytes to the application (wire corruption, pinned by `TestUpgrader_TracingDisabled_DoesNotNegotiateOTelWS` / `TestDial_TracingDisabled_DoesNotOfferOTelWS`). `WithTracingEnabled(true)` still cannot force the envelope onto a peer that did not negotiate it. **Since 0.8.0, `NewConn` also derives `tracingEnabled` from the raw conn's negotiated subprotocol (`isOTelWireProtocol`) instead of forcing it true, and `configureConn` clamps `tracingEnabled &&= capable`.** Two consequences to know: callers who hand-roll a handshake without `otel-ws` get no envelope and no WS trace propagation at all (`WithTracingEnabled(true)` cannot restore it — it only sets capability); and wrapping an `otel-ws`-negotiated conn with capability off makes `ReadMessage` return the peer's envelope bytes to the application unparsed, because the `!c.capable` fast path skips the unwrap.

### Shared `internal/flags` package

All four modules vendor their own copy of `internal/flags` (`flags.go` + `flags_test.go`); its doc comment requires the file contents (excluding the `package` line) to stay byte-identical across every copy. It exports:

- `EnvEnabled(name string) bool` — default-off env var read with a truthy **allow-list**: only `1`/`true`/`yes`/`on` (trimmed, case-insensitive) → `true`. Unset, the empty string and every other value → `false`.
- `EnvGlobalTracing` / `GlobalTracingPossible()` — the process-wide kill-switch name (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, the only `OTEL_*` literal allowed in this shared file) and its default-off read. Each module's `env_flags.go` aliases `envGlobalTracingEnabled = flags.EnvGlobalTracing` rather than repeating the literal.
- `EnvSet(name string) bool` / `GlobalTracingSet()` — bare presence tests, for the mutual-exclusion check only. Never use them to decide whether a switch is enabled.
- `FlagDomain` (`otel-instrumentation-go`) — the single OpenFeature domain **all four** modules resolve through. One per module is impossible: `InProcess.Init` is not idempotent, so one provider instance under N domains leaks N−1 unstoppable pollers.
- `EnvFlagsEndpoint` / `EnvFlagsAPIKey` / `EnvFlagsPollInterval` / `EnvServiceName` — the provider auto-install and its targeting attribute. Process-scoped, so allowed in the shared file.
- `Resolver` — resolves a module's relay verdicts through the OpenFeature client on **every call**. It caches **nothing**: no snapshot, no TTL, no clock, no refresh. `NewResolver(WithFlagKeys(...))` constructs one (no domain parameter); `Allowed(i)` reads key `i` and returns `false` for an out-of-range index.

`Gate`/`NewGate`/`ResetForTest` were **removed in 0.8.0/0.9.0** — a process-lifetime cache is incompatible with runtime flag changes. So were the module-level reset hooks (`resetPropEnabledCacheForTest`, `resetNATSGateForTest`, `resetWSGateForTest`): with nothing cached there is nothing to re-arm, and tests drive the real path by rebinding the provider.

The evaluation default is a literal `true` and the module env var is ANDed **separately** by each module, never passed as that default. That is what makes the relay revoke-only: `Client.Boolean` returns the default on every failure path (no provider, not ready, flag absent, evaluation error, type mismatch), so all of them mean *do not interfere* and the environment alone decides. Nothing on the relay can enable what the deployment left off.

`EnvEnabled` emits one `slog.Warn` when a variable is set to a value in neither the truthy nor the falsy list, then returns `false`. Unset, truthy and explicitly-falsy values stay silent, so a correct deployment logs nothing. Deliberately not deduplicated.

The file names no **module**: module flag keys and module env var names live in each module's own `env_flags.go` and are passed in through `WithFlagKeys`. Process-scoped names (the kill switch, the three `_FLAGS_*` variables, `OTEL_SERVICE_NAME`, `FlagDomain`) do belong here — they are properties of the binary, not of any module.

### Dynamic feature flags (0.8.0/0.9.0+)

Tracing and Mongo propagation are resolved at **runtime** through [OpenFeature](https://openfeature.dev), so an operator can flip them via a GO Feature Flag relay proxy without restarting the application.

| OpenFeature key | Fallback env var | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

Three **conjunctive** tiers — `tracing = gate1 && OTEL_<MODULE>_TRACING_ENABLED && relayVerdict` — not a precedence list. The relay can only **subtract**:

1. **`gate1`** — `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **or** `WithTracingEnabled(v)`, two spellings of one switch. Setting **both is a configuration error** returned by the constructor (`ErrTracingConfigConflict`), even when they agree; the check is on presence, not value. Off ⇒ no evaluation happens at all.
2. **`OTEL_<MODULE>_TRACING_ENABLED`** — read once at construction. Together with `gate1` it decides whether the instrumented implementation is **allocated at all**, so a module-off process keeps the pre-dynamic zero-cost passthrough.
3. **The relay flag** — resolved on **every operation**, with an evaluation default of `true`. It is the only tier that can change without a redeploy, and it can only revoke.

`WithTracingEnabled` no longer pins anything: a connection carrying it still reads the relay verdict per operation and still stops when the relay revokes.

Three further **process-scoped** variables configure the relay connection rather than any module: `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` (unset ⇒ no provider is installed), `_API_KEY`, and `_POLL_INTERVAL` (Go duration strings only, default `60s`). `OTEL_SERVICE_NAME` supplies a `service.name` targeting attribute, on the auto-install path only.

**Revocation latency is the provider's poll interval — 60 s by default — not "immediate".** The resolver adds none of its own.

**Never touch the DEFAULT provider from library code** — never `openfeature.SetProvider`, `SetEvaluationContext`, `AddHooks` or `Shutdown`, the same rule as never initializing a `TracerProvider`. Nothing the library does may change how the application's own feature flags resolve.

The one deliberate exception is the environment-driven auto-install: when `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` is set **and** `openfeature.NamedProviderMetadata(flags.FlagDomain).Name == "NoopProvider"` (no default provider and none bound to our domain), `Resolver.evaluator()` builds a GO Feature Flag provider and registers it with the non-blocking `SetNamedProvider(flags.FlagDomain, p)`. `DataCollectorDisabled: true` and in-process evaluation are hardcoded there. An application that installs its own provider first keeps it — the trigger fails and nothing is written.

Applications may still install a provider themselves, which also closes the non-blocking startup window:

```go
provider, _ := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint:              "http://relay:1031",
    DataCollectorDisabled: true, // required — see feature-flags.md
})
// Blocking install; on error log and continue rather than failing startup.
_ = openfeature.SetProviderAndWait(provider)
```

Two provider settings are load-bearing and easy to omit **on this path only** — the auto-install hardcodes both. `DataCollectorDisabled: true` avoids a buffer that flushes synchronously from the evaluating goroutine once full, which wedges every instrumented operation behind a failing 10 s request while the relay is down. `SetProviderAndWait` rather than `SetProvider` closes the startup window in which an unfetched provider cannot revoke anything; the auto-install deliberately does not block, so that window is the application's to close. Both are explained in `feature-flags.md`.

With no provider installed, every module's **span on/off** behavior is driven by the same environment variables as before dynamic flags. **Exception (otel-gorilla-ws):** subprotocol negotiation is gated on the global kill switch alone, not `GLOBAL && OTEL_GORILLA_WS_TRACING_ENABLED`, so env-only deployments with global on + module env off may negotiate otel-ws between library peers (envelope on the wire, no spans while the module flag is off). Peers that do not negotiate otel-ws still see raw payloads. The library passes an empty `EvaluationContext{}`; targeting is the application's job via `SetEvaluationContext`. Because the snapshot is process-wide, only process-scoped targeting attributes are meaningful — **per-request targeting is not supported**.

**Tests install their in-memory provider with `openfeature.SetNamedProviderAndWait(flags.FlagDomain, …)`, never `SetProviderAndWait`** — a named provider outranks the default for these clients, so a default install is silently shadowed once anything in the binary has auto-installed, and `clientOnce` makes that unrepeatable. There is no reset hook to call: the resolver caches nothing, so a rebound provider is observed on the very next operation. Tests **must not call `t.Parallel`** (the provider and the environment are process-global) and **must leave `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` unset**, or they will auto-install and reach the network. A **module** env var is read once at construction, so a test that changes one must reconstruct the wrapper.

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

**Which switch selects the implementation.** `gate1 && EnvEnabled(moduleEnv)` — the **whole static part** of the decision, both terms fixed at construction. Keying it on the global switch alone (the 0.8.0 shape) allocated the instrumented wrapper for module-off processes and made them pay per operation for nothing. Including the module flag is safe **only because the relay can never enable**: with it off, no relay value could raise the answer, so the instrumented path is unreachable and there is no reason to build it — nor to register `shared.NewCommandMonitor`, which otherwise runs on every MongoDB command. The previous release's zero-cost passthrough is preserved for every configuration that had it.

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
- **Dual implementation (0.9.0+).** Facade `Collection`, `Cursor` and `ChangeStream` hold **both** a `direct` and a `traced` field plus a `tracing func() bool`, and select per operation via `impl()`. `traced` is nil when the global kill switch was off at construction, so no OTel path is reachable. `SingleResult` is the **documented exception**: `traced.SingleResult` holds the live `FindOne` span (ended once on the first `Decode`/`TraceContext`/`Raw`), so a passthrough `FindOne` leaves nothing to wrap and a mid-flight flip would strand an unended span — its implementation stays fixed by the path that ran the `FindOne`.
- `traced.Collection.PropagationEnabled` is a `func() bool` (not a `bool`), read per call via the nil-safe `t.propagationOn()`. Same for `traced.Cursor` / `traced.ChangeStream`'s `propagationEnabled`.
- Facade `collectionImpl` interface returns raw driver types (`*mongo.Cursor`, `*mongo.SingleResult`, `*mongo.ChangeStream`) + `shared.XImpl` — the impl packages never need to import the facade, preventing any facade ↔ internal cycle. Facade methods wrap raw types into facade wrappers (`&Cursor{Cursor: raw, impl: cImpl}`).
- `internal/traced.Collection` has **exported fields** (`Coll`, `Tracer`, `Propagator`, `PropagationEnabled`, `ServerAddr`, `ServerPort`) so facade-package tests can build literals and call them directly.
- v1/v2 parity extends to `internal/{direct,traced,shared}/`. The helpers in `internal/shared/{bulkwrite.go,semconv.go,tracing.go,impls.go,monitor.go,hostport.go}` are intentionally duplicated across modules (separate `internal/` trees cannot share). A drift-check CI step to catch divergence between the two copies is planned but not yet implemented.
- `internal/shared/monitor.go` builds the `event.CommandMonitor` (`shared.NewCommandMonitor`) that captures the real per-command server address from `CommandStartedEvent.ConnectionID` into a context-scoped holder (`shared.WithAddrCapture`/`*shared.AddrCapture.Resolve`), chaining any caller-supplied monitor rather than replacing it. `client.go`'s `ConnectWithOptions` registers it (tracing-enabled branch only, via `options.MergeClientOptions`); `internal/traced/collection.go` call sites read it back after the raw driver call to overwrite `server.address`/`server.port` on the span, falling back to the static URI-parsed value when nothing was captured. `internal/shared/hostport.go` (`SplitHostPort`) is the shared IPv6-aware host:port parser used by both `monitor.go` and `client.go`'s `parseServerFromURI`.

### The ungated document helpers (otel-mongo)

`ContextFromDocument` / `ContextFromRawDocument` (`tracing.go`, both v1 and v2) carry **no feature-flag gate at all** — not `gate1`, not the module env vars, not the relay verdicts. They start no span, build no attributes, initialise no part of the OTel SDK and write nothing: they read `_oteltrace` out of a value the caller already holds and return what it encodes, and you only call them when you want extraction.

`Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace` look similar and **are** gated, because each starts and ends a real `mongo.cursor.decode` span. So **a revocation stops those but does not stop extraction here** — this pair is the supported way to keep trace linking while the library is silenced, and `feature-flags.md` says so in those words. **BREAKING in 0.9.0:** a fully-disabled process now gets a valid span context from them where it previously got nothing.

Consequences worth knowing: they cost no relay evaluation, so a per-document change-stream loop no longer pays one; and there is nothing for them to misread when `gate1` is supplied as `WithTracingEnabled` rather than the environment variable, which removes the option blind spot the previous design had.

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
- `otel-sampler/otelsampler/version.go` — `instrumentationVersion` const (not an instrumentation scope — this module emits no spans; the constant exists for the release guard and for callers recording which sampler build they run). `otel-sampler/v0.1.0` is published but points at a pre-rebase commit, so it is superseded and unusable — releases start at `0.1.1`. `otel-testkit` is deliberately untagged.

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

- `test-and-lint` — matrix over all six modules (`otel-sampler`, `otel-testkit`, `otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`): `go build`, `go test -race`, `golangci-lint`. For `otel-mongo` and `otel-mongo/v2` only, an additional "Verify direct/ has no OTel SDK imports" step greps `internal/direct/` for `go.opentelemetry.io/otel` imports and fails the build if any are found — this is the CI-enforced half of the disabled-mode invariant described above (the strategy-split package boundary is the compiler-enforced half).
- `integration-test` — gated on `needs: test-and-lint`; matrix over `otel-nats/tests/integration`, `otel-mongo/tests/integration`, `otel-mongo/v2/tests/integration`, and `otel-gorilla-ws/tests/integration`, running `go test -v -race -timeout 300s` over `go list ./...` **minus `/sampling`** (testcontainers-based, requires Docker). The `./sampling` packages are excluded on purpose — they belong to the dedicated flag-matrix jobs below, and running them here too doubles the Docker cost and squeezes a 600s suite into a 300s budget. The same exclusion is mirrored in the Makefile's `test-integration`.
- `sampling-e2e` / `nats-sampling-e2e` — the feature-flag × `OTEL_TRACES_SAMPLER_ARG` matrices, the only place `./sampling` runs (600s budget).
- `http-direct-e2e` — lints and runs both `otel-testkit/examples/httpdirect` (consistent sampler) and `httpdirect-stdlib` (sampler-agnostic baseline).

`.github/workflows/release-guard.yml` (0.7.0+) runs only on pushed tags matching one of the five module shapes (`otel-mongo/v[0-9]*`, `otel-mongo/v2/v[0-9]*`, `otel-nats/v[0-9]*`, `otel-gorilla-ws/v[0-9]*`, `otel-sampler/v[0-9]*`) — see `VERSIONING.md`. It parses the module and version out of the tag and fails if they don't match that module's version constant (table above). Routing details: `otel-mongo/v2.*` tags validate against `otel-mongo/v2/version.go` (the v2 module's Go-resolvable shape); the deprecated `otel-mongo/v2/v*` shape fails immediately with a pointer to `otel-mongo/v2.x.y` (its trigger pattern is kept so the mistake fails loudly). `otel-mongo`/`otel-mongo/v2`'s constant is a standalone `const instrumentationVersion = "..."` statement; `otel-nats`'s is inside a `const (...)` block with no per-line `const` keyword — the guard's extraction regex tolerates both shapes (`^\s*(const\s+)?instrumentationVersion\s*=`).
