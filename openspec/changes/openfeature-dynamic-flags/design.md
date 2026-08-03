## Context

Every tracing and propagation switch in this repo is an environment variable read once per process. Three enforcement patterns consume those reads, and which one a type uses is decided by how much of its surface needs instrumenting:

- **Strategy split** (`otel-mongo` Collection / Cursor / SingleResult / ChangeStream; all of `otel-nats` — both `otelnats.Conn` and the `oteljetstream` wrappers). The flag decides *which object the caller talks to*: two types implement one interface, one pure delegation and one pure instrumentation, so no method body contains both paths. Chosen where nearly every method needs a span — `collectionImpl` is 15 methods, and the passthrough and instrumented implementations are 99 and 423 lines respectively. `otel-mongo` enforces the split across an `internal/direct` / `internal/traced` package boundary, so the disabled path is **compiler-enforced**: `internal/direct` imports no `go.opentelemetry.io/otel` package and CI greps to keep it that way. `otel-nats` keeps `directConn`/`tracedConn` and `directJSImpl`/`tracedJSImpl` in one package, so its equivalent is reviewer-enforced.
- **Cached gate** (`otel-gorilla-ws`). The flag decides *which branch inside a method runs*: the constructor resolves a `featureEnabled` bool onto the wrapper and each instrumented method opens with a gate check. Chosen because `Conn` embeds `*websocket.Conn` — that embedding is what gives callers the whole gorilla API for free, and only `WriteMessage` and `ReadMessage` need wrapping. Splitting two methods would cost an interface over roughly thirty.
- **Gate carrier** (`otel-mongo` Client / Database). Neither type instruments anything: `Disconnect`, `Ping`, `StartSession` and `Database.Collection` are pure delegation with no span. Their whole job is to resolve the flag state once and hand it down to the `Collection` / `Cursor` / `ChangeStream` wrappers that do the work. There is nothing to split and nothing to gate — only state to carry, which R16 factors into a single `gateState` helper.

`internal/flags` is vendored as four byte-identical copies with zero external dependencies. It exports `EnvEnabled` (default-off env read) and `Gate` (a `sync.Once` + `atomic.Bool` cache whose documented contract is that environment changes after the first read are ignored).

All three patterns and the `Gate` contract assume the answer never changes after startup. Making the switches dynamic means revisiting all of them: the split must re-select per operation, the gate must re-read per call, and the carrier must stop caching what it hands down. It also ends `internal/flags`'s zero-dependency property — a property worth naming, because a vendored four-copy file with no imports is trivially safe to duplicate. That trade is accepted in D5.

The OpenFeature Go SDK is at v1.17.2. Its relevant surface:

- `openfeature.SetProviderAndWait(p)` installs a process-global **default** provider; `SetNamedProvider(domain, p)` installs one scoped to a domain, which takes precedence over the default for clients bound to that domain.
- `openfeature.NewClient(domain)` returns a client that resolves through the named provider for `domain` when one exists and falls back to the default otherwise.
- `client.Boolean(ctx, key, defaultValue, evalCtx)` returns `defaultValue` on any error — including when no provider was ever installed, since the SDK's default is a no-op provider.
- `openfeature.ProviderMetadata().Name` reports the installed default provider's identity, and is `"NoopProvider"` exactly when nothing has been installed. This is a reliable "has the application configured a provider?" test, and D17 uses it.
- `Client.evaluate` merges evaluation contexts in the order *API (global) → transaction → client → invocation* (`client.go:695`), so an attribute passed at the invocation site composes with — and wins over — the application's global context without the library ever calling `SetEvaluationContext`.
- `openfeature/memprovider` provides an in-memory provider suitable for tests.

This document has been revised once. An earlier model — in which the relay decided in both directions and `WithTracingEnabled` pinned a connection static — was implemented and merged before design review replaced it, so the code in the tree does not yet match what follows. See § "Superseded decisions" for the point-by-point mapping and `tasks.md` § 9 for the remaining work.

## Goals / Non-Goals

**Goals:**

- An operator can **turn off** each module's tracing and propagation through a GO Feature Flag relay proxy without restarting the application.
- No remote party can turn anything **on**. Everything that is enabled was enabled by a reviewed deployment.
- Deployments that never configure an OpenFeature provider keep exactly their environment-driven behavior, modulo the `EnvEnabled` truthiness change in D14.
- Hot paths pay a bounded, predictable cost with no network call, and the per-operation cost of resolving a flag is measured and recorded rather than assumed (D4).
- The compiler-enforced disabled path survives. `internal/direct` still imports no `go.opentelemetry.io/otel` package, CI still greps for it, and a process whose switches are off still cannot reach OTel code.
- A configuration that expresses two different intents for the same switch fails loudly at construction rather than silently picking one.
- An application can obtain relay control **without writing any Go code**, by setting environment variables alone. Three new process-scoped variables serve that (`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` / `_API_KEY` / `_POLL_INTERVAL`); no new module-scoped variable is added.
- New exported API is limited to what the mutual-exclusion rule forces — one error sentinel per module (two in `otel-mongo`) and one changed constructor signature — plus two additive `otel-gorilla-ws` symbols that make D9's "run your own handshake" instruction followable.

**Non-Goals:**

- Remotely enabling tracing. This is the deliberate inverse of the usual feature-flag posture; see D2.
- Supporting the GO Feature Flag provider's remote evaluation mode, which would put an HTTP request on the operation path; see D4.
- Per-request flag targeting (per tenant, per user). The resolver holds no per-request state and passes a process-scoped evaluation context; request-scoped attributes cannot influence it.
- Dynamic sampling rates. `otel-sampler` is untouched.
- Changing span shapes, attributes, semantic conventions, or business logic anywhere.
- Owning the OpenFeature **default** provider, the **global** evaluation context, or provider shutdown. The library installs a **named** provider on its own domain when the environment asks for one and none exists (D17); everything outside that domain stays the application's.

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
tracing     := useTracedImpl && resolver.Allowed(idxTracing)
propagation := tracing && gateProp && resolver.Allowed(idxPropagation)     // otel-mongo only
```

Three tiers, three owners, three distinct powers:

| Tier | Owner | When it is off | Can it be changed without a redeploy? |
|---|---|---|---|
| `gate1` — `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **or** `WithTracingEnabled`, never both | deployer, or the caller that constructs the wrapper | Every module in the process is off, only passthrough implementations are allocated, and no OpenFeature code path is reachable | No |
| `OTEL_<MODULE>_TRACING_ENABLED` (and `OTEL_MONGO_PROPAGATION_ENABLED`) | deployer | That module is off, only its passthrough implementation is allocated, and its resolver is never consulted | No |
| relay flag `otel-<module>-tracing` (and `otel-mongo-propagation`) | operator | That module stops emitting on a running process as soon as the provider observes it | **Yes — this is the only tier that can** |

The first two tiers are conjunctive and interchangeable in effect; they differ in scope (whole process vs one module) and in who owns them. The third is the only dynamic one, and it can only subtract from what the first two allow.

## Decisions

### D1. The application owns the default provider; the library reads through its own domain

`internal/flags` calls `openfeature.NewClient(FlagDomain)` and never `SetProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown`. The one thing it may install is a **named** provider bound to `FlagDomain`, and only under D17's conditions. The client itself is created lazily on the first evaluation rather than in `NewResolver`, so a process whose switches are off never initializes any part of the OpenFeature SDK.

The boundary this draws is narrower than "never touch the SDK's global state", and it is the load-bearing one: nothing the library does can change how the **application's own** feature flags resolve. A named provider on `otel-instrumentation-go` is invisible to `NewClient("")` and to every other domain.

This still mirrors the rule this repo applies to tracing — packages never initialize a `TracerProvider` — in the part that matters. A `TracerProvider` decides where the application's telemetry goes; the analogous blast radius here is the default provider, and that stays untouched.

