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
| MongoDB | Document field `_oteltrace` | `{ traceparent, tracestate }` injected on every write; stripped on read |
| NATS/JetStream | Message headers | `traceparent`, `tracestate` headers via `HeaderCarrier` |
| WebSocket | JSON message body | `{"header":{"traceparent":...,"tracestate":...},"data":<payload>}` envelope on write; non-JSON payloads are JSON-string-encoded (not base64) into `data`; legacy flat top-level `traceparent`/`tracestate` fields still accepted as a read-only fallback |

### Feature Flags (otel-mongo)

Three switches plus optional `ConnectWithOptions` overrides. Since 0.9.0 the two module switches are **dynamic** — the env var is the fallback the relay overrides, not the final say (see **Dynamic feature flags** below).

| Switch | Scope |
|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (env only) | global kill switch. Off ⇒ no OpenFeature evaluation, no relay value can enable anything, and only the passthrough implementations are constructed |
| `otel-mongo-tracing` (relay; default `OTEL_MONGO_TRACING_ENABLED`) | gates **both** wrapper **CLIENT** spans **and** `_oteltrace` document propagation for this package |
| `otel-mongo-propagation` (relay; default `OTEL_MONGO_PROPAGATION_ENABLED`) | only consulted when the global switch and `otel-mongo-tracing` are on; final say on `_oteltrace` inject/extract on Collection/Cursor/ChangeStream and **ContextFromDocument** / **ContextFromRawDocument** |

Both module flags live in **one** `flags.Resolver` so they always come from the same snapshot instant — a torn read reporting tracing off while propagation read on could never correspond to a relay state.

When `otel-mongo-tracing` resolves off, this package emits no wrapper spans **and** force-disables `_oteltrace` propagation — Mongo tracing and Mongo trace propagation share a single kill switch. `WithTracePropagationEnabled` only overrides the propagation default while tracing resolves on; it **cannot** enable propagation when the global switch or `otel-mongo-tracing` is off.

`WithTracingEnabled(v bool)` (0.7.0+) overrides all of the above for a single `Client`, in either direction, and makes that client fully static — see **Per-connection tracing override** below.

### Per-connection tracing override (all four modules, 0.7.0+)

Every wrapper constructor accepts `WithTracingEnabled(v bool)` — `otelnats.ConnectWithOptions`/`ConnectTLSWithOptions`/`ConnectWithCredentialsWithOptions`, `otelmongo.ConnectWithOptions` (v1 and v2), `otelgorillaws.NewConn`/`Dial`/`Upgrader.Upgrade`. When present, it is authoritative for that connection/client, overriding the module's env-gate default in either direction; when absent, behavior is unchanged (env gates decide, exactly as before). The override composes **at the wrapper layer only** — `internal/flags` itself is untouched (still byte-identical across the four copies) and gains no new exported reset hooks. Everything constructed from an option-configured connection inherits its effective tracing state automatically (e.g. `oteljetstream` wrappers built from an `otelnats.Conn`, or `Database`/`Collection` built from an `otelmongo.Client`).

All four option appliers skip nil `Option` values (a literal `nil` variadic arg once made `ConnectTLS`/`ConnectWithCredentials` panic on every successful connection — pinned by `TestNewConnConfig_SkipsNilOptions` and siblings).

