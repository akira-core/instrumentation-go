## Context

Every tracing and propagation switch in this repo is an environment variable read once per process. Three enforcement patterns consume those reads, and which one a type uses is decided by how much of its surface needs instrumenting:

- **Strategy split** (`otel-mongo` Collection / Cursor / SingleResult / ChangeStream; all of `otel-nats` — both `otelnats.Conn` and the `oteljetstream` wrappers). The flag decides *which object the caller talks to*: two types implement one interface, one pure delegation and one pure instrumentation, so no method body contains both paths. Chosen where nearly every method needs a span — `collectionImpl` is 15 methods, and the passthrough and instrumented implementations are 99 and 423 lines respectively. `otel-mongo` enforces the split across an `internal/direct` / `internal/traced` package boundary, so the disabled path is **compiler-enforced**: `internal/direct` imports no `go.opentelemetry.io/otel` package and CI greps to keep it that way. `otel-nats` keeps `directConn`/`tracedConn` and `directJSImpl`/`tracedJSImpl` in one package, so its equivalent is reviewer-enforced.
- **Cached gate** (`otel-gorilla-ws`). The flag decides *which branch inside a method runs*: the constructor resolves a `featureEnabled` bool onto the wrapper and each instrumented method opens with a gate check. Chosen because `Conn` embeds `*websocket.Conn` — that embedding is what gives callers the whole gorilla API for free, and only `WriteMessage` and `ReadMessage` need wrapping. Splitting two methods would cost an interface over roughly thirty.
- **Gate carrier** (`otel-mongo` Client / Database). Neither type instruments anything: `Disconnect`, `Ping`, `StartSession` and `Database.Collection` are pure delegation with no span. Their whole job is to resolve the flag state once and hand it down to the `Collection` / `Cursor` / `ChangeStream` wrappers that do the work. There is nothing to split and nothing to gate — only state to carry, which R16 factors into a single `gateState` helper.

`internal/flags` is vendored as four byte-identical copies with zero external dependencies. It exports `EnvEnabled` (default-off env read) and `Gate` (a `sync.Once` + `atomic.Bool` cache whose documented contract is that environment changes after the first read are ignored).

All three patterns and the `Gate` contract assume the answer never changes after startup. Making the switches dynamic means revisiting all of them: the split must re-select per operation, the gate must re-read per call, and the carrier must stop caching what it hands down. It also ends `internal/flags`'s zero-dependency property — a property worth naming, because a vendored four-copy file with no imports is trivially safe to duplicate. That trade is accepted in D5.

The OpenFeature Go SDK is at v1.17.2. Its relevant surface: `openfeature.SetProviderAndWait(p)` installs a process-global provider, `openfeature.NewClient(domain)` returns a client bound to that global, and `client.Boolean(ctx, key, defaultValue, evalCtx)` returns `defaultValue` on any error — including when no provider was ever installed, since the SDK's default is a no-op provider. `openfeature/memprovider` provides an in-memory provider suitable for tests.

This document has been revised once. An earlier model — in which the relay decided in both directions and `WithTracingEnabled` pinned a connection static — was implemented and merged before design review replaced it, so the code in the tree does not yet match what follows. See § "Superseded decisions" for the point-by-point mapping and `tasks.md` § 9 for the remaining work.

## Goals / Non-Goals

**Goals:**

- An operator can **turn off** each module's tracing and propagation through a GO Feature Flag relay proxy without restarting the application.
- No remote party can turn anything **on**. Everything that is enabled was enabled by a reviewed deployment.
- Deployments that never configure an OpenFeature provider keep exactly their environment-driven behavior, modulo the `EnvEnabled` truthiness change in D14.
- Hot paths pay a bounded, predictable cost: no network call and no OpenFeature evaluation on any given operation, and the evaluation pipeline is entered at most once per TTL window per module.
- The compiler-enforced disabled path survives. `internal/direct` still imports no `go.opentelemetry.io/otel` package, CI still greps for it, and a process whose switches are off still cannot reach OTel code.
- A configuration that expresses two different intents for the same switch fails loudly at construction rather than silently picking one.
- No new environment variables. New exported API is limited to what the mutual-exclusion rule forces: one error sentinel per module (two in `otel-mongo`) and one changed constructor signature.

**Non-Goals:**

- Remotely enabling tracing. This is the deliberate inverse of the usual feature-flag posture; see D2.
- Supporting the GO Feature Flag provider's remote evaluation mode, which would put an HTTP request on the operation path; see D4.
- Per-request flag targeting (per tenant, per user). The design caches a single process-wide value per flag; request-scoped attributes cannot influence it.
- Dynamic sampling rates. `otel-sampler` is untouched.
- Changing span shapes, attributes, semantic conventions, or business logic anywhere.
- Owning the OpenFeature provider lifecycle, evaluation context, or relay polling interval. Those belong to the application.

## The resolution model in one place

Construction resolves everything that can never change again. Every conflict is collected before any of them is returned, so a caller that violates both rules learns both at once rather than fixing one and rediscovering the other:

```go
// Construction, once per connection/client.
var errs []error
if flags.GlobalTracingSet() && tracingOption != nil {
    errs = append(errs, fmt.Errorf("%w: option=%v, %s=%q",
        ErrTracingConfigConflict, *tracingOption, flags.EnvGlobalTracing, os.Getenv(flags.EnvGlobalTracing)))
}
if flags.EnvSet(envMongoPropagationEnabled) && propagationOption != nil {   // otel-mongo only
    errs = append(errs, fmt.Errorf("%w: option=%v, %s=%q",
        ErrTracePropagationConfigConflict, *propagationOption, envMongoPropagationEnabled, os.Getenv(envMongoPropagationEnabled)))
}
if len(errs) > 0 {
    return nil, errors.Join(errs...)     // tracing first, propagation second — order is fixed
}

gate1 := flags.GlobalTracingPossible()
if tracingOption != nil {
    gate1 = *tracingOption
}
gateProp := flags.EnvEnabled(envMongoPropagationEnabled)                    // otel-mongo only
if propagationOption != nil {
    gateProp = *propagationOption
}

// Both terms are environment-derived and fixed here, so they also decide which
// implementations are allocated at all.
useTracedImpl := gate1 && flags.EnvEnabled(moduleTracingEnv)
```