*Alternatives considered.* Leaving provider construction entirely to the application, the shape this design carried until D17, keeps the library free of any global mutation at all. It was replaced because it makes relay control cost a code change in every consuming application, and the three objections that originally justified it have since been answered: the triple-poll problem is removed by a shared domain and a `NoopProvider` check (D17), the collision problem by using a named rather than default provider, and "the SDK offers no reliable way to ask whether the installed provider is still the no-op default" was simply **false** — `openfeature.ProviderMetadata().Name` answers it exactly.

*Consequence — the domain is now an isolation boundary in one direction.* Once D17 has installed a named provider on `FlagDomain`, another library in the same binary that installs a global provider (breaking the rule we follow) can no longer decide our flags. Until then — and in every process that does not set the endpoint variable — `NewClient(FlagDomain)` falls back to the default provider, so the old exposure remains. Under D2 the worst such a provider can do is revoke, so the failure is in the safe direction either way.

*Consequence — per-module providers are no longer available.* All four modules share one domain (D17), so an application cannot point `otel-mongo` at a different relay from `otel-nats`. The previous design's per-module `SetNamedProvider("otel-mongo", p)` hook is given up; nothing asked for it, and a single domain is what makes one provider instance serve all four without leaking a poller goroutine per module.

*Consequence — two provider settings are load-bearing.* The design's guarantee that a relay outage cannot affect the application holds only if the application configures the provider correctly, and two defaults work against it.

`DataCollectorDisabled: true` is required. The provider's data collector appends one event per evaluation to an in-memory buffer and flushes it on a two-minute ticker. A failed flush does not clear the buffer, and once the buffer reaches its 100,000-event cap **every subsequent append flushes synchronously, on the evaluating goroutine, holding the buffer's mutex**. With the relay down that flush fails after the HTTP client's 10 s timeout and the buffer never drains, so each evaluation repeats it while every other evaluating goroutine queues on the same mutex — turning a relay outage into stalled Mongo queries and NATS publishes. D4's decision to evaluate per operation makes the buffer fill proportionally to traffic, so this is reachable rather than theoretical. Nothing is lost by disabling it: the collector feeds the relay's evaluation-analytics dashboards, and for process-wide flags evaluated once per operation those analytics are a copy of the traffic volume.

An install failure must not abort startup. If the relay is unreachable at boot, the provider's first fetch fails and `SetProviderAndWait` returns an error; the documented handling is to log and continue, because the relay is a brake and not a prerequisite. The cost is unavoidable and stated rather than hidden: a process that starts while the relay is down cannot learn about an active revocation, so it comes up at the state its environment declares.

For the provider D17 installs, the first is **enforced in code**: `DataCollectorDisabled: true` and `EvaluationType: INPROCESS` are hardcoded and deliberately not exposed as environment variables, so the zero-code path cannot be misconfigured into either failure. For an application that installs its own provider the library can enforce nothing, so both remain stated as requirements in `feature-flags.md` rather than as suggestions, with the failure mode spelled out.

*Consequence — the provider must be ready before the first operation.* D2 makes an unresolvable flag mean "do not interfere", which is fail-open with respect to the relay. That is correct in steady state and wrong for exactly one window: process startup. An application that installs its provider with `openfeature.SetProvider` gets a non-blocking install, so between that call and the provider's first successful fetch every flag resolves to `true`. A module whose environment variable is truthy — which it must be for the relay to control it at all — is therefore **on** during that window, even while the relay is revoking it.

The scenario that makes this matter is the ordinary one: an operator revokes a module to stop an incident, and the process restarts for an unrelated reason. Under the superseded design a not-ready provider fell back to the environment, which for a deployment expecting relay control was usually off, so it failed closed. Under this one it falls back to allow.

**Blocking on readiness is the application's call, not a requirement of this design.** D17 installs non-blocking, deliberately: the alternative is to make the first instrumented operation of the process wait on a relay round trip, and a brake must not become a latency source. The window that leaves open is bounded by one relay fetch, and an application that cannot accept it closes it itself — install a provider with `openfeature.SetProviderAndWait` before constructing any wrapper, and D17 stands down (its trigger requires `ProviderMetadata().Name == "NoopProvider"`). The capability is not lost; it moves to the party that can decide whether the window matters.

What the window costs is stated rather than hidden, because for `otel-mongo` it is not only spans: `_oteltrace` written during it is permanent (D10), and cleanup is a `$unset` migration. `feature-flags.md` states both the window and the way to close it.

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

### D4. No caching: the relay verdict is resolved on every operation

```go
type Resolver struct {
    keys []string   // OpenFeature flag keys, in Allowed-index order

    clientOnce sync.Once
    client     openfeature.IClient
    evalCtx    openfeature.EvaluationContext   // populated only when D17 installed (service.name)
}

func NewResolver(opts ...ResolverOption) *Resolver
func WithFlagKeys(keys ...string) ResolverOption

func (r *Resolver) Allowed(i int) bool {
    if i < 0 || i >= len(r.keys) {
        return false      // a mis-wired module degrades to disabled, not to a panic
    }
    return r.evaluator().Boolean(context.Background(), r.keys[i], true, r.evalCtx)
}
```

That is the whole resolver. There is no snapshot, no TTL, no clock, no refresh, and no timeout. `NewResolver` takes no domain: the domain is process-scoped rather than module-scoped, so D5 makes it a constant in the byte-identical file and removes a string that would otherwise have to agree across five places with nothing checking it.

`evaluator()` is where the lazy `NewClient` lives, and D17 hangs the environment-driven provider install on the same `sync.Once`.

**What this costs, measured.** On an in-memory provider — the same shape as the GO Feature Flag in-process provider this design supports — one `client.Boolean` call is **2.0 µs, 336 B and 7 allocations**, against **82 ns and 0 allocations** for an atomic-pointer snapshot read. The 2 µs is not the flag lookup; it is the SDK's evaluation pipeline around it: before/after/finally hook chains, evaluation-context merging, the provider registry's RWMutex, interface dispatch. The provider being in memory does not help with any of it.

This is a real, known regression on the instrumented path. A wrapper that emits a span already pays roughly 1–3 µs for it, so resolving the flag per operation roughly doubles the instrumentation overhead of a NATS publish. Against a Mongo round trip it is noise. Against `0.7.0`, where the equivalent read was a plain struct field, it is 2 µs and 7 allocations that did not exist.

**Why it is accepted anyway.** Caching is a **pure internal optimisation behind an unchanged signature**. `Allowed(i) bool` reads identically whether the value came from a live evaluation or from a cached snapshot, so adding a cache later costs nothing at any call site. Deferring it is therefore not a bet — it can be added the moment a benchmark on a real workload says it matters, without touching a single module.

What deferring buys is the removal of an entire class of concurrency subtleties from the file that must stay byte-identical across four copies, and that the package doc names as its highest drift risk:

- no TTL semantics to specify, document or test (no 900 ms / 1100 ms boundary tests, no injectable clock)
- no snapshot timestamp question — whether `at` is stamped before or after the evaluation loop (R3) simply does not arise
- no cross-flag consistency question: with no TTL there is no TTL boundary for a two-flag read to straddle, leaving only the microsecond window between two consecutive `Boolean` calls, which R19 already accepted as WONTFIX
- no shared mutable snapshot slice to protect from callers
- no refresh timeout, and therefore no magic number defending a configuration the design already declares unsupported
- one less term in the revocation-latency budget

**What the latency budget actually is.** The removed TTL was never the dominant term. A revocation becomes visible when the provider's background poll picks it up, and that interval is **60 seconds** by default under D17 (the GO Feature Flag provider's own default is 120 s, and `interval <= 0` falls back to it — `evaluator/inprocess.go:186`). Against that, the deleted one-second TTL was under 2% of the end-to-end delay. It is listed above as a simplification, not as a latency win, and this correction matters twice over: `feature-flags.md` must not describe revocation as immediate, and the bar for adding a cache back is lower than this section originally implied — a one-second TTL on top of a 60-second poll moves the worst case from 60 s to 61 s.