Three module-specific pitfalls to know if you touch this:
- **otel-mongo**: `resolveDocumentPropagation` (in `env_flags.go`) takes the caller's already-resolved effective tracing state as a parameter — it does **not** recompute `mongoTracingEnabled()` internally. If a future change reintroduces an internal recompute there, `WithTracingEnabled(true)` + `WithTracePropagationEnabled(true)` (env gates off) will silently stay disabled again. The process-wide, env-only `propEnabledGate` (`ContextFromDocument`/`ContextFromRawDocument`) is intentionally unaffected by this option — it still passes the plain env-derived value.
- **otel-mongo/v2 only**: driver v2's `options.MergeClientOptions` returns the **caller's own** `*ClientOptions` when exactly one is passed (v1 always builds a fresh struct), so `ConnectWithOptions` merges through a fresh `options.Client()` base before `SetMonitor` — otherwise it would mutate caller-owned options in place. Pinned by `TestConnectWithOptions_DoesNotMutateCallerOptions` (both modules, for parity).
- **otel-gorilla-ws**: `WithTracingEnabled` controls `Conn.featureEnabled` (whether any OTel SDK code path runs); `Conn.tracingEnabled` is the otel-ws subprotocol *negotiation outcome* — two distinct, similarly-named booleans. Since the negotiation-gating fix (0.7.0), `Dial` and `Upgrader.Upgrade` resolve the effective feature flag **before** the handshake and never offer/confirm otel-ws when it is off — otherwise the peer envelopes every frame and the feature-off side hands raw `{"header":...,"data":...}` bytes to the application (wire corruption, pinned by `TestUpgrader_TracingDisabled_DoesNotNegotiateOTelWS` / `TestDial_TracingDisabled_DoesNotOfferOTelWS`). `WithTracingEnabled(true)` still cannot force the envelope onto a peer that did not negotiate it.

### Shared `internal/flags` package

All four modules vendor their own copy of `internal/flags` (`flags.go` + `flags_test.go`); its doc comment requires the file contents (excluding the `package` line) to stay byte-identical across every copy. It exports two primitives:

- `EnvEnabled(name string) bool` — default-off env var read; unset or falsy (`0`/`false`/`no`/`off`, case-insensitive) → `false`.
- `Resolver` — resolves a module's dynamic flags through the process-global OpenFeature client and caches them in an immutable snapshot behind an `atomic.Pointer` with a **fixed one-second TTL**. `NewResolver(domain, WithSpecs(...), WithClock(...))` constructs one; `Enabled(i)` reads spec `i`. `WithClock` is test-only.

`Gate`/`NewGate`/`ResetForTest` were **removed in 0.8.0/0.9.0** — a process-lifetime cache is incompatible with runtime flag changes.

Each `Spec` pairs an OpenFeature key with the env var passed as the evaluation **default**, so `client.Boolean(ctx, key, EnvEnabled(envVar), ...)` expresses the whole fallback policy in one call: the relay decides when it has an opinion, otherwise the environment does. `Client.Boolean` returns the default on every failure path (no provider, not ready, flag absent, evaluation error), so there is no error handling and no undefined state.

The file names no module: flag keys and env var names live in each module's own `env_flags.go` and are passed in as `Spec` values.

### Dynamic feature flags (0.8.0/0.9.0+)