```go
// Every operation. Only the relay verdicts can still change.
//
// AllowedAll, not two Allowed calls: a module that needs more than one verdict
// must take them from one snapshot, or an operation can select the traced impl
// on a pre-refresh tracing verdict and resolve propagation from a post-refresh one.
allowed := resolver.AllowedAll()

tracing     := useTracedImpl && allowed[idxTracing]
propagation := tracing && gateProp && allowed[idxPropagation]               // otel-mongo only
```

Three tiers, three owners, three distinct powers:

| Tier | Owner | When it is off | Can it be changed without a redeploy? |
|---|---|---|---|
| `gate1` — `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **or** `WithTracingEnabled`, never both | deployer, or the caller that constructs the wrapper | Every module in the process is off, only passthrough implementations are allocated, and no OpenFeature code path is reachable | No |
| `OTEL_<MODULE>_TRACING_ENABLED` (and `OTEL_MONGO_PROPAGATION_ENABLED`) | deployer | That module is off, only its passthrough implementation is allocated, and its resolver is never consulted | No |
| relay flag `otel-<module>-tracing` (and `otel-mongo-propagation`) | operator | That module stops emitting on a running process, within one TTL | **Yes — this is the only tier that can** |

The first two tiers are conjunctive and interchangeable in effect; they differ in scope (whole process vs one module) and in who owns them. The third is the only dynamic one, and it can only subtract from what the first two allow.

## Decisions

### D1. Application owns the provider; the library only reads

`internal/flags` calls `openfeature.NewClient(domain)` and never `SetProvider`, `SetNamedProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown`. The client itself is created lazily on the first refresh rather than in `NewResolver`, so a process whose switches are off never initializes any part of the OpenFeature SDK.

This mirrors the rule this repo already applies to tracing: packages never initialize a `TracerProvider`, they fall back to `otel.GetTracerProvider()`. The reasons transfer verbatim — provider lifecycle is an application concern, a library must not mutate process-global state, and several instrumentation modules in one binary must not fight over it.

*Alternatives considered.* Having each `internal/flags` copy lazily construct its own provider from an environment variable would be zero-configuration, but an application using Mongo, NATS, and WebSocket would run three independent providers polling the relay, and would collide with an application that installs its own. Racing to install the global provider "first one wins" avoids the triple poll but still has a library silently mutating global state, and the SDK offers no reliable way to ask whether the installed provider is still the no-op default.

*Consequence — the domain is a hook, not an isolation boundary.* `NewClient("otel-mongo")` resolves through the process-global default provider unless the application installs a named one. An application that wants a different provider for one module can do so with `SetNamedProvider("otel-mongo", p)`, and that falls out for free — but until it does, any other library in the same binary that installs a global provider (breaking the rule we follow) also decides our flags. Under D2 the worst such a provider can do is revoke, so the failure is in the safe direction; the domain name alone should nonetheless not be read as protection.

*Consequence — the provider must be ready before the first operation.* D2 makes an unresolvable flag mean "do not interfere", which is fail-open with respect to the relay. That is correct in steady state and wrong for exactly one window: process startup. An application that installs its provider with `openfeature.SetProvider` gets a non-blocking install, so between that call and the provider's first successful fetch every flag resolves to `true`. A module whose environment variable is truthy — which it must be for the relay to control it at all — is therefore **on** during that window, even while the relay is revoking it.

The scenario that makes this matter is the ordinary one: an operator revokes a module to stop an incident, and the process restarts for an unrelated reason. Under the superseded design a not-ready provider fell back to the environment, which for a deployment expecting relay control was usually off, so it failed closed. Under this one it falls back to allow. Applications SHALL therefore install the provider with `openfeature.SetProviderAndWait` — or otherwise block until it reports ready — before serving traffic. This is a requirement of the design rather than a stylistic preference, and the README states it as one, because an application has no way to derive the reason on its own.

### D2. The relay is a kill switch: the evaluation default is always `true`

Each flag is resolved as `client.Boolean(ctx, key, true, evalCtx)`, and the module's environment variable is read separately by the module and **ANDed** with the result:

```go
enabled := EnvEnabled(moduleEnvVar) && client.Boolean(ctx, key, true, evalCtx)
```

Written that way for clarity; in practice the environment half is hoisted. Because it cannot change after construction, D7 folds it into `useTracedImpl` once, and the per-operation expression reduces to the relay verdict alone.

The resolver never sees the environment variable. That is deliberate: `internal/flags` receives only OpenFeature keys (D5), and the conjunction lives where the module's own constants already live.

The relay can therefore only subtract. A `false` on the relay disables a module that the environment enabled; nothing on the relay can enable a module that the environment left off. `Client.Boolean` returns the default on every failure path — no provider installed, provider not ready, flag missing from the relay configuration, evaluation error, type mismatch — so all of those resolve to `true`, meaning *do not interfere*, and the environment alone decides.

The reasoning is a safety posture rather than a convenience one: **anything that is on was turned on by a deployment that someone reviewed.** A misconfigured, compromised, or merely stale relay cannot introduce instrumentation — and for `otel-mongo`, cannot introduce *writes into the application's documents* (see D10 and the Risks section). The relay's job is to be an emergency brake that works when the deployment pipeline is too slow.

Because the environment variable is read separately and ANDed rather than passed as the evaluation default, a module whose environment variable is off **short-circuits before touching the resolver**. There is nothing to ask.

*Alternatives considered.* Passing `EnvEnabled(moduleEnvVar)` as the evaluation default — the shape this design carried through PR #27 — makes the relay authoritative in both directions and expresses the whole fallback policy in one call. It is the conventional feature-flag posture and it permits the operationally attractive move of turning tracing on to investigate a live incident. It was rejected because the same mechanism lets a relay turn instrumentation on in a process whose deployment deliberately left it off, and because for `otel-mongo` "on" means new fields written into stored documents (D10). A design where the failure mode of a remote system is *more* instrumentation, not less, was judged the wrong trade for a library that ships inside other people's applications.

*Consequence — the capability that is lost.* There is no way to enable tracing remotely. A site that wants relay control must deploy with `OTEL_<MODULE>_TRACING_ENABLED` truthy, run with tracing on, and use the relay to hold it off. Investigating an incident on a process that shipped with the module switch off requires a redeploy. This is deliberate and must be stated in the README, because it inverts what operators expect a feature flag to do.

*Consequence — failure direction.* When the relay is unreachable the resolved state is whatever the deployment declared, not a fallback to "off". The library never fails into a state nobody deployed.

*Consequence — there is no single revoke-everything switch.* D3 keeps `gate1` environment-only, so the relay has no key that stops the whole process. An operator who needs to silence every module revokes all four flags. This was true before as well, but it matters more now that the relay is the primary brake rather than a secondary one, so the README states it next to the flag key table.

### D3. `gate1` is a single switch expressible in exactly one place

The process-wide kill switch `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and the per-connection option `WithTracingEnabled(v bool)` are two spellings of the same tier. Passing both is a configuration error:

```go
if flags.GlobalTracingSet() && tracingOption != nil {
    // collected, not returned immediately — see § "The resolution model in one place"
    errs = append(errs, fmt.Errorf("%w: option=%v, %s=%q", ErrTracingConfigConflict, ...))
}
```

The check is on **presence**, not on value — setting both to the same value is still an error. The rule an operator has to remember is "set the environment variable *or* pass the option, never both", which is checkable at a glance; "they must agree" is not, because agreement depends on the truthiness rules in D14.

`EnvSet` (a bare `os.LookupEnv` presence test) is required because `EnvEnabled` cannot distinguish "unset" from "set to a falsy value", and the two must behave differently here: unset means the deployment expressed no opinion and the option may supply one.

When `gate1` resolves off, the module constructs only its passthrough implementation, performs no OpenFeature evaluation, and no relay value can reach it. D7 extends the same treatment to the module environment variable, which is the other term fixed at construction.

*Alternatives considered.* Letting the option override the environment variable — the shape prior to this change — is more flexible and supports a downstream test that builds one traced and one untraced connection in the same process while the environment is set. It was rejected because two sources of truth for one switch produce configurations whose behavior nobody can predict by reading either one alone. The test use case survives: with the environment variable unset, the option is the only source and both connections behave per their option values. Erroring only when the *values* differ was also considered and rejected for the reason above.

*Consequence.* `WithTracingEnabled` is no longer a per-connection override of a process-wide setting. It is an alternative spelling of that setting, scoped to one connection, usable only where the environment stays silent.

### D4. Per-module snapshot behind an `atomic.Pointer`, refreshed lazily with a one-second TTL

```go
type Resolver struct {
    client openfeature.IClient
    keys   []string   // OpenFeature flag keys, in Allowed-index order
    ttl    time.Duration
    now    func() time.Time
    snap   atomic.Pointer[snapshot]
}

type snapshot struct {
    at     time.Time
    values []bool
}

func NewResolver(domain string, opts ...ResolverOption) *Resolver
func WithFlagKeys(keys ...string) ResolverOption
func (r *Resolver) Allowed(i int) bool     // relay verdict for key i; true means "not revoked"
func (r *Resolver) AllowedAll() []bool     // every key's verdict from ONE snapshot load
```

`Allowed` loads the snapshot pointer, compares one timestamp, and returns `values[i]`. On expiry the calling goroutine evaluates every key for that module and stores a fresh snapshot. Concurrent refreshes are permitted and unsynchronized: last store wins, and a lock on this path would cost more than the duplicate work it prevents.