**The evaluation runs on the caller's goroutine.** With the supported in-process provider that is a local, allocation-heavy but bounded computation. With the unsupported remote evaluation mode it would be a synchronous HTTP request on the path of every Mongo query and every NATS publish — which is the reason that mode is unsupported rather than merely discouraged.

*Alternatives considered.* A per-module snapshot behind an `atomic.Pointer` with a one-second TTL — the shape this design carried through PR #27 — is 25× faster and allocation-free, and was the right answer while the module environment variable was the evaluation default, because back then the resolver was consulted even for modules that were switched off. Under D2 and D7 it is consulted only by wrappers that are actively instrumenting, which narrows the population that pays. It was deferred rather than rejected: see *Consequence* below. A background ticker goroutine writing a cached value removes the cost entirely but adds a permanently resident goroutine per module and a shutdown story this repo has no API for.

*Consequence — this is a deferral, not a decision that caching is unnecessary.* The measured numbers above stand. If a benchmark on a real workload shows the per-operation cost matters, the fix is a cache inside `Resolver` with no change to `Allowed`, and the design questions it reopens (TTL length, timestamp placement, multi-flag consistency, snapshot immutability) are recorded in this section so they do not have to be rediscovered.

### D5. Module-specific data lives outside the byte-identical file

`internal/flags` must stay byte-identical across four copies, so it cannot name **module** flag keys or **module** environment variables. `Resolver` receives the OpenFeature keys through `WithFlagKeys`; each module's own `env_flags.go` — which is not shared — supplies them, and owns the paired environment variable outright. The resolver never learns that a pairing exists, which is what keeps the shared file free of module vocabulary.

**Shared global kill-switch helper.** The process-wide name `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is the one env string every module already hard-codes. `internal/flags` SHALL export:

```go
const (
    EnvGlobalTracing = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"

    // D17: provider auto-install. Process-scoped, like the kill switch above.
    EnvFlagsEndpoint     = "OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT"
    EnvFlagsAPIKey       = "OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY"
    EnvFlagsPollInterval = "OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL"

    // D12: the targeting attribute source, defined by the OTel specification.
    EnvServiceName = "OTEL_SERVICE_NAME"

    // The one OpenFeature domain all four modules resolve through. Exported
    // because module-package tests install their in-memory provider on it.
    FlagDomain = "otel-instrumentation-go"
)

func GlobalTracingPossible() bool { return EnvEnabled(EnvGlobalTracing) }
func GlobalTracingSet() bool      { return EnvSet(EnvGlobalTracing) }
```

The rule is that the shared file may not name anything **module**-scoped. Every name above is process-scoped — one kill switch, one provider, one domain for the whole binary — so it belongs here for the same reason `EnvGlobalTracing` does, and putting it here is what keeps five copies of the domain string from having to agree with nothing checking them.

Module-specific godoc (especially D9's otel-ws negotiation rationale) lives at the Dial/Upgrade/capability call sites, not on the shared one-liners.

```go
// otel-mongo/otelmongo/env_flags.go
const (
    idxTracing = iota
    idxPropagation
)

