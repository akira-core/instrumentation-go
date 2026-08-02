## Context

Every tracing and propagation switch in this repo is an environment variable read once per process. Two enforcement patterns consume those reads:

- **Strategy split** (`otel-mongo` Collection / Cursor / SingleResult / ChangeStream): construction picks an `internal/direct` (passthrough, no OTel SDK imports, CI-enforced) or `internal/traced` implementation once. There are no per-method runtime gates.
- **Cached gate** (`otel-nats`, `otel-gorilla-ws`, `otel-mongo` Client / Database): the constructor resolves a `tracingEnabled bool` onto the wrapper struct, and every public method starts with `if !c.tracingEnabled { /* delegate to native */ }`.

`internal/flags` is vendored as four byte-identical copies with zero external dependencies. It exports `EnvEnabled` (default-off env read) and `Gate` (a `sync.Once` + `atomic.Bool` cache whose documented contract is that environment changes after the first read are ignored).

Both patterns and the `Gate` contract assume the answer never changes after startup. Making the switches dynamic means revisiting all three.

The OpenFeature Go SDK is at v1.17.2. Its relevant surface: `openfeature.SetProviderAndWait(p)` installs a process-global provider, `openfeature.NewClient(domain)` returns a client bound to that global, and `client.Boolean(ctx, key, defaultValue, evalCtx)` returns `defaultValue` on any error — including when no provider was ever installed, since the SDK's default is a no-op provider. `openfeature/memprovider` provides an in-memory provider suitable for tests.

## Goals / Non-Goals

**Goals:**

- An operator can turn each module's tracing and propagation on or off through a GO Feature Flag relay proxy without restarting the application.
- Deployments that never configure an OpenFeature provider keep environment-driven span on/off behavior, with the D9/R4 exception for otel-ws negotiation (global-only gate may change wire between library peers when global is on and the module env is off).
- An out-of-band kill switch exists that does not depend on the relay being reachable or correctly configured.
- Hot paths pay a bounded, predictable cost — no network call, no OpenFeature evaluation pipeline, per operation.
- No new exported API and no new environment variables in any module.

**Non-Goals:**

- Per-request flag targeting (per tenant, per user). The design caches a single process-wide value per flag; request-scoped attributes cannot influence it.
- Dynamic sampling rates. `otel-sampler` is untouched.
- Changing span shapes, attributes, semantic conventions, or business logic anywhere.
- Owning the OpenFeature provider lifecycle, evaluation context, or relay polling interval. Those belong to the application.

## Decisions

### D1. Application owns the provider; the library only reads

`internal/flags` calls `openfeature.NewClient(domain)` and never `SetProvider`, `SetEvaluationContext`, or `Shutdown`.

This mirrors the rule this repo already applies to tracing: packages never initialize a `TracerProvider`, they fall back to `otel.GetTracerProvider()`. The reasons transfer verbatim — provider lifecycle is an application concern, a library must not mutate process-global state, and several instrumentation modules in one binary must not fight over it.

*Alternatives considered.* Having each `internal/flags` copy lazily construct its own provider from an environment variable would be zero-configuration, but an application using Mongo, NATS, and WebSocket would run three independent providers polling the relay, and would collide with an application that installs its own. Racing to install the global provider "first one wins" avoids the triple poll but still has a library silently mutating global state, and the SDK offers no reliable way to ask whether the installed provider is still the no-op default.

*Consequence.* Because the client is domain-scoped (`NewClient("otel-mongo")`), an application that wants a different provider for one module can install one with `SetNamedProvider("otel-mongo", p)`. This falls out for free; it is documented, not designed for.

### D2. The environment variable is the OpenFeature default value

Each flag is resolved as `client.Boolean(ctx, spec.Key, EnvEnabled(spec.EnvVar), evalCtx)`.

`client.Boolean` returns the default on every failure path — no provider installed, provider not ready, flag missing from the relay configuration, evaluation error, type mismatch. So a single call expresses the entire fallback policy: *the relay decides when it has an opinion, otherwise the environment does.* There is no error handling to write and no failure mode where the value is undefined.