**Snapshot timestamp.** `at` MUST be taken at the **start** of a refresh (before evaluating any key), not after the evaluation loop completes. Stamping at store time lets a slower refresh that read a stale relay value overwrite a fresher snapshot while carrying a newer clock, so the stale values appear fresh for a full TTL (PR #27 review, R3). Stamping at start means a late stale writer either loses on time-order reasoning or is immediately eligible for re-refresh; it does not eliminate brief last-writer races, only the "stale looks fresh for 1 s" failure mode. CAS/generation and singleflight remain non-goals.

**`AllowedAll` and cross-flag consistency.** `Allowed(i)` twice in one operation can straddle a TTL boundary and read two snapshots. `AllowedAll` exists so a caller that needs several of a module's flags observes one snapshot load, which is the only way to make the "all flags of a module share one instant" guarantee true rather than aspirational. `otel-mongo`'s tracing + propagation pair is the reason it exists; `otel-nats` and `otel-gorilla-ws` have one spec each and use `Allowed`.

**Refresh is bounded.** `refresh` evaluates under `context.WithTimeout(context.Background(), refreshTimeout)`. The context is deliberately **not** the caller's: a flag snapshot is process-scoped state, and letting whichever goroutine happened to trigger the refresh donate its cancellation would make the snapshot's fate depend on an unrelated request's lifetime. The timeout exists because `refresh` runs synchronously on a caller's goroutine, so a provider configured for remote evaluation (see D2's provider-mode constraint below) would otherwise block a Mongo or NATS operation for that provider's HTTP timeout — 10 s by default. On timeout `Boolean` returns its default `true`, which is exactly the documented "relay does not interfere" outcome, so the fail-safe path needs no extra handling.

**Supported provider mode.** The documented wiring uses the GO Feature Flag provider's default `EvaluationType`, `INPROCESS`, in which the provider polls the relay in the background (120 s by default) and each `Boolean` is a local lookup. `EvaluationTypeRemote` is **not supported**: it turns every evaluation into an HTTP request and therefore puts network I/O on the operation path once per TTL window. The `refreshTimeout` above bounds the damage if a site configures it anyway; it does not make it supported.

*Alternatives considered.* Calling `client.Boolean` on each operation is 100 ns – 1 µs (hooks chain, evaluation context assembly, provider lock, flag lookup) — acceptable next to a Mongo round trip, not acceptable on a NATS publish, and paid even when the flag is off. A per-flag TTL costs one clock read per flag rather than per module and permits torn reads. A background ticker goroutine writing the snapshot would remove the clock read entirely (~1 ns instead of ~25 ns) but adds a permanently resident goroutine per module and a shutdown story this repo has no API for. Parallelizing the per-key `Boolean` loop inside one refresh was rejected: modules have at most two keys, and fan-out complexity is not worth the gain (R13).

*Consequence.* A revocation takes effect within one second of the provider observing it; the provider's own polling interval (minutes) dominates end-to-end latency. The TTL is therefore fixed at one second and not configurable — tightening it cannot meaningfully improve responsiveness and loosening it saves nanoseconds. Concurrent refreshes may still briefly surface a stale last-writer value; the next expiry self-heals.

### D5. Module-specific data lives outside the byte-identical file

`internal/flags` must stay byte-identical across four copies, so it cannot name **module** flag keys or **module** environment variables. `Resolver` receives the OpenFeature keys through `WithFlagKeys`; each module's own `env_flags.go` — which is not shared — supplies them, and owns the paired environment variable outright. The resolver never learns that a pairing exists, which is what keeps the shared file free of module vocabulary.

**Shared global kill-switch helper.** The process-wide name `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is the one env string every module already hard-codes. `internal/flags` SHALL export:

```go
const EnvGlobalTracing = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"

func GlobalTracingPossible() bool { return EnvEnabled(EnvGlobalTracing) }
func GlobalTracingSet() bool      { return EnvSet(EnvGlobalTracing) }
```

Module-specific godoc (especially D9's otel-ws negotiation rationale) lives at the Dial/Upgrade/capability call sites, not on the shared one-liners. No other `OTEL_*` names may appear in the shared file.

```go
// otel-mongo/otelmongo/env_flags.go
const (
    idxTracing = iota
    idxPropagation
)

var mongoResolver = flags.NewResolver("otel-mongo",
    flags.WithFlagKeys(
        "otel-mongo-tracing",      // paired with envMongoTracingEnabled, ANDed by this package
        "otel-mongo-propagation",  // paired with envMongoPropagationEnabled, ANDed by this package
    ),
)
```

The refresh, timeout and TTL logic — the part most likely to drift if hand-copied — stays inside the byte-identical file, which is the whole point of that rule. The zero-dependency property that file used to have is given up here; the OpenFeature SDK is the only import added, and it is the reason the file is worth sharing at all.

*Alternatives considered.* Leaving `internal/flags` untouched and writing the TTL, snapshot, and OpenFeature logic separately in each module would keep the existing spec unchanged but would place the highest-risk shared logic outside the only mechanism this repo has for preventing drift.

### D6. `Gate` is deleted

`natsGate`, `wsGate`, and `propEnabledGate` are the only users of `flags.Gate`, and all three are replaced by `Resolver`. `Gate`, `NewGate`, and `ResetForTest` are removed rather than left as dead code. The package is `internal/`, so nothing consumer-visible is removed.

### D7. Strategy selection keys on the whole static part of the decision

```
useTracedImpl = gate1 && EnvEnabled(moduleEnv)
```

Both terms are environment-derived and fixed at construction; only the relay verdict is dynamic, and it is excluded. This is the same expression `otel-gorilla-ws` uses for its negotiation capability (D9), so all four modules decide construction identically.

No option branch is needed, because D3 makes `gate1` the single expression of that tier: the option, when present, *is* `gate1`. This removes the most error-prone part of the previous design, in which implementation selection had to special-case an option that could disagree with the environment.

The kill-switch model is what makes including `moduleEnv` safe. Under a relay that could enable, the instrumented implementation had to be built even when the module switch was off, because a later relay value might need it — that constraint is why the earlier revision keyed construction on `gate1` alone. Under D2, `EnvEnabled(moduleEnv) == false` makes `tracing` false permanently: no relay value can raise it, so the instrumented implementation can never be reached and there is no reason to allocate it.

When `useTracedImpl` is false, only the passthrough implementation is constructed and no OTel SDK code path is reachable for that wrapper's lifetime. When it is true, both implementations exist and the per-operation relay verdict selects between them.

*Consequence.* The previous release's zero-cost passthrough is preserved for every configuration that had it, including `gate1` on with the module switch off — a configuration the earlier revision of this design would have slowed down. The cost of dynamism is paid only where dynamism is possible: a wrapper built on the traced path pays one atomic load and one clock read per operation, amortized across the TTL window.

*Consequence.* Changing a module's environment variable after construction (`os.Setenv` in a long-running process) does not change which implementations exist. This already held for `gate1` and for `otel-gorilla-ws`'s capability; tests must set the environment before constructing, which is the discipline they already follow.

### D8. Long-lived objects consult the flags per call; no connection is ever static

Because the relay must be able to revoke on a running process, **no wrapper may cache its tracing decision**. This holds even for connections constructed with `WithTracingEnabled`: that option fixes `gate1`, not the module tier, so such a connection still reads `EnvEnabled(moduleEnv) && resolver.Allowed(...)` on every operation and still stops when the relay revokes.

The cached-gate modules convert cheaply: `if !c.tracingEnabled` becomes `if !c.tracingEnabled()`. The strategy-split types need a structural change — each facade type holds **both** implementations and selects per call:

```go
type Collection struct {
    direct *direct.Collection
    traced *traced.Collection
    // ...
}

func (c *Collection) impl() collectionImpl {
    if c.traced != nil && c.tracing() {
        return c.traced
    }
    return c.direct
}
```

This keeps `internal/direct` free of OTel SDK imports (the CI grep and the package boundary are both unchanged) and keeps `internal/traced` free of gating code. The alternative — a runtime gate at the top of every `traced` method — would duplicate the entire `direct` package inside `traced` and defeat the split.

`otelnats.Conn` needs the same treatment. Contrary to the pattern table this design originally carried, `otelnats.Conn` was already a strategy split (`connImpl` = `directConn` / `tracedConn`, chosen once at construction), not a cached gate, so it gains a `direct`/`traced` pair and an `impl()` selector rather than a field-to-method rename. `oteljetstream` wrappers, consumers, `MessagesContext` and `MessageBatch` forwarders derive their gate from the `Conn` and re-read it per message.

`Cursor` and `ChangeStream` follow the same dual-implementation shape rather than inheriting a fixed choice from the call that produced them. This matters most for `ChangeStream`, which can outlive many revocations. Both are structurally able to: `traced.Cursor` and `traced.ChangeStream` hold only a tracer, a propagator and their propagation flag — no per-call span state — so the facade can build an instrumented and a passthrough wrapper around the same raw driver object.

`SingleResult` is the one exception, and it is forced rather than chosen. `traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`. A `FindOne` that ran through the passthrough path started no span, so there is nothing to construct an instrumented wrapper around. Selecting per call would also be incoherent in the other direction: a revocation between `FindOne` and `Decode` would strand an already-started span that the passthrough path would never end. `SingleResult` is the result of one already-executed operation, so the flag value at `FindOne` time is the only meaningful answer — unlike a cursor or change stream, which keep producing new work.

*Consequence.* `collectionImpl` returns raw driver types for `Find`/`Aggregate`/`Watch` and the facade constructs both wrappers itself; `FindOne` keeps returning a `shared.SingleResultImpl` alongside its raw result. `traced.Collection`'s exported `PropagationEnabled bool` field is a `func() bool`, which facade-package tests that build `traced.Collection` literals must follow.

**Per-operation tracing/propagation consistency (R5).** Within a single public operation, the tracing decision used to select `impl()` SHALL be the same value passed into `resolveDocumentPropagation` / `propagationOn` — callers MUST NOT re-resolve tracing for that operation's propagation decision. The operation's two relay verdicts SHALL come from one `AllowedAll` load (D4). Client and Database share one small gate-state helper so the rule is not hand-copied four times (R16).

**Facade `impl()` selection (R14).** The `if traced != nil && tracing() { return traced }; return direct` pattern is factored once per module (v1 and v2) via a tiny generics helper; v1↔v2 parity copies remain required by CLAUDE.md.

### D9. `otel-gorilla-ws` negotiates `otel-ws` on the static portion of the decision

Negotiation happens during the handshake and cannot be revisited, so it is gated on everything that cannot change afterwards, and only on that:

```
negotiationCapability = gate1 && EnvEnabled(OTEL_GORILLA_WS_TRACING_ENABLED)
```

The relay verdict is excluded because it can flip a second later — but, under D2, excluding it costs nothing, because a relay can only revoke. A connection whose module environment variable is off at handshake time can never be switched on by any later relay value, so there is no future state in which it would need the envelope. This is the direct benefit of the kill-switch model: capability is now a fully static expression, and the previous design's R4 exception — "upgrading without a provider changes the wire between library peers when the global switch is on and the module switch is off" — **no longer exists**. With no provider installed, `otel-gorilla-ws` reproduces the previous release's wire behavior exactly.

The read path is safe either way: `tryUnmarshalWire` probes and falls back to the raw payload when the message is not an envelope. Only the write path must match what the peer agreed to, because sending an envelope to a peer that did not negotiate `otel-ws` hands `{"header":...,"data":...}` to that peer's application code.

**Envelope follows negotiation outcome, not feature-on aspiration (R1).** `Conn.tracingEnabled` means "otel-ws was negotiated (or proven via subprotocol)", not "this process might want spans".

- `Dial` / `Upgrader.Upgrade` set `tracingEnabled` from the handshake result.
- `NewConn` has no handshake: it sets `tracingEnabled` from `isOTelWireProtocol(conn.Subprotocol())`. Callers that manage the handshake themselves must leave a correct negotiated subprotocol on the raw conn. There is **no** `WithOTelWSNegotiated` escape hatch — that would reintroduce force-envelope wire corruption.
- When negotiation failed or is unproven the wire is raw passthrough; if capability and the per-call gate are on, local send/receive spans may still be created without inject/extract.
- `configureConn` clamps `tracingEnabled = tracingEnabled && capable` so a historical `true` cannot outlive a capability-off process (R7).
- A relay revocation on a negotiated connection stops spans and stops injection; the envelope keeps being written with an empty header, because the peer parses every frame as one.

*Consequence.* Two peers that both run this library with `gate1` on **and** `OTEL_GORILLA_WS_TRACING_ENABLED` truthy exchange the JSON envelope on every message, including while the relay has revoked tracing. That is a deliberate deployment choice by that site, not something a reader of the previous CHANGELOG would be surprised by.

### D10. Mongo document helpers carry no gate

`ContextFromDocument` and `ContextFromRawDocument` carry no feature-flag gate at all. They read a `_oteltrace` field out of a document the caller already holds, run `propagator.Extract` on it, and return the span context it encodes. They start no span, allocate no attributes, initialise nothing in the OTel SDK, and write nothing anywhere.

The flags exist to stop the library doing work **on the caller's behalf**. `Collection.InsertOne` is called for the business operation and gets instrumented as a side effect the caller never asked for at that call site; a kill switch is exactly right there. These two are called only when the caller wants trace extraction and for no other reason. Gating something whose sole purpose is the thing being gated leaves the caller no way to express what they already expressed by calling it.

The comparison that settles it is with `Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace`, which look superficially similar and **are** gated:

| | emits telemetry | writes to the document | gated |
|---|---|---|---|
| `Collection.InsertOne` and siblings | CLIENT span | `_oteltrace` | yes |
| `Cursor.DecodeAndTrace` | `mongo.cursor.decode` span | no | yes |
| `ContextFromDocument` | no | no | **no** |
| `ContextFromRawDocument` | no | no | **no** |

`DecodeAndTrace` starts and ends a real span on every call, so it belongs under the switch. The package-level pair does not, so it does not. An earlier revision of this design had them following the relay on the grounds that two code paths in one change-stream loop should not obey different rules; that argument assumed the two paths were the same kind of thing, and they are not.

*Consequence — the invariant is about gated paths.* The disabled-mode invariant's "no propagator inject/extract" clause is scoped to code the flags govern. `propagation` is OTel **API**, not SDK, so nothing in the compiler-enforced `internal/direct` boundary or the CI grep is weakened; the clause is restated in `CLAUDE.md` to say what it protects (no span, no SDK, no exporter, no attribute build, no injection) rather than to list mechanisms.

*Consequence — BREAKING.* A process with every switch off previously got a zero `SpanContext` and `false` from `ContextFromDocument`, and an unmodified `ctx` from `ContextFromRawDocument`. It now gets the document's real span context. The direction is more capability, not less, and only code that calls these functions is affected — but a deployment that switched an environment variable off specifically to stop trace linking must now stop calling them instead.

*Consequence — the option blind spot stops mattering.* Because there is no gate, there is nothing for the package-level pair to misread when a deployment supplies `gate1` through `WithTracingEnabled` rather than the environment variable. The mutual-exclusion rule in D3 no longer has a corner where choosing the option spelling silently disables a read path.

**Why the write side needs the kill-switch model most.** `_oteltrace` is not observability-only: it is roughly 90 bytes of BSON (more with a `tracestate`) appended to the document by `InsertOne`, `InsertMany`, `UpdateOne`/`UpdateMany`, `UpdateByID`, `ReplaceOne` and `BulkWrite`. Nothing in this module ever removes it — there is no strip on read, so once written the field is visible to the application on every subsequent read, permanently. Turning the flag back off does not undo anything; cleanup is a `$unset` migration. Under a relay that could enable, a remote configuration change could start failing every write against a collection with `$jsonSchema` + `additionalProperties: false`, or silently add gigabytes to a large collection. D2 removes that possibility entirely: `OTEL_MONGO_PROPAGATION_ENABLED` (or `WithTracePropagationEnabled`) must be set by the deployment before a single byte is written, and the relay's only power is to stop it.

### D11. Flag keys are fixed, kebab-case, and module-scoped

| Flag key | Paired environment variable | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

The environment variable is **not** the flag's evaluation default (D2); it is a separate, ANDed tier. There is no key for `gate1` — D3 makes it environment-and-option-only.

*Alternatives considered.* Reusing the environment variable strings as flag keys would give operators one identifier instead of two, but `UPPER_SNAKE` is foreign to GO Feature Flag configuration and welds the two namespaces together. Making keys overridable through additional environment variables would let a site match an in-house naming convention, but the relay configuration is written by that same site — naming a flag `otel-mongo-tracing` there costs nothing.

### D12. Evaluation context is the application's

The library passes an empty `openfeature.EvaluationContext{}`. Applications that want targeting install a global one with `openfeature.SetEvaluationContext`, which the SDK merges into every evaluation.

The library has no non-arbitrary source for `service.name` or `deployment.environment` — it would have to guess between `OTEL_SERVICE_NAME`, the OTel resource, and hostname — and anything it invented would collide with what the application set. Per D4 the snapshot is process-wide, so only process-scoped attributes are meaningful anyway.

### D13. Testing uses an in-memory provider and an injected clock

`NewResolver` accepts `WithClock(func() time.Time)`, exported for tests in the same way `Gate.ResetForTest` was. Tests install `memprovider.NewInMemoryProvider(...)` through `SetProviderAndWait`, mutate flag values, advance the fake clock past the TTL, and assert the new value is observed. This makes TTL behavior itself testable — that 0.9 s does not refresh and 1.1 s does — which a reset hook could only bypass.

Tests must exercise the kill-switch asymmetry explicitly: a relay `true` against a falsy module environment variable must produce **no** spans and **no** evaluation, and a relay `false` against a truthy one must stop a running connection.

Because D3 makes the environment variable and the option mutually exclusive, every existing test that sets `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` *and* passes `WithTracingEnabled` must be rewritten to use exactly one of them. The same applies to `OTEL_MONGO_PROPAGATION_ENABLED` and `WithTracePropagationEnabled`.

One integration test stands up a real GO Feature Flag relay proxy container and drives one module end to end, verifying that the wiring recipe in the documentation actually resolves against a real relay: provider construction options, endpoint format, and flag keys matching a real relay configuration file. It must assert the revoke direction, since that is the only direction the relay has. Only one module is covered; the wiring is identical across the four and three more containers would add cost without information.

A full harness-level assertion that spans stop reaching the OTLP sink after a revocation is deliberately excluded. It would have to outwait the TTL, the provider's poll interval, and the exporter's batch timeout, making it a timing race; its two halves are already covered separately by the integration test (the value propagates) and the unit tests (a false value emits no span).

Because the OpenFeature provider and the environment are both process-global, tests that touch them must not call `t.Parallel` — the same constraint that already applies to the environment-toggling tests.

### D14. `EnvEnabled` recognises an explicit truthy allow-list

```go
switch strings.ToLower(strings.TrimSpace(v)) {
case "1", "true", "yes", "on":
    return true
default:
    return false
}
```

An unset variable is false, as before. What changes is the default branch: previously any set value that was not in a four-item falsy list counted as enabled, so `OTEL_MONGO_TRACING_ENABLED=` (empty string), `=enabled` and `=2` all turned tracing on. An empty string enabling a kill switch is the sharpest of these — `export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=` opened the gate.

The allow-list mirrors the existing falsy list one-for-one (`0`/`false`/`no`/`off`), so the documented rule is symmetric and short.

*Consequence — BREAKING.* Deployments that enable a switch with any value outside the allow-list silently flip to disabled on upgrade. The direction is fail-safe (less instrumentation, never more), but it presents as "spans disappeared after upgrading" and must be called out in every CHANGELOG.

### D15. Conflicting configuration fails at construction, with named errors

Each module exports a sentinel:

```go
var ErrTracingConfigConflict = errors.New("otelnats: WithTracingEnabled and OTEL_INSTRUMENTATION_GO_TRACING_ENABLED are mutually exclusive; set exactly one")
```

and `otel-mongo` (v1 and v2) additionally exports `ErrTracePropagationConfigConflict` for the `WithTracePropagationEnabled` / `OTEL_MONGO_PROPAGATION_ENABLED` pair. `internal/flags` is `internal/` and cannot export a type consumers can match on, so each module owns its own value; the shared file owns only the `EnvSet` predicate they all use.

Returned errors wrap the sentinel and name both observed values (`option=false, OTEL_INSTRUMENTATION_GO_TRACING_ENABLED="1"`), because a configuration conflict is only actionable once you know which two settings disagree.

`otel-mongo` is the only module with two independent checks, and a caller can violate both — one configuration file setting every environment variable, one code path passing every option. Both checks therefore run before either error is returned, and the results are combined with `errors.Join` in a fixed order (tracing first, propagation second). `errors.Is` matches either sentinel through the join, and tests can assert on the order. Returning the first failure instead would make the caller fix one conflict only to rediscover the other on the next run, which is the failure mode configuration errors are worst at.

### D16. `otelgorillaws.NewConn` returns an error

`NewConn(conn *websocket.Conn, opts ...Option) *Conn` becomes `(*Conn, error)`. It is the only option-accepting constructor in the repository that cannot report a failure, and D15 gives it one to report.

*Alternatives considered.* Adding a second constructor (`NewConnWithOptions`) or renaming to `WrapConn` and deprecating `NewConn` keeps existing call sites compiling, but leaves the conflict undetectable on the very entry point most likely to be misconfigured — `NewConn` is the path for callers who run their own handshake. Deferring the error to the first `WriteMessage`/`ReadMessage` avoids the signature change but turns a construction-time configuration mistake into a runtime error that a never-used connection never reports, and that callers must `errors.Is` to distinguish from a network failure. Exempting `NewConn` from D15 leaves the rule with a hole where it is most needed.

*Consequence — BREAKING.* Every `NewConn` call site must change. In this repository that is four test call sites; the one known downstream consumer (`instrumentation-demo`) does not use `otel-gorilla-ws` at all.

## Risks / Trade-offs

**Tracing cannot be enabled remotely.** The relay is the wrong tool for "turn this on so I can see what is happening". → Stated in the README next to the wiring snippet, and in every CHANGELOG. Sites that want relay control deploy with the module switch on and use the relay as a brake.

**Deployments that want relay control run with instrumentation on by default.** Under D2 the only relay-controllable state is "environment says on, relay may revoke", so the resting state is on. → This is the honest cost of never letting a remote party enable anything. Sites that want tracing off by default simply leave the module switch off and ignore the relay.

**`EnvEnabled` truthiness change silently disables some deployments.** Values like `enabled`, `2`, `y` and the empty string stop working. → BREAKING in all four CHANGELOGs, with the allow-list spelled out.

**Mutual exclusion invalidates existing configurations and 89 in-repo test call sites.** Anything that set the environment variable and passed the option now fails at construction. → BREAKING; the error message names both values. The one known downstream consumer passes no options.

**`NewConn` signature change.** → BREAKING; see D16.

**`refresh` runs on a caller's goroutine.** With the supported in-process provider this is a local lookup once per TTL window. With an unsupported remote-evaluation provider it is an HTTP request on the operation path. → Bounded by `refreshTimeout` (D4), with the timeout resolving to "relay does not interfere"; and the supported mode is stated in the README.

**Revocations are not atomic across modules.** Each module refreshes independently, so a relay change touching Mongo and NATS together can be observed by one before the other, up to one TTL apart. → Within a module `AllowedAll` gives one snapshot, which is where combinations actually interact.

**`_oteltrace` is written into application documents and never removed.** Roughly 90 bytes per document across six write methods, with no strip on read, no undo, and a hard write failure against strict `$jsonSchema` validation. → D2 and D10 mean only the deployment can start this; the relay can only stop it. Documented in the module README with the field shape, the write methods, the size, and the fact that cleanup is a `$unset` migration.

**Four `go.mod` files gain the OpenFeature SDK.** Consumers that never use the relay still resolve the dependency. → The SDK is small and dependency-light; the alternative designs that avoid it all require application-side wiring that this change explicitly set out to avoid.

## Migration Plan

1. Land all four modules in one commit — the byte-identical `internal/flags` copies cannot be split across commits.
2. Tag `otel-mongo/v0.9.0`, `otel-mongo/v2.9.0`, `otel-nats/v0.8.0`, `otel-gorilla-ws/v0.8.0`. Tags may be pushed sequentially; the release guard validates each against its version constant.
3. Existing deployments that upgrade **without** installing an OpenFeature provider keep their previous behavior, with two exceptions that must be checked before upgrading:
   - any switch set to a value outside the `1`/`true`/`yes`/`on` allow-list now reads as disabled (D14);
   - any code that sets `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` *and* passes `WithTracingEnabled` now fails at construction (D3/D15).

   Unlike the design that preceded this revision, there is **no** wire-format exception for `otel-gorilla-ws`: negotiation is gated on the static capability (D9), so peers see exactly the previous release's wire.
4. `otelgorillaws.NewConn` call sites must take the new error return (D16).
5. Deployments adopting the relay install a provider at startup, next to their existing `otelsetup.Init()` call, create the flags on the relay, and **deploy with the module switches on** — the relay can only revoke. Until a flag exists on the relay, the module runs at its deployed state.

**Rollback.** Pin the previous module version. There is no persisted state and no relay configuration that must be torn down — flags left on the relay are simply ignored by the older build. `_oteltrace` fields already written to documents are not removed by a rollback; that is a `$unset` migration either way.

## Superseded decisions

This design was revised after PR #27 shipped an implementation of an earlier model. The following are recorded so that reviewers reading the diff do not mistake the change for a regression:

| Earlier decision | Replaced by |
|---|---|
| The module env var is the OpenFeature evaluation default; the relay decides in both directions | D2 — the env var is a separate ANDed tier and the default is always `true` |
| `WithTracingEnabled` overrides the global switch in either direction | D3 — the two are mutually exclusive spellings of one tier |
| A connection carrying `WithTracingEnabled` is fully static and no relay change reaches it | D8 — no connection is static; the option fixes `gate1` only |
| `mongoPropagationEnvOnly()` serves static clients so the relay cannot reach a kill-switched process | Deleted — D2 makes every relay value a revocation, so there is nothing to protect against |
| `ContextFromDocument` / `ContextFromRawDocument` follow the relay, so one flag governs both halves of a change-stream loop | D10 — they are ungated; they emit no telemetry, unlike `DecodeAndTrace`, which does and stays gated |
| Implementation selection keys on the global switch alone, so `gate1` on + module switch off allocates an instrumented wrapper it can never use | D7 — construction keys on `gate1 && EnvEnabled(moduleEnv)`, restoring the zero-cost passthrough and matching `otel-gorilla-ws` |
| R4: "no provider ⇒ identical behavior" is false for otel-ws negotiation | Withdrawn — D9's capability is fully static, so the exception no longer arises |
| Detecting whether the relay "has an opinion" (double `Boolean` evaluation) | Unnecessary — under D2 "relay silent" and "relay allows" are the same outcome |

## Post-review remediation (PR #27 grill, 2026-08)

Source: `reviews/code-review-pr-27-openfeature-dynamic-flags.zh-TW.html` and a decision grill on each finding. Items unaffected by the revision above stand as decided.

| ID | Topic | Decision | Status after revision |
|----|--------|----------|----------------------|
| R1 | `NewConn` wire corruption when capability on + feature off | Envelope only if negotiated/proven; fail → raw wire; local spans OK; no force-negotiated option; clamp with capable (R7) | Stands (D9) |
| R2 | `MessageBatch` freezes flag at Fetch | Always return a dynamic batch wrapper; per-message gate re-check | Stands (D8) |
| R3 | `Resolver.refresh` last-store-wins + late `at` | Stamp `at` at evaluation start; no CAS/mutex | Stands (D4) |
| R4 | otel-ws negotiation vs "no provider ⇒ no change" | Was: keep behavior, document exception | **Withdrawn** — D9 removes the exception |
| R5 | Mongo single-call-chain torn read of tracing | Pass resolved tracing into propagation; no internal recompute | Stands, strengthened by `AllowedAll` (D4/D8) |
| R6 | JetStream per-message rebuild of tracer/attrs | Hoist tracer/prop/baseAttrs to construction; gate stays per-message | Stands |
| R7 | `capable` / `tracingEnabled` no choke-point clamp | Subsumed into R1 | Stands |
| R8 | Dead second return of `collectionImpl` Find/Aggregate/Watch | Drop second return; stop throwaway `New*` in impls | Stands |
| R9 | `tracedMessagesContext.Next` not gate-first | Gate-first delegate to `directMessagesContext` | Stands |
| R10 | Gate/propEnabledGate doc drift | Full sync: CLAUDE, test comments, jetstream godoc, main spec | Stands, widened by the revision |
| R11 | `WriteMessage` nil span + dual guards | Feature-off uses noop span; drop nil guards | Stands |
| R12 | NATS Consume path triple `impl()` per message | Resolve once per message; pass down | Stands |
| R13 | `dynamicTracingPossible` duplication / parallel refresh | `flags.GlobalTracingPossible()`; no parallel Boolean in refresh | Stands, plus `GlobalTracingSet` (D5) |
| R14 | Six copies of facade `impl()` selection | Generics `selectImpl` per mongo module | **Not yet implemented** — `collection.go`, `cursor.go` and `results.go` still hand-roll it |
| R15 | Five copies of relay test helpers | Move to `otel-testkit/harness` | Stands |
| R16 | Client/Database `effective*` duplication | Shared gateState | Stands |
| R17 | otelnats `impl`/`msgHandler`/`traceEventMsgHandler` policy | WONTFIX extract; lockstep comment only | Stands |
| R18 | Dead nil-handler guard in `tracedConsumeHandler` | Delete dead guard | Stands |
| R19 | Same-refresh sequential Boolean micro-torn pair | WONTFIX | Superseded — `AllowedAll` (D4) removes the cross-TTL half; the intra-refresh microsecond window remains WONTFIX |

## Open Questions

Two items are known-open and tracked in `tasks.md`, not blockers for the design:

1. **Read-modify-write may produce a duplicate `_oteltrace` field.** `InjectTraceIntoDocument` appends unconditionally, so a document read into a `bson.M`, modified and written back with `ReplaceOne` carries the field twice in the resulting `bson.D`. Needs a test to confirm the server's behavior; if confirmed, inject must remove any existing key before appending. Independent of the relay.
2. **`CLAUDE.md` claims `_oteltrace` is "stripped on read".** No such code exists in either module. The claim must be corrected wherever it appears.

Items deliberately excluded from this change stay Non-Goals: dynamic sampling rates, per-request targeting, a harness-level flag-flip E2E assertion, `Resolver` CAS/singleflight, parallel `Boolean` fan-out, and remote enablement of any switch.