var mongoResolver = flags.NewResolver(
    flags.WithFlagKeys(
        "otel-mongo-tracing",      // paired with envMongoTracingEnabled, ANDed by this package
        "otel-mongo-propagation",  // paired with envMongoPropagationEnabled, ANDed by this package
    ),
)
```

The evaluation call, the truthiness rules and the provider install — the parts most likely to drift if hand-copied — stay inside the byte-identical file. The zero-dependency property that file used to have is given up here, and D17 gives up more of it than the OpenFeature SDK alone: the file also imports the GO Feature Flag provider and `log/slog`. That concentration is the argument for sharing the file, not against it — the alternative is four hand-maintained copies of a provider construction that must agree on two hardcoded options.

**The byte-identical rule is maintained by code review, not by a check.** There is no CI step comparing the four copies; the package doc comment currently claims otherwise and is corrected as part of this change. All four are identical today, so the discipline has held, but it is worth naming what a single drifted copy would do, because this change concentrates more load-bearing logic in that file than it previously held:

| Drifted line | Silent consequence |
|---|---|
| the `true` evaluation default reverting to `EnvEnabled(...)` | that module's relay can **enable** again — the property the whole design rests on, gone for one module, in a diff that looks like the previous release |
| the truthiness allow-list | that module accepts `enabled` or an empty string where the others do not |
| `EnvSet` | the mutual-exclusion check misfires or fails to fire for that module |
| lazy client construction | "switches off ⇒ the OpenFeature SDK is never touched" stops holding for that module |
| `FlagDomain` | that module resolves through a domain nobody installed a provider on, so it falls back to the default provider and the relay's revocations never reach it — the kill switch is dead for one module and no test goes red |
| `EnvFlagsEndpoint` | that module never auto-installs, so in a zero-code deployment it is the only one the relay cannot revoke |

A CI step comparing the copies' hashes, alongside the existing "Verify direct/ has no OTel SDK imports" grep that protects an invariant of exactly this shape, would turn the rule into a check. It is deliberately out of scope here — this change's mandate is the kill switch, not CI infrastructure — and is recorded so the option is not lost.

*Alternatives considered.* Leaving `internal/flags` untouched and writing the OpenFeature call and the truthiness rules separately in each module would keep the existing spec unchanged but would place the highest-risk shared logic outside the only mechanism this repo has for preventing drift — and that risk grows, not shrinks, if a cache is added later (D4).

### D6. `Gate` is deleted

`natsGate`, `wsGate`, and `propEnabledGate` are the only users of `flags.Gate` — four call sites, not three, because `otel-mongo` v1 and v2 each carry their own `propEnabledGate` in their own `env_flags.go`. All four are replaced by `Resolver`. `Gate`, `NewGate`, and `ResetForTest` are removed rather than left as dead code. The package is `internal/`, so nothing consumer-visible is removed.

### D7. Strategy selection keys on the whole static part of the decision

```
useTracedImpl = gate1 && EnvEnabled(moduleEnv)
```

Both terms are environment-derived and fixed at construction; only the relay verdict is dynamic, and it is excluded. This is the same expression `otel-gorilla-ws` uses for its negotiation capability (D9), so all four modules decide construction identically.

No option branch is needed, because D3 makes `gate1` the single expression of that tier: the option, when present, *is* `gate1`. This removes the most error-prone part of the previous design, in which implementation selection had to special-case an option that could disagree with the environment.

The kill-switch model is what makes including `moduleEnv` safe. Under a relay that could enable, the instrumented implementation had to be built even when the module switch was off, because a later relay value might need it — that constraint is why the earlier revision keyed construction on `gate1` alone. Under D2, `EnvEnabled(moduleEnv) == false` makes `tracing` false permanently: no relay value can raise it, so the instrumented implementation can never be reached and there is no reason to allocate it.

When `useTracedImpl` is false, only the passthrough implementation is constructed and no OTel SDK code path is reachable for that wrapper's lifetime. When it is true, both implementations exist and the per-operation relay verdict selects between them.

**Construction fixes the ceiling; the relay fixes the current state.** This is not in tension with D8's rule that no connection is ever static. D7 answers "could this wrapper ever trace?", which only the two environment-derived terms can decide and which therefore cannot change. D8 answers "is it tracing on this operation?", which the relay decides and which changes freely. Because the relay can only lower the answer and never raise it, deriving the ceiling from the static terms loses nothing.

**Everything derived from a wrapper inherits its decision.** `oteljetstream.New(conn)`, `Client.Database()` and `Database.Collection()` are called after their source was constructed, and they SHALL inherit its implementations rather than re-resolving. Re-resolving would let a passthrough-only `Conn` hand out an instrumented JetStream wrapper that has no tracer to use.

*Consequence.* The previous release's zero-cost passthrough is preserved for every configuration that had it, including `gate1` on with the module switch off — a configuration the earlier revision of this design would have slowed down. The cost of dynamism is paid only where dynamism is possible: a wrapper built on the traced path resolves its relay verdict per operation, at the cost measured in D4.

*Consequence.* `otel-mongo` registers `shared.NewCommandMonitor` on the same condition. That monitor runs on **every** MongoDB command, capturing the real server address out of `CommandStartedEvent.ConnectionID` into a context-scoped holder, and exists only to correct `server.address` on a span. Under the earlier revision a process with `gate1` on and the module switch off registered it and paid it per command while emitting no spans at all; under D7 it is not registered, so that cost disappears with the rest.

*Consequence.* Changing a module's environment variable after construction (`os.Setenv` in a long-running process) does not change which implementations exist. This already held for `gate1` and for `otel-gorilla-ws`'s capability; tests must set the environment before constructing, which is the discipline they already follow.

### D8. Long-lived objects consult the flags per call; no connection is ever static

Because the relay must be able to revoke on a running process, **no wrapper may cache the relay's verdict**. This holds even for connections constructed with `WithTracingEnabled`: that option supplies `gate1`, which D7 folds into the construction-time decision along with the module environment variable, but it says nothing about the relay. Such a connection still calls `resolver.Allowed(...)` on every operation and still stops when the relay revokes.

**No environment variable is read on any hot path.** D7 fixes both environment-derived terms at construction, so what remains per operation is one relay verdict and nothing else — `tracedBuilt && Allowed(idx)` for the Mongo and NATS wrappers, `capable && Allowed(idx)` for `otelgorillaws.Conn`. Every `os.LookupEnv` in the flag path happens once, during construction. This is worth stating as an invariant because it is cheap to check and easy to lose: a future change that re-reads a module switch inside a gate would reintroduce a per-operation syscall-shaped cost without changing any behaviour, so nothing would fail.

`otel-gorilla-ws`, the one cached-gate module, converts cheaply: `if !c.featureEnabled` becomes `if !c.featureEnabled()`, where the method is `capable && wsResolver.Allowed(idxTracing)`. The strategy-split types need a structural change — each facade type holds **both** implementations and selects per call:

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

`otelnats.Conn` needs the same treatment. It has always been a strategy split (`connImpl` = `directConn` / `tracedConn`, chosen once at construction), so it gains a `direct`/`traced` pair and an `impl()` selector rather than a field-to-method rename. `oteljetstream` wrappers, consumers, `MessagesContext` and `MessageBatch` forwarders derive their gate from the `Conn` and re-read it per message.

`Cursor` and `ChangeStream` follow the same dual-implementation shape rather than inheriting a fixed choice from the call that produced them. This matters most for `ChangeStream`, which can outlive many revocations. Both are structurally able to: `traced.Cursor` and `traced.ChangeStream` hold only a tracer, a propagator and their propagation flag — no per-call span state — so the facade can build an instrumented and a passthrough wrapper around the same raw driver object.

`SingleResult` is the one exception, and it is forced rather than chosen. `traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`. A `FindOne` that ran through the passthrough path started no span, so there is nothing to construct an instrumented wrapper around. Selecting per call would also be incoherent in the other direction: a revocation between `FindOne` and `Decode` would strand an already-started span that the passthrough path would never end. `SingleResult` is the result of one already-executed operation, so the flag value at `FindOne` time is the only meaningful answer — unlike a cursor or change stream, which keep producing new work.

*Consequence.* `collectionImpl` returns raw driver types for `Find`/`Aggregate`/`Watch` and the facade constructs both wrappers itself; `FindOne` keeps returning a `shared.SingleResultImpl` alongside its raw result. `traced.Collection`'s exported `PropagationEnabled bool` field is a `func() bool`, which facade-package tests that build `traced.Collection` literals must follow.

**Per-operation tracing/propagation consistency (R5).** Within a single public operation, the tracing decision used to select `impl()` SHALL be the same value passed into `resolveDocumentPropagation` / `propagationOn` — callers MUST NOT re-resolve tracing for that operation's propagation decision. The two relay verdicts an `otel-mongo` operation needs are two consecutive `Boolean` calls; the microsecond window between them is the one R19 accepted as WONTFIX, and is fail-safe because a false tracing verdict short-circuits propagation. Client and Database share one small gate-state helper so the rule is not hand-copied four times (R16).

**Facade `impl()` selection (R14) — WONTFIX.** The six `impl()` methods (`Collection`, `Cursor`, `ChangeStream`, in v1 and v2) each hand-roll `if traced != nil && tracing() { return traced }; return direct`. R14 proposed factoring that through a generics helper, and this design previously recorded it as decided; it is now explicitly declined.

The three types return three different interfaces (`collectionImpl`, `shared.CursorImpl`, `shared.ChangeStreamImpl`) over three different concrete types, so the helper needs both an interface type parameter and a comparable concrete one just to express the nil check — a signature longer than the four lines it removes, sitting on the most-read entry point of the facade. R14 was raised when `impl()` still carried an option branch and a three-tier conjunction; D3 and D7 reduced it to four lines, and the duplication is no longer worth an abstraction. `c.tracing()` in that snippet is now the relay verdict alone, not a conjunction.

### D9. `otel-gorilla-ws` negotiates `otel-ws` on the static portion of the decision

Negotiation happens during the handshake and cannot be revisited, so it is gated on everything that cannot change afterwards, and only on that:

```
negotiationCapability = gate1 && EnvEnabled(OTEL_GORILLA_WS_TRACING_ENABLED)
```

The relay verdict is excluded because it can flip a second later — but, under D2, excluding it costs nothing, because a relay can only revoke. A connection whose module environment variable is off at handshake time can never be switched on by any later relay value, so there is no future state in which it would need the envelope. This is the direct benefit of the kill-switch model: capability is now a fully static expression, and the previous design's R4 exception — "upgrading without a provider changes the wire between library peers when the global switch is on and the module switch is off" — **no longer exists**. With no provider installed, `otel-gorilla-ws` reproduces the previous release's wire behavior exactly.

Only the write path must match what the peer agreed to, because sending an envelope to a peer that did not negotiate `otel-ws` hands `{"header":...,"data":...}` to that peer's application code. The read path probes with `tryUnmarshalWire`, which recognises the envelope and otherwise treats the frame as a legacy flat message or as an opaque payload.

**The probe is not byte-transparent, and must be made so.** `tryUnmarshalWire`'s legacy branch unmarshals any non-empty JSON object into a `map[string]json.RawMessage`, deletes `traceparent`/`tracestate`, and re-marshals (`message.go:76-101`). Go serialises maps with keys sorted, so an ordinary JSON payload carrying neither trace key still comes back with its fields reordered and its whitespace normalised — semantically identical, byte-wise different, and wrong for any caller that hashes or signature-verifies the frame. A message with neither key is by definition not a legacy envelope, so the branch SHALL return `ok=false` when both are absent, leaving the original bytes untouched. This is newly reachable because the R7 clamp (below) makes a capability-off peer write raw frames onto a negotiated connection, which is exactly the input that falls through to this branch.

*The envelope shape is reserved.* The envelope branch matches any object with a `header` of all-string values and a `data` member, so an application payload of that shape on an `otel-ws` connection is unwrapped and its outer structure discarded. Tightening the match (requiring `header` to contain only the two trace keys) is rejected: it would make any future header member added by the JS packages fail the match and fall into the legacy branch, which is worse. `otel-ws` is a negotiated protocol and `otel-ws.md` publishes the envelope, so `{"header":…,"data":…}` is a reserved wire structure — stated there, since that document currently does not say so.

**Envelope follows negotiation outcome, not feature-on aspiration (R1).** `Conn.tracingEnabled` means "otel-ws was negotiated (or proven via subprotocol)", not "this process might want spans".

- `Dial` / `Upgrader.Upgrade` set `tracingEnabled` from the handshake result.
- `NewConn` has no handshake: it sets `tracingEnabled` from `isOTelWireProtocol(conn.Subprotocol())`. Callers that manage the handshake themselves must leave a correct negotiated subprotocol on the raw conn. There is **no** `WithOTelWSNegotiated` escape hatch — that would reintroduce force-envelope wire corruption.
- That instruction has to be followable, and today it is not: the token (`otelWSProtocol`), the `otel-ws+<app>` composite form and the predicate (`isOTelWireProtocol`) are all unexported, so a caller running their own handshake can only hardcode strings that are internal details — while `otel-ws.md` already publishes them as a wire contract. Two additive symbols close that gap without reopening the escape hatch, because neither can force an envelope onto a peer that did not negotiate one:

  ```go
  // SubprotocolOTelWS is the subprotocol token this package negotiates.
  const SubprotocolOTelWS = "otel-ws"

  // IsOTelNegotiated reports whether NewConn will enable the envelope on conn.
  func IsOTelNegotiated(conn *websocket.Conn) bool
  ```

  The token lets a hand-rolled handshake be written correctly; the predicate lets it be verified rather than assumed. A stock `websocket.Dialer`/`Upgrader` can only reach the bare `otel-ws` form — gorilla echoes exact matches — so the `otel-ws+<app>` composite remains exclusive to `Upgrader.Upgrade`; documented alongside the constant.
- When negotiation failed or is unproven the wire is raw passthrough; if capability and the per-call gate are on, local send/receive spans may still be created without inject/extract.
- **The R7 clamp applies to the write path only.** `configureConn` clamps the *write* decision with `capable`, so a capability-off process never emits an envelope and a historical `true` cannot outlive it. It must **not** clamp the read path. Whether the peer envelopes is a fact established by the handshake; our gate is a local policy, and applying policy to the fact is what produced the defect: on a connection that proved `otel-ws` with `capable` false, `ReadMessage`'s `!c.capable` fast path (`conn.go:190-193`) hands the peer's `{"header":…,"data":…}` bytes to the application unparsed. `Conn` therefore records the wire fact in its own field, unclamped, and the read path unwraps whenever that field is set. The write side stays clamped and the asymmetry is safe in that direction, because a peer receiving a raw frame falls back to the payload. Unwrapping is `json.Unmarshal` with the headers discarded — no span, no attribute build, no propagator call — so the disabled-mode invariant is untouched.
- A relay revocation on a negotiated connection stops spans and stops injection; the envelope keeps being written with an empty header, because the peer parses every frame as one.

*Consequence — the WebSocket kill switch is a telemetry switch, not an overhead switch.* Because the envelope survives a revocation, a revoked `otel-gorilla-ws` still runs `marshalWire` on every write and the `tryUnmarshalWire` probe on every read; only the spans and the inject/extract disappear. It is the one module of the four that does not return to the zero-cost path when the relay revokes, and removing that wire overhead requires a redeploy. The alternative — dropping the envelope on revocation — desynchronises the wire from a peer that is still enveloping and silently dismembers any payload shaped like one, so correctness wins. `feature-flags.md`'s operational summary states the limit next to "set its relay flag to `false`", because an operator pulling the brake during a latency incident would otherwise expect relief that does not come.

*Consequence.* Two peers that both run this library with `gate1` on **and** `OTEL_GORILLA_WS_TRACING_ENABLED` truthy exchange the JSON envelope on every message, including while the relay has revoked tracing. That is a deliberate deployment choice by that site, not something a reader of the previous CHANGELOG would be surprised by.

### D10. Mongo document helpers carry no gate

`ContextFromDocument` and `ContextFromRawDocument` carry no feature-flag gate at all. They read a `_oteltrace` field out of a document the caller already holds, run `propagator.Extract` on it, and return the span context it encodes. They start no span, allocate no attributes, initialise nothing in the OTel SDK, and write nothing anywhere.

The flags exist to stop the library doing work **on the caller's behalf**. `Collection.InsertOne` is called for the business operation and gets instrumented as a side effect the caller never asked for at that call site; a kill switch is exactly right there. These two are called only when the caller wants trace extraction and for no other reason. Gating something whose sole purpose is the thing being gated leaves the caller no way to express what they already expressed by calling it.

The comparison that settles it is with `Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace`, which look superficially similar and **are** gated:

| | emits telemetry | writes to the document | gated | still extracts when the flag is off |
|---|---|---|---|---|
| `Collection.InsertOne` and siblings | CLIENT span | `_oteltrace` | yes | — |
| `Cursor.DecodeAndTrace` | `mongo.cursor.decode` span | no | yes | **no** (`direct.Cursor.DecodeAndTrace` returns `ctx` unchanged) |
| `ContextFromDocument` | no | no | **no** | **yes** |
| `ContextFromRawDocument` | no | no | **no** | **yes** |

`DecodeAndTrace` starts and ends a real span on every call, so it belongs under the switch. The package-level pair does not, so it does not. An earlier revision of this design had them following the relay on the grounds that two code paths in one change-stream loop should not obey different rules; that argument assumed the two paths were the same kind of thing, and they are not.

The last column is the one an operator has to read, and it is why the table gains it: without it, both ungated rows look inert. **Revocation does not stop trace-context extraction.** A caller who wants linking to survive the library being silenced writes `Decode` + `ContextFromDocument` instead of `DecodeAndTrace`, and gets it — the gate on `DecodeAndTrace` governs the span it emits, not the linking, and is bypassable by design through the documented alternative. `feature-flags.md` § *What is not gated* says this in those words, because § *Operational summary*'s "to stop a module now" otherwise reads as though everything stops.

*Consequence — the invariant is about gated paths.* The disabled-mode invariant's "no propagator inject/extract" clause is scoped to code the flags govern. `propagation` is OTel **API**, not SDK, so nothing in the compiler-enforced `internal/direct` boundary or the CI grep is weakened; the clause is restated in `CLAUDE.md` to say what it protects (no span, no SDK, no exporter, no attribute build, no injection) rather than to list mechanisms.

*Consequence — BREAKING.* A process with every switch off previously got a zero `SpanContext` and `false` from `ContextFromDocument`, and an unmodified `ctx` from `ContextFromRawDocument`. It now gets the document's real span context. The direction is more capability, not less, and only code that calls these functions is affected — but a deployment that switched an environment variable off specifically to stop trace linking must now stop calling them instead.

*Consequence — one relay evaluation per document disappears.* These two are the per-document call in a change-stream or cursor loop, so under the previous gate they paid D4's 2 µs and 7 allocations once per document, on top of whatever the operation itself resolved. Ungated, they resolve nothing. The cost D4 accepts is now confined to wrappers doing work on the caller's behalf, which is where the argument for paying it lives.

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

### D12. Evaluation context is the application's, except for one attribute on the zero-code path

An application that installs its own provider owns its evaluation context outright: it calls `openfeature.SetEvaluationContext`, the SDK merges that into every evaluation, and the library adds nothing. That is unchanged.

The zero-code path (D17) cannot do that — `SetEvaluationContext` is Go code, and the whole point of the path is that there is none. Left empty, its evaluation context makes every relay rule untestable, so `otel-mongo-tracing: false` is the only expressible revocation and it lands on **every process in the fleet**. An incident in one service forces a fleet-wide revocation or nothing.

So when — and only when — D17 installed the provider, the resolver supplies one attribute:

```go
if svc := os.Getenv("OTEL_SERVICE_NAME"); svc != "" {
    r.evalCtx = openfeature.NewTargetlessEvaluationContext(
        map[string]any{"service.name": svc})
}
```

Three things make this narrow enough to be safe:

- **`OTEL_SERVICE_NAME` is not a guess.** It is the OpenTelemetry specification's own variable, already set by any deployment running an exporter. Reading it is the least arbitrary source available to an OTel instrumentation library; `OTEL_RESOURCE_ATTRIBUTES` (a spec format to parse) and hostname (genuinely arbitrary) stay out.
- **It is passed at the invocation site, never through `SetEvaluationContext`.** The SDK merges *API → transaction → client → invocation*, so this composes with an application's global context instead of replacing it, and D1's rule against mutating global state holds.
- **It is confined to the D17 path**, which removes the one collision the merge order would otherwise create: invocation wins over global, so supplying it on the application-installed path could override a `service.name` the application set itself. A process on the D17 path has no global context to override.

Unset `OTEL_SERVICE_NAME` yields an empty context and today's behaviour exactly. What it buys is a relay rule that can name one service:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  targeting:
    - query: service.name eq "checkout-api"
      variation: disabled
  defaultRule: { variation: enabled }
```