*Consequence.* Relay outage behavior is inherited from the provider, not chosen here. The GO Feature Flag in-process provider serves its last successfully fetched configuration, so values freeze rather than snapping back to the environment. This is documented, not enforced.

### D3. `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is a hard kill switch with no relay counterpart

When the global switch is off, no OpenFeature lookup happens for any module and no relay value can enable anything.

An operator needs a way to stop tracing that does not depend on the relay — and the moments when that is most needed (relay unreachable, relay misconfigured, tracing itself causing the incident) are exactly the moments when a relay-mediated switch is least trustworthy.

*Alternatives considered.* Demoting the global switch to a default like the module switches would make every dimension bidirectionally dynamic, at the cost of having no out-of-band brake. Requiring `env AND relay` for every tier would make the relay a pure kill switch that can never turn anything on; the only reachable state that distinguishes it from this design is "global on, module env off, relay true", and that state is precisely the common case for turning tracing on to investigate a live incident. Worse, preserving any turn-on capability under that scheme requires shipping every module switch on everywhere and relying on relay values to hold them off — which inverts the failure mode, since a relay outage would then flood tracing on.

*Consequence.* A process that booted with the global switch off cannot be traced without a restart. This is deliberate.

### D4. Per-module snapshot behind an `atomic.Pointer`, refreshed lazily with a one-second TTL

```go
type Spec struct {
    Key    string // OpenFeature flag key
    EnvVar string // fallback env var, evaluated as the OpenFeature default value
}

type Resolver struct {
    client openfeature.IClient
    specs  []Spec
    ttl    time.Duration
    now    func() time.Time
    snap   atomic.Pointer[snapshot]
}

type snapshot struct {
    at     time.Time
    values []bool
}

func NewResolver(domain string, opts ...ResolverOption) *Resolver
func (r *Resolver) Enabled(i int) bool
```

`Enabled` loads the snapshot pointer, compares one timestamp, and returns `values[i]`. On expiry the calling goroutine evaluates every spec for that module and stores a fresh snapshot. Concurrent refreshes are permitted and unsynchronized: last store wins, and a lock on this path would cost more than the duplicate work it prevents.