Tracing and Mongo propagation are resolved at **runtime** through [OpenFeature](https://openfeature.dev), so an operator can flip them via a GO Feature Flag relay proxy without restarting the application.

| OpenFeature key | Fallback env var | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

Precedence, highest first:

1. **`WithTracingEnabled(v)`** — when present the connection/client is fully **static**: implementation fixed at construction, no OpenFeature evaluation ever runs for it, no relay change reaches it.
2. **`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`** — an out-of-band **kill switch with no relay counterpart**. Off ⇒ no evaluation happens at all and no relay value can enable anything. It is the only brake that works when the relay is unreachable or misconfigured.
3. **The relay flag**, defaulting to the module env var when the relay has no opinion.

**Never call `openfeature.SetProvider`** (or `SetNamedProvider`/`SetEvaluationContext`/`AddHooks`/`Shutdown`) from library code — same rule as never initializing a `TracerProvider`. Applications install a provider at startup:

```go
provider, _ := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{Endpoint: "http://relay:1031"})
_ = openfeature.SetProviderAndWait(provider)
```

With no provider installed, every module behaves exactly as it did before dynamic flags. The library passes an empty `EvaluationContext{}`; targeting is the application's job via `SetEvaluationContext`. Because the snapshot is process-wide, only process-scoped targeting attributes are meaningful — **per-request targeting is not supported**.

**Tests that toggle env vars or install a provider must re-arm the module's resolver** (`resetPropEnabledCacheForTest` in otel-mongo, `resetNATSGateForTest`, `resetWSGateForTest`) and **must not call `t.Parallel`** — the provider, the environment and the resolver are all process-global.

### Consumer Context

Subscribers always receive a `Msg` (NATS/JetStream; call `.Context()` for the extracted trace) or a new `context.Context` return value (WebSocket `ReadMessage`) carrying the extracted remote trace. This context must be threaded into downstream calls to continue the trace chain.

### Span Links vs. Parent-Child

Async consumers (NATS subscribers, MongoDB change stream readers, WebSocket readers) use **span links** rather than parent-child relationships to connect to the producer span. This is intentional — preserves causality without implying synchronous nesting.

### Disabled-mode invariant (0.3.0+)

When any feature flag returns false, **no OTel SDK code path may run**: no `tracer.Start` on a real tracer, no `sdktrace.NewTracerProvider`, no `otlptracegrpc.New` / `otlptracehttp.New`, no `[]attribute.KeyValue` build, no propagator inject/extract. Two enforcement patterns coexist:

**1. Strategy split (preferred — otel-mongo Collection / Cursor / SingleResult / ChangeStream).** The facade type holds an `impl` interface satisfied by either `internal/direct.X` (passthrough) or `internal/traced.X` (instrumented). Construction picks the impl once; per-method runtime gates disappear. `internal/direct/*.go` imports no `go.opentelemetry.io/otel/sdk/*` and no `otel/exporters/*` — the disabled path is **compiler-enforced** by package boundary.

**2. Per-call gate (otel-nats `oteljetstream`, otel-gorilla-ws).** Every public method starts with `if !c.on() { /* delegate to native */ }`, where `on()` re-resolves the flag. Reviewer-enforced. These packages have no `internal/direct` boundary — direct and traced live in the same package — so there is nothing for the compiler to enforce.

**Which switch selects the implementation.** Since 0.8.0/0.9.0 the choice is `option != nil ? *option : EnvEnabled(GLOBAL)` — the **global switch alone**, not the module flag. This is load-bearing: keying it on the module flag would make `WithTracingEnabled(true)` with every env var off select the passthrough path, so the option could never produce a span. Consequence: a process with the global switch on and a module flag off now allocates the instrumented wrapper and pays one atomic load plus one clock read per operation. It still emits no spans.

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

### Propagation flag caching (otel-mongo)

`ContextFromDocument` / `ContextFromRawDocument` (`tracing.go`, both v1 and v2) call `cachedPropagationEnabled()`, which resolves through the module's `flags.Resolver` snapshot (`env_flags.go`) — the global env kill switch AND the dynamic `otel-mongo-tracing` AND `otel-mongo-propagation` values. **They follow the relay within the resolver's TTL** (changed in 0.9.0; previously a permanently cached env-only gate), so a flag that stops the `Collection` path also stops a change-stream reader in the same loop. They still ignore per-connection options — they are package-level functions with no client to consult. Tests that toggle any of those three vars via `t.Setenv` **must** call `resetPropEnabledCacheForTest()` after the Setenv to reset the cache. Helpers `enableTracing` / `enableDocumentPropagation` in `tracing_test.go` already invoke reset + `t.Cleanup` (and `enableDocumentPropagation` now sets all three flags). Do **not** add `t.Parallel()` to tests that touch these env vars — the reset is not parallel-safe.

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
- **Strategy-split layout:** Collection / Cursor / SingleResult / ChangeStream all live in `internal/{direct,traced}/` (see *Strategy-split layout (otel-mongo)* above). Client and Database still use the cached-gate pattern.
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

- `NewConn` wraps an already-dialed `*websocket.Conn`; the package-level `Dial` function dials and wraps in one step.
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