Per-request targeting remains a Non-Goal regardless: the attribute is process-scoped and the resolver holds no request state.

### D13. Testing uses an in-memory provider

Tests install `memprovider.NewInMemoryProvider(...)` through **`SetNamedProviderAndWait(flags.FlagDomain, …)`**, mutate a flag value, and assert the next operation observes it.

**Named, not default, and the endpoint variable must be unset.** Both halves are forced by D17. A named provider on `FlagDomain` outranks the default for our clients, so a test that installs a default provider is silently shadowed the moment any earlier test in the same binary triggered an auto-install — the assertion then reads whatever that provider serves. And `clientOnce` makes the install a once-per-process event that no test can undo, since D6 deleted `ResetForTest`. Installing on the same domain the production path resolves through removes the shadowing; keeping `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` unset in the test environment removes the trigger. Tests that exercise D17 itself set the variable deliberately and assert on the registration, in isolation from the rest.

Rejected: reintroducing a `ResetForTest` hook, which D6 has just deleted and which the above does not need; and making the domain configurable, which restores the five-way string agreement D5 removed, for tests alone. Because D4 resolves per call, no clock injection, no reset hook and no waiting are involved: the change is visible on the very next operation. This is what replaces the deleted `Gate.ResetForTest` — the provider is the control surface, so tests drive the real code path instead of bypassing it.