**Snapshot timestamp.** `at` MUST be taken at the **start** of a refresh (before evaluating specs), not after the evaluation loop completes. Stamping at store time lets a slower refresh that read a stale relay value overwrite a fresher snapshot while carrying a newer clock, so the stale values appear fresh for a full TTL (PR #27 review). Stamping at start means a late stale writer either loses on time-order reasoning or is immediately eligible for re-refresh; it does not eliminate brief last-writer races, only the "stale looks fresh for 1s" failure mode. CAS/generation and singleflight remain non-goals (see post-review R3).

Evaluating every flag of a module together means one clock read covers all of them and the values are mutually consistent **within a single snapshot store** — `otel-mongo`'s tracing and propagation flags always come from the same refresh. Callers that invoke `Enabled` twice across a TTL boundary can still see two snapshots; operation-scoped consistency is enforced at call sites (R5), not by serializing every `Enabled` in the process.

*Alternatives considered.* Calling `client.Boolean` on each operation is 100 ns – 1 µs (hooks chain, evaluation context assembly, provider lock, flag lookup) — acceptable next to a Mongo round trip, not acceptable on a NATS publish, and paid even when the flag is off. A per-flag TTL costs one clock read per flag rather than per module and permits torn reads. A background ticker goroutine writing the snapshot would remove the clock read entirely (~1 ns instead of ~25 ns) but adds a permanently resident goroutine per module and a shutdown story this repo has no API for. Parallelizing the per-spec `Boolean` loop inside one refresh was rejected: modules have at most two specs, and fan-out complexity is not worth the gain (R13).

*Consequence.* Flag changes take effect within one second of the provider observing them; the provider's own polling interval (minutes) dominates end-to-end latency. The TTL is therefore fixed at one second and not configurable — tightening it cannot meaningfully improve responsiveness and loosening it saves nanoseconds. Concurrent refreshes may still briefly surface a stale last-writer value; the next expiry self-heals.

### D5. Module-specific data lives outside the byte-identical file

`internal/flags` must stay byte-identical across four copies, so it cannot name **module** flag keys or **module** environment variables. `Resolver` takes those as `Spec` values; each module's own `env_flags.go` — which is not shared — supplies them.

**Shared global kill-switch helper (post-review R13-B1).** The process-wide name `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is the one env string every module already hard-codes. `internal/flags` SHALL export:

```go
const EnvGlobalTracing = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"

func GlobalTracingPossible() bool { return EnvEnabled(EnvGlobalTracing) }
```

Call sites replace local `dynamicTracingPossible` / `wsNegotiationPossible` wrappers with `flags.GlobalTracingPossible()`. Module-specific godoc (especially D9's otel-ws negotiation rationale) lives at the Dial/Upgrade/capability call sites, not on the shared one-liner. No other `OTEL_*` names may appear in the shared file.

```go
// otel-mongo/otelmongo/env_flags.go
const (
    idxTracing = iota
    idxPropagation
)

var mongoResolver = flags.NewResolver("otel-mongo",
    flags.WithSpecs(
        flags.Spec{Key: "otel-mongo-tracing", EnvVar: envMongoTracingEnabled},
        flags.Spec{Key: "otel-mongo-propagation", EnvVar: envMongoPropagationEnabled},
    ),
)

func mongoTracingEnabled() bool {
    if !flags.GlobalTracingPossible() {
        return false
    }
    return mongoResolver.Enabled(idxTracing)
}
```

`mongoTracingEnabled` keeps its name and signature, so most call sites are untouched — what changes is that it is now cheap-but-not-free and may return a different answer than it did a second ago.

The refresh and fallback logic — the part most likely to drift if hand-copied — stays inside the byte-identical file, which is the whole point of that rule.

*Alternatives considered.* Leaving `internal/flags` untouched and writing the TTL, snapshot, and OpenFeature logic separately in each module would keep the existing spec unchanged but would place the highest-risk shared logic outside the only mechanism this repo has for preventing drift.

### D6. `Gate` is deleted

`natsGate`, `wsGate`, and `propEnabledGate` are the only users of `flags.Gate`, and all three are replaced by `Resolver`. `Gate`, `NewGate`, and `ResetForTest` are removed rather than left as dead code. The package is `internal/`, so nothing consumer-visible is removed.

### D7. Strategy selection keys on the global switch, except when overridden

```
useTracedImpl = (WithTracingEnabled option present) ? option value : EnvEnabled(GLOBAL)
```

The option branch is load-bearing and easy to get wrong. `WithTracingEnabled(true)` must keep working with every environment variable off — that is an existing, tested requirement. If implementation selection keyed on the global switch alone, that configuration would select the passthrough path and the option could never produce a span.

When the option is present the connection is fully static: the implementation is fixed at construction and OpenFeature is never consulted for it. When absent, the global switch fixes the implementation and the module's flag is resolved dynamically on each call.

*Consequence.* A process with the global switch on and a module switch off used to take the zero-cost passthrough path. It now allocates the instrumented wrapper and performs the snapshot read per call. It still emits no spans. This is a real performance regression for that configuration, accepted in exchange for not introducing a fifth environment variable to opt into dynamic mode.

### D8. Long-lived objects consult the flag per call, not per construction

The cached-gate modules convert almost for free: `if !c.tracingEnabled` becomes `if !c.tracingEnabled()`, where the method returns the option override when present and the resolver value otherwise. `otelnats.Conn`, `oteljetstream` wrappers, and `otelgorillaws.Conn` need no structural change.

The strategy-split types in `otel-mongo` need one. Each facade type holds **both** implementations and selects per call:

```go
type Collection struct {
    direct *direct.Collection
    traced *traced.Collection
    // ...
}

func (c *Collection) impl() collectionImpl {
    if c.tracingEnabled() {
        return c.traced
    }
    return c.direct
}
```

This keeps `internal/direct` free of OTel SDK imports (the CI grep and the package boundary are both unchanged) and keeps `internal/traced` free of gating code. The alternative — a runtime gate at the top of every `traced` method — would duplicate the entire `direct` package inside `traced` and defeat the split.

`Cursor` and `ChangeStream` follow the same dual-implementation shape rather than inheriting a fixed choice from the call that produced them. This matters most for `ChangeStream`, which can outlive many flag changes. Both are structurally able to: `traced.Cursor` and `traced.ChangeStream` hold only a tracer, a propagator and their propagation flag — no per-call span state — so the facade can build an instrumented and a passthrough wrapper around the same raw driver object.

`SingleResult` is the one exception, and it is forced rather than chosen. `traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`. A `FindOne` that ran through the passthrough path started no span, so there is nothing to construct an instrumented wrapper around. Selecting per call would also be incoherent in the other direction: a flag flip between `FindOne` and `Decode` would strand an already-started span that the passthrough path would never end. `SingleResult` is the result of one already-executed operation, so the flag value at `FindOne` time is the only meaningful answer — unlike a cursor or change stream, which keep producing new work.

*Consequence.* `collectionImpl` returns raw driver types for `Find`/`Aggregate`/`Watch` and the facade constructs both wrappers itself; `FindOne` keeps returning a `shared.SingleResultImpl` alongside its raw result. `traced.Collection`'s exported `PropagationEnabled bool` field becomes a `func() bool`, which facade-package tests that build `traced.Collection` literals must follow.

**Post-review cleanup of the dual-impl migration (R8).** After the facade builds dual Cursor/ChangeStream wrappers, the second return value from `Find`/`Aggregate`/`Watch` on `collectionImpl` is unused (`raw, _, err`). The interface SHALL return only `(*mongo.Cursor, error)` / `(*mongo.ChangeStream, error)` for those methods; `internal/{direct,traced}` SHALL NOT allocate a throwaway `NewCursor`/`NewChangeStream` on those paths. `FindOne` retains the dual return for the live span (D8 exception).

**Per-operation tracing/propagation consistency (R5).** Within a single public operation, the tracing decision used to select `impl()` SHALL be the same value passed into `resolveDocumentPropagation` / `propagationOn` — callers MUST NOT re-invoke `effectiveTracing()` / `mongoTracingEnabled()` for that operation's propagation decision. `ContextFromDocument` / `ContextFromRawDocument` SHALL resolve tracing once and pass it into `resolveDocumentPropagation`. Client and Database share one small gate-state helper so the rule is not hand-copied four times (R16).

**Facade `impl()` selection (R14).** The four-line `if traced != nil && tracing() { return traced }; return direct` pattern is factored once per module (v1 and v2) via a tiny generics helper; v1↔v2 parity copies remain required by CLAUDE.md.

### D9. `otel-gorilla-ws` negotiates `otel-ws` whenever the connection could ever trace

Negotiation happens during the handshake and cannot be revisited, so it is gated on the same expression as implementation selection in D7 — the option if present, otherwise the global switch — and specifically *not* on the dynamic value.

The read path is already safe either way: `tryUnmarshalWire` probes and falls back to the raw payload when the message is not an envelope. Only the write path must match what the peer agreed to, because sending an envelope to a peer that did not negotiate `otel-ws` hands `{"header":...,"data":...}` to that peer's application code.

**Envelope follows negotiation outcome, not feature-on aspiration (R1).** `Conn.tracingEnabled` means "otel-ws was negotiated (or proven via subprotocol)," not "this process might want spans."

- `Dial` / `Upgrader.Upgrade` set `tracingEnabled` from the handshake result (unchanged).
- `NewConn` has no handshake: it SHALL set `tracingEnabled` from `isOTelWireProtocol(conn.Subprotocol())` (N1). Callers that manage the handshake themselves must leave a correct negotiated subprotocol on the raw conn. There is **no** `WithOTelWSNegotiated` escape hatch (O1) — that would reintroduce force-envelope wire corruption.
- When negotiation failed / is unproven: **wire is raw passthrough**; if capability and the dynamic feature gate are on, **local send/receive spans may still be created** without inject/extract (S1).
- `configureConn` / construction SHALL clamp `tracingEnabled = tracingEnabled && capable` so a historical `true` cannot outlive a capability-off process (R7, same root as R1).
- `WithTracingEnabled(true)` is authoritative for the **feature / SDK** path; it MUST NOT force the JSON envelope onto a connection whose peer did not negotiate otel-ws.

*Alternatives considered.* Gating negotiation on the dynamic value at handshake time avoids the envelope entirely while tracing is off, but connections established during an "off" period could never propagate trace context afterwards — and WebSocket connections routinely live for hours. Keeping `NewConn`'s forced `tracingEnabled=true` was rejected after PR #27: with capability on and the dynamic flag off it still wrote empty-header envelopes to non-negotiating peers (regression vs 0.7.x passthrough-when-feature-off and vs the CHANGELOG promise that non-negotiating peers see no wire change).

*Consequence.* Two peers that both use this library with the global switch on now exchange the JSON envelope on every message even while tracing is off, which is a wire-format change and a JSON marshal per message. Peers that do not negotiate `otel-ws` — including all non-library clients and `NewConn` wrappers whose subprotocol is not otel-ws — see raw payloads.

### D10. Mongo document helpers resolve through the same snapshot

`ContextFromDocument` and `ContextFromRawDocument` are package-level functions with no connection to consult, which is why they are documented today as environment-only and deliberately unaffected by `WithTracingEnabled`. The relay value, unlike a per-connection option, is process-global and therefore visible to them.

They now read the same snapshot as the `Collection` path. The alternative leaves one environment variable governing two code paths that appear in the same change-stream loop, obeying the relay in one and ignoring it in the other — a split that is harder to reason about than no dynamism at all.

### D11. Flag keys are fixed, kebab-case, and module-scoped

| Flag key | Fallback env var | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

There is no key for the global switch; D3 makes it environment-only.

*Alternatives considered.* Reusing the environment variable strings as flag keys would give operators one identifier instead of two, but `UPPER_SNAKE` is foreign to GO Feature Flag configuration and welds the two namespaces together. Making keys overridable through additional environment variables would let a site match an in-house naming convention, but the relay configuration is written by that same site — naming a flag `otel-mongo-tracing` there costs nothing.

### D12. Evaluation context is the application's

The library passes an empty `openfeature.EvaluationContext{}`. Applications that want targeting install a global one with `openfeature.SetEvaluationContext`, which the SDK merges into every evaluation.

The library has no non-arbitrary source for `service.name` or `deployment.environment` — it would have to guess between `OTEL_SERVICE_NAME`, the OTel resource, and hostname — and anything it invented would collide with what the application set. Per D4 the snapshot is process-wide, so only process-scoped attributes are meaningful anyway.

### D13. Testing uses an in-memory provider and an injected clock

`NewResolver` accepts `WithClock(func() time.Time)`, exported for tests in the same way `Gate.ResetForTest` was. Tests install `memprovider.NewInMemoryProvider(...)` through `SetProviderAndWait`, mutate flag values, advance the fake clock past the TTL, and assert the new value is observed. This makes TTL behavior itself testable — that 0.9 s does not refresh and 1.1 s does — which a reset hook could only bypass.

One integration test stands up a real GO Feature Flag relay proxy container and drives one module end to end, verifying that the wiring recipe in the documentation actually resolves against a real relay: provider construction options, endpoint format, and flag keys matching a real relay configuration file. Only one module is covered; the wiring is identical across the four and three more containers would add cost without information.

A full harness-level assertion that spans stop reaching the OTLP sink after a flag flip is deliberately excluded. It would have to outwait the TTL, the provider's poll interval, and the exporter's batch timeout, making it a timing race; its two halves are already covered separately by the integration test (the value propagates) and the unit tests (a false value emits no span).

Because the OpenFeature provider and the environment are both process-global, tests that touch them must not call `t.Parallel` — the same constraint that already applies to the environment-toggling tests.

## Risks / Trade-offs

**A relay flag silently overrides a deliberately-off environment variable.** An operator who set `OTEL_MONGO_TRACING_ENABLED=false` to guarantee no tracing loses that guarantee if a flag named `otel-mongo-tracing` exists on the relay and is true. → Called out as BREAKING in every affected CHANGELOG and in the migration notes. Two ceilings remain that the relay cannot cross: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` for the whole process, and `WithTracingEnabled(false)` for one connection.

**Global switch on, module switch off is now slower.** That configuration takes the instrumented path and performs a snapshot read per operation. → It emits no spans, so only throughput is affected; the cost is one atomic load and one monotonic clock read. Sites that want the old cost profile turn the global switch off.

**Envelope overhead on WebSocket connections that are not tracing.** → Limited to peer pairs that both run this library with the global switch on, since `otel-ws` requires bilateral negotiation.

**Clock read on the hot path.** `time.Now()` is roughly 25 ns via vDSO. On a NATS publish measured in microseconds this is a fraction of a percent; it is nonetheless a new per-operation cost that did not exist. → Accepted over a resident ticker goroutine; revisit only with a benchmark showing it matters.

**Flag changes are not atomic across modules.** Each module refreshes independently, so a relay change touching Mongo and NATS together can be observed by one before the other, up to one TTL apart. → Within a module the snapshot is consistent, which is where combinations actually interact (tracing AND propagation).

**In-flight objects created before a change keep their construction-time wiring in one respect.** A WebSocket connection that did not negotiate `otel-ws` cannot start propagating, and per D9 that only happens when the global switch was off at handshake — in which case nothing about that connection is dynamic anyway. Mongo cursors and change streams do follow the flag per call, per D8.

**Four `go.mod` files gain the OpenFeature SDK.** Consumers that never use dynamic flags still resolve the dependency. → The SDK is small and dependency-light; the alternative designs that avoid it all require application-side wiring that this change explicitly set out to avoid.

## Migration Plan

1. Land all four modules in one commit — the byte-identical `internal/flags` copies cannot be split across commits.
2. Tag `otel-mongo/v0.9.0`, `otel-mongo/v2.9.0`, `otel-nats/v0.8.0`, `otel-gorilla-ws/v0.8.0`. Tags may be pushed sequentially; the release guard validates each against its version constant.
3. Existing deployments that upgrade **without** installing an OpenFeature provider keep **span on/off** behavior driven by the same environment variables as before, with one **documented exception (R4)**: `otel-gorilla-ws` subprotocol negotiation is gated on the **global** switch alone (D9), not `GLOBAL && OTEL_GORILLA_WS_TRACING_ENABLED`. Env-only deployments with **global on + module env off** that previously never offered/confirmed otel-ws will now negotiate it between library peers (envelope on the wire, no spans while the module flag is off). Third parties that never negotiate otel-ws remain on raw payloads. CHANGELOG, CLAUDE.md, and README migration notes MUST state this exception; the unconditional sentence "no provider ⇒ identical to previous release" is false for that case and must not stand alone.
4. Deployments adopting dynamic flags install a provider at startup, next to their existing `otelsetup.Init()` call, and create the four flags on the relay. Until a flag exists there, the corresponding environment variable continues to decide.

**Rollback.** Pin the previous module version. There is no persisted state, no wire-format migration for peers that never negotiated `otel-ws`, and no relay configuration that must be torn down — flags left on the relay are simply ignored by the older build.

## Post-review remediation (PR #27 grill, 2026-08)

Source: `reviews/code-review-pr-27-openfeature-dynamic-flags.zh-TW.html` and a decision grill on each finding. Implementation is tracked in `tasks.md` §8; this section is the normative design record of those decisions.

| ID | Topic | Severity (review) | Decision |
|----|--------|-------------------|----------|
| R1 | `NewConn` wire corruption when capability on + feature off | 85 | Fix behavior: envelope only if negotiated/proven (N1 Subprotocol); fail → raw wire; S1 local spans OK; O1 no force-negotiated option; clamp with capable (R7) |
| R2 | `MessageBatch` freezes flag at Fetch | 75 | Always return a dynamic batch wrapper; per-message gate re-check; bidirectional flip tests |
| R3 | `Resolver.refresh` last-store-wins + late `at` | 70 | Stamp `at` at evaluation **start**; no CAS/mutex this round |
| R4 | otel-ws negotiation vs "no provider ⇒ no change" | 70 | Keep D9 behavior; document exception in CHANGELOG / design migration / CLAUDE |
| R5 | Mongo single-call-chain torn read of tracing | 60 | Pass resolved tracing into propagation; no internal recompute; same for `ContextFromDocument` |
| R6 | JetStream per-message rebuild of tracer/attrs | 60 | Hoist tracer/prop/baseAttrs to construction (like `newTracedMessageBatch`); gate stays per-message |
| R7 | `capable` / `tracingEnabled` no choke-point clamp | 55 | Subsumed into R1 |
| R8 | Dead second return of `collectionImpl` Find/Aggregate/Watch | 55 | Drop second return; stop throwaway `New*` in impls |
| R9 | `tracedMessagesContext.Next` not gate-first | 55 | Gate-first delegate to `directMessagesContext` |
| R10 | Gate/propEnabledGate doc drift | 50 | Full sync: CLAUDE, test comments, jetstream godoc, main `openspec/specs/shared-feature-flags` |
| R11 | `WriteMessage` nil span + dual guards | 45 | Feature-off uses noop span; drop nil guards |
| R12 | NATS Consume path triple `impl()` per message | 40 | Resolve once per message; pass down |
| R13 | `dynamicTracingPossible` duplication / parallel refresh | 30 | **B1**: `flags.GlobalTracingPossible()`; call sites drop locals; **no** parallel Boolean in refresh |
| R14 | Six copies of facade `impl()` selection | 40 | Generics `selectImpl` per mongo module |
| R15 | Five copies of relay test helpers | 35 | Move to `otel-testkit/harness` |
| R16 | Client/Database `effective*` duplication | 35 | Shared gateState; implement with R5 |
| R17 | otelnats `impl`/`msgHandler`/`traceEventMsgHandler` policy | 30 | **WONTFIX** extract; optional lockstep comment only |
| R18 | Dead nil-handler guard in `tracedConsumeHandler` | 25 | Delete dead guard |
| R19 | Same-refresh sequential Boolean micro-torn pair | 50 filtered | **WONTFIX** — fail-safe via `resolveDocumentPropagation`; window µs; not worth serializing Boolean |

### R2 detail (JetStream MessageBatch)

OpenSpec already requires per-delivery follow of the relay for `MessagesContext` and `MessageBatch`. Construction-time choice of direct vs traced batch violates that. Fetch methods always return a wrapper whose forwarder re-reads the connection gate per message; when off, skip tracer/attributes/propagator entirely. Align CLAUDE.md examples so `Fetch` is not misread as "pin batch for life."

### R6 + R12 detail (JetStream / Consume hot path)

Dynamic means the **gate**, not re-deriving constant traced-branch metadata. Hoist `TraceContext`/`ServerAttrs`/`receiveBaseAttrs` where values are fixed for a traced connection; still re-check `TracingEnabled` (or equivalent) per message. Consume path must not call `impl()` three times for one message.

### Documentation sync (R4 + R10)

When remediating, rewrite every remaining reference to `Gate` / `NewGate` / `ResetForTest` / env-only permanent `propEnabledGate` in CLAUDE.md, module godoc, test helper comments, and **main** `openspec/specs/shared-feature-flags/spec.md` (not only this change's delta). Archive/sync may still apply deltas; main tree must not claim Gate exists after this change ships.

## Open Questions

None outstanding for the original design. Post-review R1–R19 are decided above; remaining work is implementation (`tasks.md` §8), not open product questions. Items deliberately excluded from this change (dynamic sampling rates, per-request targeting, a harness-level flag-flip E2E assertion, Resolver CAS/singleflight, parallel Boolean fan-out) stay Non-Goals.