Tests must exercise the kill-switch asymmetry explicitly: a relay `true` against a falsy module environment variable must produce **no** spans and **no** evaluation, and a relay `false` against a truthy one must stop a running connection.

Because D3 makes the environment variable and the option mutually exclusive, every existing test that sets `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` *and* passes `WithTracingEnabled` must be rewritten to use exactly one of them. The same applies to `OTEL_MONGO_PROPAGATION_ENABLED` and `WithTracePropagationEnabled`.

One integration test stands up a real GO Feature Flag relay proxy container and drives one module end to end, verifying that the wiring recipe in the documentation actually resolves against a real relay: provider construction options, endpoint format, and flag keys matching a real relay configuration file. It must assert the revoke direction, since that is the only direction the relay has. Only one module is covered; the wiring is identical across the four and three more containers would add cost without information.

A full harness-level assertion that spans stop reaching the OTLP sink after a revocation is deliberately excluded. It would have to outwait the provider's poll interval and the exporter's batch timeout, making it a timing race; its two halves are already covered separately by the integration test (the value propagates) and the unit tests (a false value emits no span).

Because the OpenFeature provider and the environment are both process-global, tests that touch them must not call `t.Parallel` — the same constraint that already applies to the environment-toggling tests.

This revision's decisions each need coverage:

- **D17 auto-install** — fires with the endpoint set and no provider installed; stands down when a provider already exists; a malformed `_POLL_INTERVAL` warns, falls back to 60 s and still installs; an unset endpoint installs nothing and touches no SDK state.
- **D12 `service.name`** — attached on the auto-install path when `OTEL_SERVICE_NAME` is set, absent otherwise, and never attached on the application-installed path.
- **D14 warning** — a set-but-unrecognised value warns; unset, truthy and explicitly falsy values do not.
- **D9 Q2** — `SubprotocolOTelWS` and `IsOTelNegotiated` agree with what `NewConn` actually does.
- **D9 Q3** — a conn that proved `otel-ws` with `capable` false returns the *unwrapped* payload from `ReadMessage`, not the envelope bytes, and still writes raw.
- **D9 Q4** — a JSON-object payload carrying neither trace key comes back byte-identical, key order included.
- **Open question 1** — a document already carrying `_oteltrace`, re-injected, yields exactly one occurrence and extraction returns the new value.

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

**A set-but-unrecognised value warns.** `EnvEnabled` emits one `slog.Warn` when the variable is present and its value is in neither list:

```
level=WARN msg="unrecognised boolean value; treated as disabled"
  var=OTEL_INSTRUMENTATION_GO_TRACING_ENABLED value=enabled
  accepted="1,true,yes,on / 0,false,no,off"
```

Three cases stay silent, so a correct deployment logs nothing: unset (the legitimate default-off), a value in the falsy list (an explicit off), and a value in the truthy list. Only a misconfiguration speaks. This became possible with D17, which brings `log/slog` into the shared file for the provider install; before it the library had no output channel and the failure had to stay silent. The cost is bounded by D8's invariant that `EnvEnabled` is called at construction only and never on a hot path.

**No deduplication.** A process constructing N wrappers emits the warning N times. A `sync.Map` of already-warned names would fix that, and is rejected: it puts mutable state into the file D5 identifies as the highest drift risk in the repository, to suppress repetition of a message that only appears when something is already wrong.

*Consequence — BREAKING.* Deployments that enable a switch with any value outside the allow-list flip to disabled on upgrade. The direction is fail-safe (less instrumentation, never more), and the warning above names the cause at the moment it happens rather than leaving it to present as "spans disappeared after upgrading" — but it must still be called out in every CHANGELOG, because a deployment that does not read warnings sees only the symptom.

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

### D17. The library installs a named provider from the environment when none exists

An application obtains relay control by setting environment variables. It writes no Go code, adds no import, and changes nothing but its deployment configuration.

```go
// inside Resolver.evaluator(), under the existing clientOnce
if endpoint := os.Getenv(EnvFlagsEndpoint); endpoint != "" &&
    openfeature.ProviderMetadata().Name == "NoopProvider" {
    // ... construct, register (non-blocking), populate evalCtx per D12
}
r.client = openfeature.NewClient(FlagDomain)
```

Two conditions, both necessary. The endpoint variable is the operator's expression of intent; the `NoopProvider` check is what makes the install an *allowance* rather than a takeover — an application that installs its own provider before constructing any wrapper keeps it, and this path stands down.

| Setting | Source | Note |
|---|---|---|
| `Endpoint` | `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | Unset or empty ⇒ nothing is installed and no SDK state is touched |
| `APIKey` | `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | Never included in a warning or error message |
| `FlagChangePollingInterval` | `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | `time.ParseDuration` only, default `60s` |
| `DataCollectorDisabled` | **hardcoded `true`** | D1's stall mechanism, now unmissable |
| `EvaluationType` | **hardcoded `INPROCESS`** | The unsupported remote mode is unreachable from this path |
| everything else | not exposed | `Headers`, `ExporterMetadata`, `HTTPClient`, `Logger`, and the six `DataCollector*`/`FlagCache*` fields, which the two hardcoded settings make inert anyway |

**Duration strings only, and a malformed one does not disable the brake.** `60` is rejected rather than read as 60 ms — the OTel convention of bare-integer milliseconds would turn a plausible value into a catastrophic misreading of a polling interval. A parse failure warns through `slog.Default()`, falls back to `60s`, and **still installs**. The opposite rule (refuse to install) was decided first, when the install had a caller that could return the error; once it moved inside the library the error's only outlet became a log line, and refusing would let a typo in an optional tuning knob silently delete the entire kill switch — the highest-severity outcome reachable from the lowest-severity mistake. An empty endpoint is different in kind: there is no default to fall back to, so nothing installs.

**Why 60 s.** The provider's own default is 120 s (`evaluator/inprocess.go:21`, applied when the interval is `<= 0`). Two minutes is the wrong default for an emergency brake. At 60 s the poll is a conditional `GET` with an ETag returning 304 in the steady state, so the cost of halving it is negligible; D4's latency-budget note records that this interval, not the resolver, is what revocation latency is made of.

*Alternatives considered — all three failed on the same constraint.* The application may change `go.mod` only, not any `.go` file. Go initialises packages purely from the import graph, and `go mod tidy` deletes a `require` that nothing imports, so **no code can be made to run from `go.mod` alone** — not via `godebug` (stdlib toggles), `tool` (build-time), or build tags (a build-command and `.go` change). The trigger therefore has to live in a package the application already imports, which means the instrumentation modules themselves.

- *A separate `otel-flagsetup` module with a blank import* (`import _ ".../autoinstall"`) is the idiomatic Go answer and was the decision until that constraint was stated. It keeps the provider's dependency tree out of every consumer's build, because a Go dependency follows the import. One `.go` line, which is one too many.
- *A minimal in-house provider* built on `net/http` and `encoding/json` keeps `go.mod` clean and needs no app code. Rejected on correctness: the relay's configuration format supports targeting rules, percentage rollouts and JSONLogic queries, and a minimal evaluator would silently ignore a revocation expressed as any of them — an operator would believe the brake was applied when it was not.
- *The OFREP provider* has an effectively empty dependency tree and would evaluate correctly, since the relay does the evaluating. Rejected because it has no cache and no poller — `internal/evaluate/resolver.go` issues one HTTP request per `Boolean` call, putting a network round trip on the path of every Mongo query and NATS publish, which D4 and `feature-flags.md` § *In-process evaluation only* exclude by design.

*Consequence — four `go.mod` files gain the GO Feature Flag provider.* That brings roughly ten modules including `go-feature-flag/modules/core`, the ofrep provider, `bluele/gcache`, `diegoholiveira/jsonlogic`, `nikunjy/rules` and a full `antlr4-go/antlr` runtime, into every consumer's build — including consumers that never set the endpoint variable. The cost is to `go.sum` length, vulnerability-scanning surface and licence review rather than to runtime, since the linker drops unreached code. It is the price of the zero-`.go`-change requirement, and no design satisfies both.

*Consequence — a bounded startup window in which nothing can be revoked.* The install is non-blocking, so between it and the provider's first successful fetch every flag resolves to `true`. An application that cannot accept that closes it by installing its own provider with `SetProviderAndWait`; see D1.

*Consequence — the four modules can race to install.* Each module's `internal/flags` copy holds its own `clientOnce`, and no state is shared between them, so two modules evaluating for the first time concurrently can both observe `NoopProvider` and both register. The second registration replaces the first and the SDK shuts the first down (`shutdownOld`, whose multiple-bindings guard does not apply since each instance is bound to one domain), leaving one live provider and one poller. The cost is a duplicated first fetch. No lock can span four `internal/` packages that do not import each other; accepted and stated.

*Consequence — the poller outlives everything.* Nothing shuts the provider down: there is no handle to hand back on a path whose entire premise is that the application writes no code. D4 rejected a background ticker partly for lacking "a shutdown story this repo has no API for", and this accepts that same gap knowingly, for one goroutine per process that ends with the process. An application that needs lifecycle control installs its own provider and owns it.

## Risks / Trade-offs

**Tracing cannot be enabled remotely.** The relay is the wrong tool for "turn this on so I can see what is happening". → Stated in the README next to the wiring snippet, and in every CHANGELOG. Sites that want relay control deploy with the module switch on and use the relay as a brake.

**Deployments that want relay control run with instrumentation on by default.** Under D2 the only relay-controllable state is "environment says on, relay may revoke", so the resting state is on. → This is the honest cost of never letting a remote party enable anything. Sites that want tracing off by default simply leave the module switch off and ignore the relay.

**`EnvEnabled` truthiness change silently disables some deployments.** Values like `enabled`, `2`, `y` and the empty string stop working. → BREAKING in all four CHANGELOGs, with the allow-list spelled out.

**Mutual exclusion invalidates existing configurations and 89 in-repo test call sites.** Anything that set the environment variable and passed the option now fails at construction. → BREAKING; the error message names both values. The one known downstream consumer passes no options.

**`NewConn` signature change.** → BREAKING; see D16.

**A misconfigured provider can turn a relay outage into an application stall.** The provider's data collector, on by default, flushes synchronously from the evaluating goroutine once its buffer fills, and a failed flush never drains it. → On the D17 path this is **enforced in code**: `DataCollectorDisabled: true` is hardcoded and not exposed as a variable. For an application installing its own provider it remains documented-only, with the mechanism spelled out, in `feature-flags.md` and in every wiring snippet.

**Every evaluation runs on the caller's goroutine.** With the supported in-process provider that is 2 µs and 7 allocations per operation (D4). With an unsupported remote-evaluation provider it would be a synchronous HTTP request on the path of every Mongo query and NATS publish. → The per-operation cost is measured, recorded and accepted as a deferral. The unsupported mode is now unreachable from the D17 path, which hardcodes `INPROCESS`; it remains stated in `feature-flags.md` and in the README for applications installing their own provider.

**Revocations are not atomic across flags.** A relay change touching several flags is observed by consecutive `Boolean` calls microseconds apart, so an operation reading two of them can in principle see one old and one new value. → This is R19, already accepted: the window is microseconds, and for the pair that actually interacts — Mongo tracing and propagation — a false tracing verdict short-circuits propagation, so the combination fails safe.

**`_oteltrace` is written into application documents and never removed.** Roughly 90 bytes per document across six write methods, with no strip on read, no undo, and a hard write failure against strict `$jsonSchema` validation. → D2 and D10 mean only the deployment can start this; the relay can only stop it. Documented in the module README with the field shape, the write methods, the size, and the fact that cleanup is a `$unset` migration.

**Four `go.mod` files gain the OpenFeature SDK *and* the GO Feature Flag provider.** Consumers that never set the endpoint variable still resolve roughly ten additional modules, including a full ANTLR runtime, a JSONLogic evaluator and a rules engine. → Accepted in D17 as the price of relay control without a `.go` change, since Go cannot run code from `go.mod` alone. The cost lands on `go.sum`, vulnerability-scanning surface and licence review, not on runtime. The one design that avoids it — a separate module plus a blank import — costs the application one line of Go, which the requirement excludes.

**The kill switch cannot be revoked during a bounded startup window.** D17 installs non-blocking, so from the install until the provider's first fetch every flag reads `true`; for `otel-mongo` that window can write permanent `_oteltrace` fields. → Stated in D1 and `feature-flags.md`, with the way to close it: install a provider with `SetProviderAndWait` before constructing any wrapper and D17 stands down. Blocking by default was rejected because it would put a relay round trip in front of the first instrumented operation.

**Nothing shuts the auto-installed provider down.** One poller goroutine and one HTTP client live for the process lifetime. → Accepted in D17; a path whose premise is "the application writes no code" has nowhere to hand a shutdown function back to. Applications needing lifecycle control install their own provider.

**Two modules can install a provider concurrently.** The four `internal/flags` copies share no state, so both may observe `NoopProvider` and register. → The SDK's replace-and-shutdown leaves one live provider; the cost is one duplicated fetch. No lock can span four non-importing `internal/` packages.

**A revoked `otel-gorilla-ws` still pays the envelope on every frame.** Revocation stops spans and injection but not `marshalWire`/`tryUnmarshalWire`, so it is the one module that does not return to the zero-cost path. → Accepted in D9: dropping the envelope would desynchronise the wire from a peer still enveloping. Stated in `feature-flags.md` § *Operational summary* so an operator does not expect overhead relief from the brake.

**Revocation is not immediate.** End-to-end latency is the provider's poll interval — 60 s by default under D17, not the microseconds the uncached resolver suggests. → D4's latency note is corrected and `feature-flags.md` gains a section stating the number, replacing wording that read as "immediate".

## Migration Plan

1. Land all four modules in one commit — the byte-identical `internal/flags` copies cannot be split across commits.
2. Tag `otel-mongo/v0.9.0`, `otel-mongo/v2.9.0`, `otel-nats/v0.8.0`, `otel-gorilla-ws/v0.8.0`. Tags may be pushed sequentially; the release guard validates each against its version constant.
3. Existing deployments that upgrade **without** installing an OpenFeature provider keep their previous behavior, with two exceptions that must be checked before upgrading:
   - any switch set to a value outside the `1`/`true`/`yes`/`on` allow-list now reads as disabled (D14);
   - any code that sets `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` *and* passes `WithTracingEnabled` now fails at construction (D3/D15).

   Unlike the design that preceded this revision, there is **no** wire-format exception for `otel-gorilla-ws`: negotiation is gated on the static capability (D9), so peers see exactly the previous release's wire.
4. `otelgorillaws.NewConn` call sites must take the new error return (D16). Two `otel-gorilla-ws` behaviours also change without a signature change, both fixes, both altering returned bytes in the affected case: `ReadMessage` on a connection that proved `otel-ws` with capability off now returns the unwrapped payload instead of the peer's envelope bytes (D9), and a JSON-object payload carrying neither trace key is returned byte-identical instead of re-marshalled with sorted keys (D9). `SubprotocolOTelWS` and `IsOTelNegotiated` are additive.
5. Deployments adopting the relay set `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` (plus `_API_KEY` and `_POLL_INTERVAL` if needed), create the flags on the relay, and **deploy with the module switches on** — the relay can only revoke. No application code changes. Until a flag exists on the relay, the module runs at its deployed state. Applications that already install their own OpenFeature provider keep it and need not set the endpoint variable; D17 stands down when a provider is present.
6. Deployments wanting per-service targeting set `OTEL_SERVICE_NAME` (D12) and write the relay rule against `service.name`. Without it, a relay flag applies to every process in the fleet.

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
| A per-module snapshot behind an `atomic.Pointer` with a one-second TTL | D4 — deferred, not rejected. Caching is invisible behind `Allowed(i) bool`, so it can be added later at no call-site cost; the measured 2 µs it would save is recorded there |
| The application owns the provider outright; the library never installs one | D17 — the library installs a **named** provider on its own domain when the environment asks for one and none exists. The default provider, the global evaluation context and shutdown remain the application's |
| Applications SHALL install with `openfeature.SetProviderAndWait`, because a not-ready provider cannot revoke | D1 — downgraded to the application's call. D17 installs non-blocking so a brake never becomes a latency source; an application that needs the startup window closed installs its own provider and D17 stands down |
| "The SDK offers no reliable way to ask whether the installed provider is still the no-op default" | Factually wrong. `openfeature.ProviderMetadata().Name == "NoopProvider"` answers it, and D17 makes it the auto-install trigger |
| Each module resolves through its own domain (`otel-mongo`, `otel-nats`, …) | D5/D17 — one process-scoped `FlagDomain`. Per-module domains would need one provider instance each, because `InProcess.Init` is not idempotent and registering one instance under N domains leaks N−1 unstoppable pollers |
| Revocation takes effect immediately | D4 — end-to-end latency is the provider's poll interval, 60 s by default. The resolver adds nothing; it never did |

## Post-review remediation (PR #27 grill, 2026-08)

Source: `reviews/code-review-pr-27-openfeature-dynamic-flags.zh-TW.html` and a decision grill on each finding. Items unaffected by the revision above stand as decided.

| ID | Topic | Decision | Status after revision |
|----|--------|----------|----------------------|
| R1 | `NewConn` wire corruption when capability on + feature off | Envelope only if negotiated/proven; fail → raw wire; local spans OK; no force-negotiated option; clamp with capable (R7) | **Amended (D9)** — the clamp is correct for the write path and wrong for the read path; the wire fact is now recorded unclamped and `ReadMessage` unwraps on it. "No force-negotiated option" stands, and `SubprotocolOTelWS`/`IsOTelNegotiated` are added so the instruction it implies can be followed |
| R2 | `MessageBatch` freezes flag at Fetch | Always return a dynamic batch wrapper; per-message gate re-check | Stands (D8) |
| R3 | `Resolver.refresh` last-store-wins + late `at` | Stamp `at` at evaluation start; no CAS/mutex | **Moot** — D4 removes the snapshot, so there is no `at` and no refresh |
| R4 | otel-ws negotiation vs "no provider ⇒ no change" | Was: keep behavior, document exception | **Withdrawn** — D9 removes the exception |
| R5 | Mongo single-call-chain torn read of tracing | Pass resolved tracing into propagation; no internal recompute | Stands |
| R6 | JetStream per-message rebuild of tracer/attrs | Hoist tracer/prop/baseAttrs to construction; gate stays per-message | Stands |
| R7 | `capable` / `tracingEnabled` no choke-point clamp | Subsumed into R1 | **Amended (D9)** — clamp applies to the write decision only |
| R8 | Dead second return of `collectionImpl` Find/Aggregate/Watch | Drop second return; stop throwaway `New*` in impls | Stands |
| R9 | `tracedMessagesContext.Next` not gate-first | Gate-first delegate to `directMessagesContext` | Stands |
| R10 | Gate/propEnabledGate doc drift | Full sync: CLAUDE, test comments, jetstream godoc, main spec | Stands, widened by the revision |
| R11 | `WriteMessage` nil span + dual guards | Feature-off uses noop span; drop nil guards | Stands |
| R12 | NATS Consume path triple `impl()` per message | Resolve once per message; pass down | Stands |
| R13 | `dynamicTracingPossible` duplication / parallel refresh | `flags.GlobalTracingPossible()` | Stands, plus `GlobalTracingSet` (D5); the parallel-refresh half is moot with no refresh |
| R14 | Six copies of facade `impl()` selection | Was: generics `selectImpl` per mongo module | **WONTFIX** — never implemented despite being marked done; declined in D8 now that `impl()` is four lines and the three impl interfaces differ |
| R15 | Five copies of relay test helpers | Move to `otel-testkit/harness` | Stands |
| R16 | Client/Database `effective*` duplication | Shared gateState | Stands |
| R17 | otelnats `impl`/`msgHandler`/`traceEventMsgHandler` policy | WONTFIX extract; lockstep comment only | Stands |
| R18 | Dead nil-handler guard in `tracedConsumeHandler` | Delete dead guard | Stands |
| R19 | Same-refresh sequential Boolean micro-torn pair | WONTFIX | Stands, and is now the only tearing window — D4 removes the TTL boundary that was the larger half |

## Open Questions

Both items previously listed here are now **in scope** and carried in `tasks.md` as work, not as questions:

1. **Read-modify-write produces a duplicate `_oteltrace` field.** `InjectTraceIntoDocument` appends unconditionally (`internal/shared/tracing.go:55`), so a document read into a `bson.M`, modified and written back with `ReplaceOne` carries the field twice. This was recorded as needing a test to establish the server's behaviour first; that framing understated it, because the **read** side is deterministically wrong regardless of what the server does: `ExtractMetadataFromRaw` uses `bson.Raw.LookupErr`, which returns the **first** match, so extraction yields the stale trace context from the original write and a read-modify-write loop pins the linkage there permanently. Inject removes any existing key before appending, in both modules. A change whose central argument is the correctness of what gets written into application documents (D2, D10) should not ship carrying a known defect in exactly that.
2. **`CLAUDE.md` claims `_oteltrace` is "stripped on read".** No such code exists in either module, and D10 depends on the opposite being true. Corrected wherever it appears.

Items deliberately excluded from this change stay Non-Goals: dynamic sampling rates, per-request targeting, a harness-level flag-flip E2E assertion, `Resolver` CAS/singleflight, parallel `Boolean` fan-out, and remote enablement of any switch.
