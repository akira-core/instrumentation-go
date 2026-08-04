## Context

Every tracing and propagation switch in this repo is an environment variable read once per process. Three enforcement patterns consume those reads, and which one a type uses is decided by how much of its surface needs instrumenting:

- **Strategy split** (`otel-mongo` Collection / Cursor / SingleResult / ChangeStream; all of `otel-nats` — both `otelnats.Conn` and the `oteljetstream` wrappers). The flag decides *which object the caller talks to*: two types implement one interface, one pure delegation and one pure instrumentation, so no method body contains both paths. Chosen where nearly every method needs a span — `collectionImpl` is 15 methods, and the passthrough and instrumented implementations are 99 and 423 lines respectively. `otel-mongo` enforces the split across an `internal/direct` / `internal/traced` package boundary, so the disabled path is **compiler-enforced**: `internal/direct` imports no `go.opentelemetry.io/otel` package and CI greps to keep it that way. `otel-nats` keeps `directConn`/`tracedConn` and `directJSImpl`/`tracedJSImpl` in one package, so its equivalent is reviewer-enforced.
- **Cached gate** (`otel-gorilla-ws`). The flag decides *which branch inside a method runs*: the constructor resolves a `featureEnabled` bool onto the wrapper and each instrumented method opens with a gate check. Chosen because `Conn` embeds `*websocket.Conn` — that embedding is what gives callers the whole gorilla API for free, and only `WriteMessage` and `ReadMessage` need wrapping. Splitting two methods would cost an interface over roughly thirty.
- **Gate carrier** (`otel-mongo` Client / Database). Neither type instruments anything: `Disconnect`, `Ping`, `StartSession` and `Database.Collection` are pure delegation with no span. Their whole job is to resolve the flag state once and hand it down to the `Collection` / `Cursor` / `ChangeStream` wrappers that do the work. There is nothing to split and nothing to gate — only state to carry, which R16 factors into a single `gateState` helper.

`internal/flags` is vendored as four byte-identical copies with zero external dependencies. It exports `EnvEnabled` (default-off env read) and `Gate` (a `sync.Once` + `atomic.Bool` cache whose documented contract is that environment changes after the first read are ignored).

All three patterns and the `Gate` contract assume the answer never changes after startup. Making the switches dynamic means revisiting all of them: the split must re-select per operation, the gate must re-read per call, and the carrier must stop caching what it hands down. It also ends `internal/flags`'s zero-dependency property — a property worth naming, because a vendored four-copy file with no imports is trivially safe to duplicate. D5 goes further and ends the vendoring itself.

The OpenFeature Go SDK is at v1.17.2. Its relevant surface:

- `openfeature.SetProviderAndWait(p)` installs a process-global **default** provider; `SetNamedProvider(domain, p)` installs one scoped to a domain, which takes precedence over the default for clients bound to that domain.
- `openfeature.NewClient(domain)` returns a client that resolves through the named provider for `domain` when one exists and falls back to the default otherwise.
- `client.Boolean(ctx, key, defaultValue, evalCtx)` returns `defaultValue` on any error — including when no provider was ever installed, since the SDK's default is a no-op provider. **This is the whole precedence mechanism** (D2).
- `openfeature.ProviderMetadata().Name` reports the installed default provider's identity, and `openfeature.NamedProviderMetadata(domain).Name` reports the provider bound to a domain, **falling back to the default's metadata when none is bound** (`openfeature_api.go:178-181`). Either reads back `"NoopProvider"` exactly when nothing has been installed, which makes them a reliable "has the application configured a provider?" test; D7 and D17 both use the named form.
- `Client.evaluate` merges evaluation contexts in the order *API (global) → transaction → client → invocation* (`client.go:695`), so an attribute passed at the invocation site composes with — and wins over — the application's global context without the library ever calling `SetEvaluationContext`.
- `openfeature/memprovider` provides an in-memory provider suitable for tests.

**This document has been revised twice.** Revision 1 replaced a relay that decided in both directions with a revoke-only kill switch, and was implemented and merged before this second review. Revision 2 restores bidirectional control, but as a per-switch **precedence ladder** rather than the conjunction of revision 1 — and pairs it with defaults that keep a zero-configuration process fully off. Neither shape has ever been released: `otel-mongo 0.9.0`, `otel-mongo/v2 2.9.0`, `otel-nats 0.8.0` and `otel-gorilla-ws 0.8.0` are all still in their CHANGELOGs' unreleased sections, so **the external migration story is `0.7.0` → this change**, not kill-switch → toggle. See § "Superseded decisions" for both rounds and `tasks.md` § 10 for the remaining work.

## Goals / Non-Goals

**Goals:**

- An operator can turn each module's tracing and propagation **on or off** through a GO Feature Flag relay proxy without restarting the application.
- A process that configures nothing traces nothing. The defaults are chosen so that "no environment variables, no options, no relay" resolves to fully off, and so that no single variable set by mistake can turn on more than it names.
- One switch — the process-wide master — can stop every module in the process regardless of what any option or per-module setting says, and it is expressible in the environment alone.
- Exactly one OpenFeature provider serves every instrumentation module in a binary, guaranteed structurally rather than by convention (D5).
- Deployments that configure no relay keep the previous release's behavior *and* its cost: no OTel SDK allocation, no OpenFeature evaluation, no per-operation overhead (D7).
- Hot paths pay a bounded, predictable cost with no network call, and the per-operation cost of resolving a flag is measured and recorded rather than assumed (D4).
- The compiler-enforced disabled path survives. `internal/direct` still imports no `go.opentelemetry.io/otel` package, and CI still greps for it.
- A configuration this library cannot interpret fails loudly at construction rather than silently picking a value (D14, D15).
- An application can obtain relay control **without writing any Go code**, by setting environment variables alone (D17).

**Non-Goals:**

- Per-request flag targeting (per tenant, per user). The resolver holds no per-request state and passes a process-scoped evaluation context; request-scoped attributes cannot influence it.
- Dynamic sampling rates. `otel-sampler` is untouched.
- Supporting the GO Feature Flag provider's remote evaluation mode, which would put an HTTP request on the operation path; see D4.
- Changing span shapes, attributes, semantic conventions, or business logic anywhere.
- Owning the OpenFeature **default** provider, the **global** evaluation context, or provider shutdown. The library installs a **named** provider on its own domain when the environment asks for one and none exists (D17); everything outside that domain stays the application's.
- Retrofitting relay control onto a WebSocket connection whose handshake already completed. Negotiation is a handshake fact (D9).

## The resolution model in one place

Each switch is resolved independently down a four-step ladder — **first source with an opinion wins**:

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

Three switches exist, and they differ in which steps they accept and in what they default to:

| Switch | Relay key | Option | Env | Default | Role |
|---|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` | veto over the whole process |
| per-module tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` | the actual on/off |
| Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` | the `_oteltrace` tier |

The three compose by conjunction:

```
tracing     = master && moduleTracing
propagation = tracing && mongoPropagation
```

**The master defaults to `true` because it is a veto, not an enabler.** Its default means "express no objection"; the only value with an effect is `false`. Nothing turns on because the master is `true` — the per-module default of `false` is what keeps a zero-configuration process silent. What the master buys is a single environment variable, or a single relay flag, that stops every module in the process including connections whose Go code passed an option.

Construction resolves everything the relay cannot change:

```go
// Construction, once per connection/client.
masterLocal, err := otelflags.MasterLocal()             // env, else default true; error on an invalid value
tracingLocal, err := resolveLocal(tracingOption, envModuleTracing, false)
propLocal, err    := resolveLocal(propagationOption, envMongoPropagation, false)   // otel-mongo only

// resolveLocal: env wins over the option, the option wins over the default; an
// env value that is neither truthy nor falsy is an error in every case.

relayPossible := otelflags.RelayPossible()   // endpoint set, or a provider already bound

// D7: what is allocated at all.
useTracedImpl := relayPossible || (masterLocal && tracingLocal)
```

```go
// Every operation.
tracing := gate.tracing()
// where:
//   !relayPossible  ->  masterLocal && tracingLocal
//   relayPossible   ->  otelflags.MasterEnabled(masterLocal) &&
//                       resolver.Value(idxTracing, tracingLocal)
propagation := tracing && resolver.Value(idxPropagation, propLocal)   // otel-mongo only
```

Four sources, four owners, four distinct powers:

| Source | Owner | Scope | Changeable without a redeploy? |
|---|---|---|---|
| relay flag | operator | fleet, or one service via D12 targeting | **Yes — the only one that can** |
| `OTEL_*` environment variable | deployer | one process | No |
| `With*Enabled` option | the caller that constructs the wrapper | one connection/client | No |
| hardcoded default | this library | everywhere nobody spoke | No |

**The ladder is ordered by how late in the pipeline each source is decided.** The default is compiled in, the option is written when the wrapper is constructed, the environment variable is set when the process is deployed, and the relay is set while it runs. Each later stage overrides the earlier ones, which is the ordinary layering an operator already expects from configuration and needs no separate rule to remember.

Putting the option *below* the environment is what gives the operator a middle setting between "stop this one process" and "stop the whole fleet". `OTEL_MONGO_TRACING_ENABLED=false` disables that module for that deployment even when the application's Go code hardcoded `WithTracingEnabled(true)`, without a relay and without silencing the other three modules. It matters most for propagation, where the alternative order would let a library author start writing permanent `_oteltrace` fields into the operator's documents with no environment-level way to stop it (D10).

*What it costs.* An option is only consulted when the paired environment variable is unset, so a process that sets the variable cannot differentiate two connections through options. That is the one thing the option is uniquely able to express — trace one of two Mongo clients — and it survives on the condition that the deployment leaves the variable unset. Under the per-module default of `false` that is the natural state rather than a sacrifice: unset means "off, awaiting an opinion", and the option is exactly the thing that supplies one. It is also a divergence from released `0.7.0`, whose `resolveFlag(override, envDefault)` let the option win; § Migration records it.

## Decisions

### D1. The application owns the default provider; the library reads through its own domain

`otel-flags` calls `openfeature.NewClient(FlagDomain)` and never `SetProvider`, `SetEvaluationContext`, `AddHooks`, or `Shutdown`. The one thing it may install is a **named** provider bound to `FlagDomain`, and only under D17's conditions. The client itself is created lazily on the first evaluation rather than in `NewResolver`, so a process that never reaches a relay-resolved path never initializes any part of the OpenFeature SDK.

The boundary this draws is narrower than "never touch the SDK's global state", and it is the load-bearing one: nothing the library does can change how the **application's own** feature flags resolve. A named provider on `otel-instrumentation-go` is invisible to `NewClient("")` and to every other domain.

This still mirrors the rule this repo applies to tracing — packages never initialize a `TracerProvider` — in the part that matters. A `TracerProvider` decides where the application's telemetry goes; the analogous blast radius here is the default provider, and that stays untouched.

*Consequence — the domain is an isolation boundary in one direction.* Once D17 has installed a named provider on `FlagDomain`, another library in the same binary that installs a global provider (breaking the rule we follow) can no longer decide our flags. Until then — and in every process that does not set the endpoint variable — `NewClient(FlagDomain)` falls back to the default provider. Under revision 1 the worst such a provider could do was revoke; under this revision it can also **enable**, so the exposure is now two-directional and worth stating plainly. The mitigations are the master veto, which no relay-installed-by-someone-else can spell for us without also knowing our key, and D7's `relayPossible`, which keeps a process with no endpoint and no bound provider entirely out of the SDK.

*Consequence — per-module providers are not available.* All four modules share one domain (D5), so an application cannot point `otel-mongo` at a different relay from `otel-nats`. Nothing asked for it, and a single domain is what makes one provider instance serve all four without leaking a poller goroutine per module.

*Consequence — two provider settings are load-bearing.* The design's guarantee that a relay outage cannot affect the application holds only if the provider is configured correctly, and two defaults work against it.

`DataCollectorDisabled: true` is required. The provider's data collector appends one event per evaluation to an in-memory buffer and flushes it on a two-minute ticker. A failed flush does not clear the buffer, and once the buffer reaches its 100,000-event cap **every subsequent append flushes synchronously, on the evaluating goroutine, holding the buffer's mutex**. With the relay down that flush fails after the HTTP client's 10 s timeout and the buffer never drains, so each evaluation repeats it while every other evaluating goroutine queues on the same mutex — turning a relay outage into stalled Mongo queries and NATS publishes. D4's decision to evaluate per operation makes the buffer fill proportionally to traffic, so this is reachable rather than theoretical. Nothing is lost by disabling it: the collector feeds the relay's evaluation-analytics dashboards, and for process-wide flags evaluated once per operation those analytics are a copy of the traffic volume.

An install failure must not abort startup. If the relay is unreachable at boot, the provider's first fetch fails and `SetProviderAndWait` returns an error; the documented handling is to log and continue, because a process must be able to start without its flag plane. The cost is stated rather than hidden: a process that starts while the relay is down comes up at the state its environment and options declare.

For the provider D17 installs, `DataCollectorDisabled: true` and `EvaluationType: INPROCESS` are **enforced in code** — hardcoded and deliberately not exposed as environment variables, so the zero-code path cannot be misconfigured into either failure. For an application that installs its own provider the library can enforce nothing, so both remain stated as requirements in `feature-flags.md` rather than as suggestions, with the failure mode spelled out.

*Consequence — the startup window is now fail-safe, and its meaning has inverted.* Between a non-blocking install and the provider's first successful fetch, every evaluation falls back to the value passed in — which under D2 is the locally resolved one. A process therefore starts at exactly the state its environment and options declare, and the relay's opinion arrives one fetch later.

Under revision 1 this window was a hazard: the fallback was a literal `true`, so a module the operator had revoked came back on until the first fetch. Under this revision the fallback is the deployment's own answer, so the window can no longer **enable** anything that was not already configured on, and for `otel-mongo` it can no longer write a `_oteltrace` field the deployment did not ask for. What it can do is delay a relay-driven **enable**, which is the harmless direction. An application that wants the relay's answer before its first operation installs its own provider with `SetProviderAndWait`, which also makes D17 stand down.

### D2. The relay is authoritative, and the evaluation default carries the rest of the ladder

Each switch is resolved as:

```go
func (r *Resolver) Value(i int, local bool) bool {
    if i < 0 || i >= len(r.keys) {
        return false            // a mis-wired module degrades to disabled, not to a panic
    }
    return r.evaluator().Boolean(context.Background(), r.keys[i], local, r.evalCtx)
}
```

`local` is the env-or-option-or-default value computed at construction. That single call **is** the precedence ladder: `Client.Boolean` returns the supplied default on every path where the relay has no usable answer — no provider installed, provider not ready, key absent from the relay configuration, evaluation error, type mismatch — and returns the relay's value otherwise. "The relay has no opinion" and "the relay is unreachable" collapse into the same outcome, which is what the ladder asks for: fall through to the next source down.

*Alternatives considered.* Detecting the relay's silence explicitly, through `BooleanValueDetails` and its `Reason` / `ErrorCode` (`FLAG_NOT_FOUND`, `ERROR`, `DEFAULT`), would let an unreachable relay be treated differently from a relay that simply does not configure the key. It was rejected because both must fall through to the local value, so the branch would compute a distinction it then discards, at the cost of a struct allocation on every operation. If a future requirement needs them separated — for example a warning when the relay is reachable but the key is missing — the details form is a drop-in replacement inside `Value` with no call-site change.

Passing a literal `true` and ANDing the environment separately — revision 1's shape — makes the relay revoke-only. It is the safer posture and it is what the previous round of review chose; § "Superseded decisions" records why it is being given up. The short version: it makes "turn this on so I can see what is happening" impossible, which is the operation an incident actually calls for, and the defaults chosen here (per-module `false`, master veto) recover most of the safety it bought without the capability loss.

*Consequence — the relay can enable.* A misconfigured, compromised, or merely stale relay can introduce instrumentation into a process whose deployment did not ask for it, and for `otel-mongo` can introduce **writes into the application's documents**. This is the deliberate inverse of revision 1 and is the change's largest risk; it is listed first in § Risks with its three partial mitigations (the master veto, the per-module default of `false`, and `relayPossible` keeping a no-endpoint process out of the SDK entirely).

*Consequence — failure direction.* When the relay is unreachable the resolved state is whatever the deployment declared. The library never fails into a state nobody configured, in either direction.

*Consequence — there is a single revoke-everything switch.* Unlike revision 1, the master switch has a relay key, so `otel-instrumentation-go-tracing: false` stops every module in every process the relay serves. Because the master's default is `true`, setting that key to `true` does nothing; the README must say so, or operators will create it expecting an enable.

### D3. The master switch is a veto with no option spelling

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and the relay key `otel-instrumentation-go-tracing` are the two spellings of the master switch. `WithTracingEnabled` is **not** a third: it feeds the per-module tier for the connection it is passed to.

The reason is that the master is process-scoped and the option is per-connection, so letting the option supply the master would mean each connection carries its own "process-wide" switch — a contradiction in the name, and a design in which no single setting can stop a process whose Go code hardcodes an opinion.

This deletes revision 1's mutual-exclusion rule and both of its error sentinels. The rule existed because two spellings of one tier could disagree; there are no longer two spellings of one tier. `ErrTracingConfigConflict` and `ErrTracePropagationConfigConflict` are removed, and the roughly 89 in-repo call sites that set an environment variable and passed an option become legal again, with the environment variable winning.

**The option sits below the environment variable, not above it.** This is a change from released `0.7.0`, whose `resolveFlag(override, envDefault)` let the option win, and it is the one place this design deliberately breaks the older behaviour rather than restoring it. Three reasons, in order of weight:

1. **The operator gets a per-module setting that code cannot override.** Without it, an operator facing a module that a library author hardcoded on has only two moves: the master veto, which silences everything, or deploying a relay. `OTEL_MONGO_TRACING_ENABLED=false` fills the gap.
2. **It closes the asymmetry on the one switch that writes data.** `WithTracePropagationEnabled(true)` would otherwise override `OTEL_MONGO_PROPAGATION_ENABLED=false`, letting application code start appending permanent `_oteltrace` fields to the operator's documents with no environment-level brake (D10). Every other switch merely produces or withholds telemetry; this one leaves state behind.
3. **The ladder stays monotonic in deployment order** — compile, construct, deploy, run — so it needs one sentence to document rather than a separate specificity rule that reverses between two adjacent rungs.

*Alternatives considered.* Letting the option win over the environment variable — `0.7.0`'s behaviour, and this design's shape until it was re-examined — makes per-connection differentiation work under every configuration, and keeps the upgrade trivial for anyone setting both. It was rejected on point 2 above; the cost of rejecting it is bounded, because per-connection differentiation still works whenever the module's environment variable is unset, which under a default of `false` is the ordinary state. § Migration records the divergence.

Letting `WithTracingEnabled` supply the master, overriding the environment, was also considered and rejected for the paragraph above it: a per-connection value cannot coherently spell a process-wide switch.

*Consequence.* `WithTracingEnabled` is a per-connection default for that connection's module tier: above the hardcoded default, below the module's environment variable, and far below the relay. It cannot escape a master veto, it cannot escape a relay verdict, and it cannot escape a deployment that names its module.

*Consequence.* A caller who passed `WithTracingEnabled(true)` with no environment variables kept tracing in `0.7.0` and keeps it here, because the master defaults to `true` and nothing above the option has an opinion. Only a deployment that sets *both* sees a change.

### D4. No caching: every relay-backed switch is resolved on every operation

```go
type Resolver struct {
    keys []string   // OpenFeature flag keys, in Value-index order

    clientOnce sync.Once
    client     openfeature.IClient
    evalCtx    openfeature.EvaluationContext   // populated only when D17 installed (service.name)
}

func NewResolver(opts ...ResolverOption) *Resolver
func WithFlagKeys(keys ...string) ResolverOption
func (r *Resolver) Value(i int, local bool) bool
```

That is the whole resolver. There is no snapshot, no TTL, no clock, no refresh, and no timeout. `NewResolver` takes no domain: the domain is process-scoped rather than module-scoped, so D5 makes it a constant in `otel-flags` and removes a string that would otherwise have to agree across five places with nothing checking it.

`evaluator()` is where the lazy `NewClient` lives, and D17 hangs the environment-driven provider install on the same `sync.Once`.

**What this costs, measured.** On an in-memory provider — the same shape as the GO Feature Flag in-process provider this design supports — one `client.Boolean` call is **2.0 µs, 336 B and 7 allocations**, against **82 ns and 0 allocations** for an atomic-pointer snapshot read. The 2 µs is not the flag lookup; it is the SDK's evaluation pipeline around it: before/after/finally hook chains, evaluation-context merging, the provider registry's RWMutex, interface dispatch. The provider being in memory does not help with any of it.

**Revision 2 multiplies the call count**, because the master switch is now relay-backed and must be resolved per operation like everything else — resolving it once at construction would mean a relay master veto took effect only for connections created afterwards:

| Module | Boolean calls per instrumented operation |
|---|---|
| `otel-nats`, `otel-gorilla-ws` | 2 — master, module |
| `otel-mongo` read path | 2 — master, tracing |
| `otel-mongo` write path | 3 — master, tracing, propagation |

So a Mongo write resolves in roughly 6 µs and 21 allocations. Against a Mongo round trip that is noise; against a NATS publish it is not, and it is stated rather than buried.

**Why no cache anyway.** Caching is a **pure internal optimisation behind an unchanged signature**. `Value(i, local) bool` reads identically whether the value came from a live evaluation or from a cached snapshot, so adding a cache later costs nothing at any call site. Deferring it removes an entire class of concurrency subtleties from the shared module — no TTL semantics to specify or test, no snapshot-timestamp question (R3), no cross-flag consistency window beyond the microseconds between consecutive calls (R19), no shared mutable snapshot to protect, no refresh timeout, and one less term in the revocation-latency budget.

**What the latency budget actually is.** A relay change becomes visible when the provider's background poll picks it up, and that interval is **60 seconds** by default under D17 (the GO Feature Flag provider's own default is 120 s, and `interval <= 0` falls back to it — `evaluator/inprocess.go:186`). A one-second TTL on top of that would move the worst case from 60 s to 61 s. This matters twice over: `feature-flags.md` must not describe a flag change as taking effect immediately, and the bar for adding a cache back is correspondingly low.

**The evaluation runs on the caller's goroutine.** With the supported in-process provider that is a local, allocation-heavy but bounded computation. With the unsupported remote evaluation mode it would be a synchronous HTTP request on the path of every Mongo query and every NATS publish — which is the reason that mode is unsupported rather than merely discouraged.

*Consequence — this is a deferral, not a decision that caching is unnecessary.* With the call count now two to three times what revision 1 measured, the case for a cache is stronger than it was. If a benchmark on a real workload shows it matters, the fix is a snapshot inside `Resolver` with no change to `Value`, and the design questions it reopens (TTL length, timestamp placement, multi-flag consistency, snapshot immutability) are recorded here so they do not have to be rediscovered. A snapshot would also resolve all three of a Mongo write's switches from one read, which is the case where the cost is highest and where R19's tearing window lives.

### D5. `otel-flags` is a published shared module, not four vendored copies

The four `internal/flags` copies are deleted. Their contents move to a new module:

```
otel-flags/
├── go.mod          module github.com/akira-core/instrumentation-go/otel-flags
├── flags.go        env tri-state read, Resolver, provider install, FlagDomain, master switch
├── flags_test.go   one copy, not four
├── version.go      instrumentationVersion, for the release guard
├── CHANGELOG.md
└── README.md
```

The four instrumentation modules `require` it.

**The forcing requirement is the provider singleton.** Four `internal/` packages in four modules cannot import each other, share a lock, or share a `sync.Once`. Revision 1 acknowledged the consequence and accepted it: two modules evaluating for the first time concurrently could both observe `NoopProvider` and both register, leaving the SDK to replace one with the other and shut the loser down — one live provider eventually, two briefly, and one duplicated first fetch. "Eventually one" is not the same guarantee as "one", and the difference is observable as a doubled relay fetch and a shutdown log line at startup.

Go's minimal version selection resolves one module path to exactly one version per build (per major version), so one shared module means one package instance, one `sync.Once`, one provider — a structural guarantee rather than a convention. Nothing else available reaches it.

**What else this removes.** The byte-identical rule, its "maintained by code review, not by a check" caveat, the drift table enumerating what a single diverged copy would silently do, the proposed-but-unimplemented CI hash check, and three redundant copies of `flags_test.go`. This is the largest simplification in the change, and it arrives as a side effect of the provider requirement rather than as its own goal.

**The module-vocabulary rule survives, restated.** `otel-flags` may not name anything **module**-scoped. Process-scoped names belong there — the master switch's environment variable and flag key, the three `OTEL_INSTRUMENTATION_GO_FLAGS_*` provider variables, `OTEL_SERVICE_NAME`, `FlagDomain` — because they are properties of the binary. Module flag keys, module environment variable names, and module hardcoded defaults stay in each module's own `env_flags.go` and reach the resolver through `WithFlagKeys` and the `local` parameter of `Value`. The resolver never learns that a key and a variable are paired.

```go
// otel-flags — process-scoped only.
const (
    EnvGlobalTracing     = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"
    FlagKeyGlobalTracing = "otel-instrumentation-go-tracing"

    EnvFlagsEndpoint     = "OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT"
    EnvFlagsAPIKey       = "OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY"
    EnvFlagsPollInterval = "OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL"

    EnvServiceName = "OTEL_SERVICE_NAME"
    FlagDomain     = "otel-instrumentation-go"
)

func Lookup(name string) (value bool, set bool, err error)   // D14
func MasterLocal() (bool, error)                             // env, else true
func MasterEnabled(local bool) bool                          // relay > local
func RelayPossible() bool                                    // D7
```

```go
// otel-mongo/otelmongo/env_flags.go — module-scoped only.
const (
    envMongoTracingEnabled     = "OTEL_MONGO_TRACING_ENABLED"
    envMongoPropagationEnabled = "OTEL_MONGO_PROPAGATION_ENABLED"

    defaultMongoTracing     = false
    defaultMongoPropagation = false
)

var mongoResolver = otelflags.NewResolver(
    otelflags.WithFlagKeys("otel-mongo-tracing", "otel-mongo-propagation"),
)
```

**Local development uses a root `go.work`; CI verifies with `GOWORK=off`.** A published module cannot carry a `replace` directive — consumers ignore it, so the four modules' `go.mod` files must name a real `otel-flags` version. A workspace file at the repo root lets all seven modules build against the working tree during development without any committed `replace`, and it is never published. CI must not use it: each module's `go build` / `go test` / `golangci-lint` step sets `GOWORK=off` so it verifies the module exactly as a consumer would resolve it. This is the only new repo-level infrastructure the change introduces, and § Migration records the release ordering it forces.

*Alternatives considered.* Keeping four copies and coordinating through the SDK registry is revision 1's shape and does not meet the guarantee, for the reason above. Moving only the provider install into a shared module and leaving the environment read and the resolver vendored would fix the singleton while keeping the byte-identical rule, the drift table and the four test copies — the cost of the shared module is paid either way, so paying it for a partial result is strictly worse. Putting each module's keys, variables and defaults into `otel-flags` as a table would make the modules thinner still, but it moves module vocabulary into the shared file and makes adding a module or renaming a key a shared-module release; rejected for the same reason the rule exists.

### D6. `Gate` is deleted

`natsGate`, `wsGate`, and `propEnabledGate` are the only users of `flags.Gate` — four call sites, not three, because `otel-mongo` v1 and v2 each carry their own `propEnabledGate`. All four are replaced by `Resolver`. `Gate`, `NewGate`, and `ResetForTest` are removed rather than left as dead code.

Unchanged from revision 1, except that the removal now happens as part of the move to `otel-flags` rather than in place, and that `otel-flags` is a public module — so `Gate` disappearing is not merely an `internal/` deletion, it is a symbol that never becomes public.

### D7. Implementation selection keys on `relayPossible`

```go
relayPossible := otelflags.RelayPossible()
useTracedImpl := relayPossible || (masterLocal && tracingLocal)
```

```go
// otel-flags
func RelayPossible() bool {
    return strings.TrimSpace(os.Getenv(EnvFlagsEndpoint)) != "" ||
        openfeature.NamedProviderMetadata(FlagDomain).Name != "NoopProvider"
}
```

Revision 1 could key construction on the fully static `gate1 && EnvEnabled(moduleEnv)`, because a relay that can only revoke can never need an implementation the environment did not authorise. Under D2 it can, so that expression is unsound: a module whose environment says off must still be able to start tracing when the relay says so.

Keying on the relay verdict itself is impossible — it changes, and construction happens once. Keying on nothing and always allocating the instrumented implementation is sound but expensive: every consumer of these libraries, including the large majority that will never configure a relay, would allocate both implementations, register `shared.NewCommandMonitor` on every MongoDB client, and pay D4's per-operation evaluation for a control plane that cannot exist.

`relayPossible` is the sound static approximation. If no endpoint is configured and no provider is bound to our domain, `Client.Boolean` can only ever return the value passed to it, so the relay is not merely silent — it is structurally incapable of speaking. The module then resolves from `env > option > default` alone, allocates the instrumented implementation only if that answer is on, and never touches the OpenFeature SDK. **Every configuration that took the zero-cost passthrough path before this change still takes it**, which is the property revision 1 achieved by a different route and this revision was at risk of losing.

When `relayPossible` is true, both implementations exist and the per-operation resolution selects between them, exactly as revision 1's traced path did.

**Everything derived from a wrapper inherits its decision.** `oteljetstream.New(conn)`, `Client.Database()` and `Database.Collection()` are called after their source was constructed, and they SHALL inherit its implementations and its gate state rather than re-resolving. Re-resolving would let a passthrough-only `Conn` hand out an instrumented JetStream wrapper that has no tracer to use.

**`relayPossible` is resolved per construction, not memoized process-wide.** A `sync.Once` would be cheaper and would guarantee that every wrapper in the process agrees, but it would freeze the answer at whichever wrapper happened to be built first — which in a test binary is whichever test ran first, making every subsequent relay test unreachable without the reset hook D6 deletes. Per-construction resolution costs one `os.Getenv` and one registry read at construction only, never on a hot path (D8), and it makes the documented ordering rule work: install your provider, *then* construct wrappers.

*Consequence — an application that installs its provider after constructing a wrapper loses relay control for that wrapper.* This is a new ordering requirement that revision 1 did not have, and it must be stated in `feature-flags.md` next to the wiring snippet. A wrapper built before the install sees `relayPossible == false`, resolves statically, and never consults the relay for the rest of its life. Applications using the zero-code path (D17) are unaffected, since the endpoint variable is set before the process starts.

*Consequence — a relay-configured deployment loses the zero-cost passthrough everywhere.* Setting `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` makes every module in the process allocate both implementations, register the Mongo command monitor, and resolve two or three flags per operation — even for modules the operator has no intention of ever enabling. That is the price of the control plane and it is charged per process, not per module.

### D8. Long-lived objects consult the flags per call; no connection is ever static

Because the relay must be able to change a running process's behaviour, **no wrapper may cache a relay-backed verdict**. This holds for connections constructed with `WithTracingEnabled`: that option supplies the module tier's `local` value, which D2 passes as the evaluation default on every call — it is an input to each resolution, not a substitute for one.

**No environment variable is read on any hot path.** Construction fixes `masterLocal`, `tracingLocal`, `propLocal` and `relayPossible`; what remains per operation is two or three `Boolean` calls and nothing else. Every `os.LookupEnv` in the flag path happens once, during construction. This is worth stating as an invariant because it is cheap to check and easy to lose: a future change that re-read a module switch inside a gate would reintroduce a per-operation syscall-shaped cost without changing any behaviour, so nothing would fail.

`otel-gorilla-ws`, the one cached-gate module, converts cheaply: `if !c.featureEnabled` becomes `if !c.featureEnabled()`. The strategy-split types need a structural change — each facade type holds **both** implementations and selects per call:

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

`otelnats.Conn` needs the same treatment. It has always been a strategy split (`connImpl` = `directConn` / `tracedConn`, chosen once at construction), so it gains a `direct`/`traced` pair and an `impl()` selector. `oteljetstream` wrappers, consumers, `MessagesContext` and `MessageBatch` forwarders derive their gate from the `Conn` and re-read it per message.

`Cursor` and `ChangeStream` follow the same dual-implementation shape rather than inheriting a fixed choice from the call that produced them. This matters most for `ChangeStream`, which can outlive many flag changes. Both are structurally able to: `traced.Cursor` and `traced.ChangeStream` hold only a tracer, a propagator and their propagation flag — no per-call span state — so the facade can build an instrumented and a passthrough wrapper around the same raw driver object.

`SingleResult` is the one exception, and it is forced rather than chosen. `traced.SingleResult` holds the live `FindOne` span and ends it exactly once on the first of `Decode`/`TraceContext`/`Raw`. A `FindOne` that ran through the passthrough path started no span, so there is nothing to construct an instrumented wrapper around. Selecting per call would also be incoherent in the other direction: a change between `FindOne` and `Decode` would strand an already-started span that the passthrough path would never end. `SingleResult` is the result of one already-executed operation, so the flag value at `FindOne` time is the only meaningful answer.

*Consequence.* `collectionImpl` returns raw driver types for `Find`/`Aggregate`/`Watch` and the facade constructs both wrappers itself; `FindOne` keeps returning a `shared.SingleResultImpl` alongside its raw result. `traced.Collection`'s exported `PropagationEnabled` field is a `func() bool`, which facade-package tests that build `traced.Collection` literals must follow.

**Per-operation tracing/propagation consistency (R5).** Within a single public operation, the tracing decision used to select `impl()` SHALL be the same value passed into `resolveDocumentPropagation` / `propagationOn` — callers MUST NOT re-resolve tracing for that operation's propagation decision. The two or three relay verdicts an `otel-mongo` operation needs are consecutive `Boolean` calls; the microsecond window between them is R19, accepted, and fail-safe because a false master or tracing verdict short-circuits propagation. Client and Database share one `gateState` helper so the rule is not hand-copied four times (R16).

**Facade `impl()` selection (R14) — WONTFIX.** The six `impl()` methods each hand-roll `if traced != nil && tracing() { return traced }; return direct`. Factoring that through a generics helper needs both an interface type parameter and a comparable concrete one just to express the nil check — a signature longer than the four lines it removes, on the most-read entry point of the facade. Declined.

### D9. `otel-gorilla-ws` negotiates on the handshake-time effective value

Negotiation happens during the handshake and cannot be revisited, so it is gated on the connection's effective tracing value resolved **once, immediately before the handshake**:

```go
negotiate := gate.tracing()      // master && module, relay included, evaluated once
```

Revision 1 could gate this on a fully static expression, because a relay that can only revoke can never need an envelope the environment did not authorise. Under D2 it can, and there are only two coherent answers:

- Negotiate on the current effective value, accepting that a relay enable reaches only connections opened afterwards.
- Negotiate whenever `relayPossible`, so the envelope capability is always in place and a relay enable reaches live connections — at the cost of `marshalWire` on every write and the `tryUnmarshalWire` probe on every read for every relay-configured deployment, including one that never enables the module.

**The first is chosen.** The wire cost of the second is unconditional and permanent, paid by deployments that configure a relay for `otel-mongo` alone, and it changes the bytes on the wire between library peers as a function of a variable that has nothing to do with WebSockets. The cost of the first is bounded and describable: a relay enable applies to new connections, and an operator who needs it on existing ones cycles them.

*Consequence — the WebSocket module's relay control is asymmetric, and both halves must be documented.*

- **Enabling** reaches only connections opened after the change. A long-lived connection opened while the module was off never carries the envelope; `WithTracingEnabled(true)` cannot restore it either, since it also resolves before the handshake and a peer that did not negotiate `otel-ws` will not parse one. Such a connection can still emit local send/receive spans once the flag is on — it simply cannot inject or extract.
- **Disabling** reaches every connection immediately for spans and for inject/extract, but the envelope keeps being written, because the peer parses every frame as one. A revoked `otel-gorilla-ws` still runs `marshalWire` on every write and the probe on every read; it is the one module of the four that does not return to the zero-cost path, and removing that wire overhead requires cycling the connection. `feature-flags.md` states this next to "set its relay flag to `false`", because an operator pulling the brake during a latency incident would otherwise expect relief that does not come.

**Envelope follows negotiation outcome, not feature-on aspiration (R1).** `Conn`'s wire fact means "otel-ws was negotiated (or proven via subprotocol)", not "this process might want spans".

- `Dial` / `Upgrader.Upgrade` set it from the handshake result.
- `NewConn` has no handshake: it sets it from `isOTelWireProtocol(conn.Subprotocol())`. Callers that manage the handshake themselves must leave a correct negotiated subprotocol on the raw conn. There is **no** `WithOTelWSNegotiated` escape hatch — that would reintroduce force-envelope wire corruption.
- That instruction has to be followable, and today it is not: the token (`otelWSProtocol`), the `otel-ws+<app>` composite form and the predicate (`isOTelWireProtocol`) are all unexported, while `otel-ws.md` already publishes them as a wire contract. Two additive symbols close the gap without reopening the escape hatch, because neither can force an envelope onto a peer that did not negotiate one:

  ```go
  // SubprotocolOTelWS is the subprotocol token this package negotiates.
  const SubprotocolOTelWS = "otel-ws"

  // IsOTelNegotiated reports whether NewConn will enable the envelope on conn.
  func IsOTelNegotiated(conn *websocket.Conn) bool
  ```

  A stock `websocket.Dialer`/`Upgrader` can only reach the bare `otel-ws` form — gorilla echoes exact matches — so the `otel-ws+<app>` composite remains exclusive to `Upgrader.Upgrade`; documented alongside the constant.
- **The R7 clamp applies to the write path only.** `configureConn` clamps the *write* decision with the negotiated fact, so a process that never negotiated never emits an envelope. It must **not** clamp the read path. Whether the peer envelopes is a fact established by the handshake; our gate is a local policy, and applying policy to the fact is what produced the defect: on a connection that proved `otel-ws` with the feature off, `ReadMessage`'s fast path (`conn.go:190-193`) hands the peer's `{"header":…,"data":…}` bytes to the application unparsed. `Conn` therefore records the wire fact in its own field, unclamped, and the read path unwraps whenever that field is set. The write side stays clamped and the asymmetry is safe in that direction, because a peer receiving a raw frame falls back to the payload. Unwrapping is `json.Unmarshal` with the headers discarded — no span, no attribute build, no propagator call — so the disabled-mode invariant is untouched.

**The probe is not byte-transparent, and must be made so.** `tryUnmarshalWire`'s legacy branch unmarshals any non-empty JSON object into a `map[string]json.RawMessage`, deletes `traceparent`/`tracestate`, and re-marshals (`message.go:76-101`). Go serialises maps with keys sorted, so an ordinary JSON payload carrying neither trace key comes back reordered and whitespace-normalised — semantically identical, byte-wise different, and wrong for any caller that hashes or signature-verifies the frame. A message with neither key is by definition not a legacy envelope, so the branch SHALL return `ok=false` when both are absent, leaving the original bytes untouched.

*The envelope shape is reserved.* The envelope branch matches any object with a `header` of all-string values and a `data` member, so an application payload of that shape on an `otel-ws` connection is unwrapped and its outer structure discarded. Tightening the match is rejected: it would make any future header member added by the JS packages fall into the legacy branch, which is worse. `otel-ws` is a negotiated protocol and `otel-ws.md` publishes the envelope, so `{"header":…,"data":…}` is a reserved wire structure — stated there, since that document currently does not say so.

*Consequence — no-relay deployments see the previous release's wire, exactly.* With `relayPossible` false the effective value is `master && module` from environment and options alone, which is what `0.7.0` computed. The wire changes only for deployments that configure a relay and use it to enable the module.

### D10. Mongo document helpers carry no gate

`ContextFromDocument` and `ContextFromRawDocument` carry no feature-flag gate at all. They read a `_oteltrace` field out of a document the caller already holds, run `propagator.Extract` on it, and return the span context it encodes. They start no span, allocate no attributes, initialise nothing in the OTel SDK, and write nothing anywhere.

The flags exist to stop the library doing work **on the caller's behalf**. `Collection.InsertOne` is called for the business operation and gets instrumented as a side effect the caller never asked for at that call site; a switch is exactly right there. These two are called only when the caller wants trace extraction and for no other reason. Gating something whose sole purpose is the thing being gated leaves the caller no way to express what they already expressed by calling it.

The comparison that settles it is with `Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace`, which look superficially similar and **are** gated:

| | emits telemetry | writes to the document | gated | still extracts when the flag is off |
|---|---|---|---|---|
| `Collection.InsertOne` and siblings | CLIENT span | `_oteltrace` | yes | — |
| `Cursor.DecodeAndTrace` | `mongo.cursor.decode` span | no | yes | **no** (`direct.Cursor.DecodeAndTrace` returns `ctx` unchanged) |
| `ContextFromDocument` | no | no | **no** | **yes** |
| `ContextFromRawDocument` | no | no | **no** | **yes** |

`DecodeAndTrace` starts and ends a real span on every call, so it belongs under the switch. The package-level pair does not, so it does not.

The last column is the one an operator has to read, and it is why the table gains it: without it, both ungated rows look inert. **Turning a module off does not stop trace-context extraction.** A caller who wants linking to survive the library being silenced writes `Decode` + `ContextFromDocument` instead of `DecodeAndTrace`, and gets it. `feature-flags.md` § *What is not gated* says this in those words.

*Consequence — the invariant is about gated paths.* The disabled-mode invariant's "no propagator inject/extract" clause is scoped to code the flags govern. `propagation` is OTel **API**, not SDK, so nothing in the compiler-enforced `internal/direct` boundary or the CI grep is weakened.

*Consequence — BREAKING.* A process with every switch off previously got a zero `SpanContext` and `false` from `ContextFromDocument`, and an unmodified `ctx` from `ContextFromRawDocument`. It now gets the document's real span context. The direction is more capability, not less, and only code that calls these functions is affected.

*Consequence — no relay evaluation per document.* These two are the per-document call in a change-stream or cursor loop, so under a gate they would pay D4's cost once per document on top of whatever the operation itself resolved — two to three evaluations under revision 2's call count. Ungated, they resolve nothing.

**What `_oteltrace` costs, and why its default is `false`.** The field is not observability-only: it is roughly 90 bytes of BSON (more with a `tracestate`) appended to the document by `InsertOne`, `InsertMany`, `UpdateOne`/`UpdateMany`, `UpdateByID`, `ReplaceOne` and `BulkWrite`. Nothing in this module ever removes it — there is no strip on read, so once written the field is visible to the application on every subsequent read, permanently. Turning the flag back off does not undo anything; cleanup is a `$unset` migration, and against a collection with `$jsonSchema` + `additionalProperties: false` the write fails outright.

Revision 1 protected this by making the relay incapable of enabling. Revision 2 gives that up, so the protection is carried by the tier's hardcoded default of `false` instead: propagation is the one switch that no amount of *absence* can turn on. Something has to say `true` — an option, an environment variable, or a relay flag someone deliberately created — and § Risks records that the relay is now one of those three.

### D11. Flag keys are fixed, kebab-case, and paired with an environment variable

| Flag key | Paired environment variable | Option | Default | Modules |
|---|---|---|---|---|
| `otel-instrumentation-go-tracing` | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | — | `true` | all (process-wide veto) |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `WithTracePropagationEnabled` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-gorilla-ws` |

The master gains a key in this revision; revision 1 had none, because a switch that can only revoke has nothing to say that the environment cannot already say. Its default of `true` means creating it on the relay with the value `true` has no effect at all — the README and `feature-flags.md` must describe it as "set to `false` to stop everything", never as an enable.

*Alternatives considered.* Reusing the environment variable strings as flag keys would give operators one identifier instead of two, but `UPPER_SNAKE` is foreign to GO Feature Flag configuration and welds the two namespaces together. Making keys overridable through additional environment variables would let a site match an in-house naming convention, but the relay configuration is written by that same site.

### D12. Evaluation context is the application's, except for one attribute on the zero-code path

An application that installs its own provider owns its evaluation context outright: it calls `openfeature.SetEvaluationContext`, the SDK merges that into every evaluation, and the library adds nothing.

The zero-code path (D17) cannot do that — `SetEvaluationContext` is Go code, and the whole point of the path is that there is none. Left empty, its evaluation context makes every relay rule untargetable, so `otel-mongo-tracing: false` lands on **every process in the fleet**. So when — and only when — D17 installed the provider, the resolver supplies one attribute:

```go
if svc := os.Getenv("OTEL_SERVICE_NAME"); svc != "" {
    r.evalCtx = openfeature.NewTargetlessEvaluationContext(
        map[string]any{"service.name": svc})
}
```

Three things make this narrow enough to be safe:

- **`OTEL_SERVICE_NAME` is not a guess.** It is the OpenTelemetry specification's own variable, already set by any deployment running an exporter. `OTEL_RESOURCE_ATTRIBUTES` (a spec format to parse) and hostname (genuinely arbitrary) stay out.
- **It is passed at the invocation site, never through `SetEvaluationContext`.** The SDK merges *API → transaction → client → invocation*, so this composes with an application's global context instead of replacing it, and D1's rule against mutating global state holds.
- **It is confined to the D17 path**, which removes the one collision the merge order would otherwise create: invocation wins over global, so supplying it on the application-installed path could override a `service.name` the application set itself. A process on the D17 path has no global context to override.

Targeting matters more in this revision than in the last one, because the relay can now enable. Enabling a module fleet-wide to investigate one service is exactly the operation that should be scoped, and this attribute is what makes it scopeable:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  targeting:
    - query: service.name eq "checkout-api"
      variation: enabled
  defaultRule: { variation: disabled }
```

Per-request targeting remains a Non-Goal: the attribute is process-scoped and the resolver holds no request state.

### D13. Testing uses an in-memory provider

Tests install `memprovider.NewInMemoryProvider(...)` through **`SetNamedProviderAndWait(otelflags.FlagDomain, …)`**, mutate a flag value, and assert the next operation observes it.

**Named, not default.** A named provider on `FlagDomain` outranks the default for our clients, so a test that installs a default provider is silently shadowed the moment any earlier test in the same binary triggered an auto-install. `clientOnce` makes that install a once-per-process event that no test can undo, since D6 deletes `ResetForTest`. Installing on the same domain the production path resolves through removes the shadowing.

**Install the provider before constructing the wrapper.** D7 resolves `relayPossible` at construction, so a wrapper built before any provider exists and with no endpoint variable set resolves statically for its whole life and will never observe a flag change. This replaces revision 1's "keep the endpoint variable unset" rule with a stronger one: ordering is now part of the test contract, not just hygiene. Tests that exercise D17 itself set the endpoint variable deliberately and assert on the registration, in isolation.

Because D4 resolves per call, no clock injection, no reset hook and no waiting are involved: the change is visible on the very next operation. Because the provider and the environment are both process-global, tests that touch them must not call `t.Parallel`.

The asymmetry to exercise is now the opposite of revision 1's. A relay `true` against an environment that says nothing must produce spans, because the relay is authoritative; a relay `false` must stop a running connection; and a master relay `false` must stop a module whose own key says `true`.

D3 removes the mutual-exclusion rule, so the roughly 89 existing call sites that set an environment variable *and* pass an option no longer fail — they must be rewritten to assert that the **environment variable wins**. That is the opposite of released `0.7.0`, so these are the tests most likely to encode the old assumption silently, and each needs reading rather than mechanical editing.

One integration test stands up a real GO Feature Flag relay proxy container and drives one module end to end, verifying that the wiring recipe in the documentation resolves against a real relay: provider construction options, endpoint format, and flag keys matching a real relay configuration file. It must assert **both** directions — enable from an environment that says nothing, and revoke — since both are now reachable. Only one module is covered; the wiring is identical across the four.

A full harness-level assertion that spans stop reaching the OTLP sink after a flag change is deliberately excluded. It would have to outwait the provider's poll interval and the exporter's batch timeout, making it a timing race; its two halves are already covered separately.

This revision's decisions each need coverage:

- **D2 precedence** — for each switch: relay beats option, option beats env, env beats default; a missing key, an evaluation error and an absent provider all fall through to the local value.
- **D3** — `WithTracingEnabled` cannot override a master veto; setting the environment variable and passing the option is legal and the **environment variable** wins; with the variable unset, the option decides, so two connections in one process can still differ.
- **D5** — one provider instance serves all four modules; constructing wrappers from every module registers exactly one.
- **D7 `relayPossible`** — false with no endpoint and no provider: no SDK client is created, the passthrough implementation alone is allocated, no command monitor is registered; true via the endpoint variable; true via a pre-installed provider; a wrapper built before the install stays static.
- **D9** — negotiation follows the handshake-time effective value; a connection opened while off does not gain the envelope when the relay enables the module; a revoked connection keeps the envelope. `SubprotocolOTelWS` and `IsOTelNegotiated` agree with what `NewConn` does. A conn that proved `otel-ws` with the feature off returns the *unwrapped* payload from `ReadMessage` and still writes raw. A JSON-object payload carrying neither trace key comes back byte-identical, key order included.
- **D12 `service.name`** — attached on the auto-install path when `OTEL_SERVICE_NAME` is set, absent otherwise, never attached on the application-installed path.
- **D14 / D15** — every invalid value, including the empty string, fails construction with an error naming the variable and the value; every truthy and falsy spelling is accepted; unset falls through.
- **D17** — fires with the endpoint set and no provider installed; stands down when a provider already exists; a malformed `_POLL_INTERVAL` warns, falls back to 60 s and still installs; an unset endpoint installs nothing.
- **Open question 1** — a document already carrying `_oteltrace`, re-injected, yields exactly one occurrence and extraction returns the new value.

### D14. Environment values are a strict tri-state

```go
// otel-flags
func Lookup(name string) (value bool, set bool, err error) {
    v, ok := os.LookupEnv(name)
    if !ok {
        return false, false, nil
    }
    switch strings.ToLower(strings.TrimSpace(v)) {
    case "1", "true", "yes", "on":
        return true, true, nil
    case "0", "false", "no", "off":
        return false, true, nil
    default:
        return false, true, fmt.Errorf("%w: %s=%q (accepted: 1,true,yes,on / 0,false,no,off)",
            ErrInvalidFlagValue, name, v)
    }
}
```

Three outcomes, and only three: **unset** (this source has no opinion, fall through), **a recognised value** (this source decides), and **anything else** (a configuration error, reported to the caller).

Revision 1 collapsed the third case into "disabled, with a warning". Under a conjunctive model where everything defaulted off that was safe: a typo produced the same result as the default. Under a precedence ladder it is not, and the direction reverses per switch — a typo in the master variable would read as `false` and silently stop the whole process, where the default is `true`. Warning and continuing means the strongest switch in the design can be turned off by a misspelling, and the only evidence is one `slog.Warn` in a startup log.

An error is the honest report. All four modules' option-accepting constructors already return one — `otelmongo.Connect` / `ConnectWithOptions` (v1 and v2), `otelnats.Connect` and siblings, `otelgorillaws.Dial`, `Upgrader.Upgrade`, and `NewConn` after D16 — so there is a channel for it everywhere it can arise.

**The empty string is an error too.** `export VAR=` reads as "set, to nothing", and the two readings available for it are both wrong somewhere: treating it as `false` makes an unexpanded `${SOMETHING}` template variable silently express an opinion the deployment did not have, and treating it as unset silently reverses the meaning for every deployment that used it as an off switch under `0.7.0`. Failing makes the ambiguity visible at the only moment anyone can act on it. The rule is one sentence — *set it to a recognised value or do not set it* — and it has no exceptions to remember.

**The error names the variable and the value**, so the fix does not require reading this document. The API key is never included in any message.

*Consequence — BREAKING, and it can stop a process from starting.* A deployment carrying `OTEL_MONGO_TRACING_ENABLED=enabled`, `=2`, `=y` or `=` fails at the first constructor instead of degrading. In Kubernetes, an unexpanded template variable is a realistic way to reach the empty-string case. This is the sharpest edge in the change and § Migration gives it a pre-upgrade check: grep the deployment configuration for every `OTEL_*_ENABLED` variable and confirm each value is in one of the two lists.

*Alternatives considered.* Warning and treating as unset — falling through to the next source — keeps startup robust and matches the "no opinion" reading. It was rejected because it makes the same input mean different things at different tiers and hides a real misconfiguration behind a log line the deployment may not read; the failure then presents days later as "spans disappeared" or "spans appeared", with nothing pointing at the typo. Warning and treating as `false`, revision 1's rule, has the same invisibility plus the master-veto reversal above.

### D15. Invalid configuration fails at construction, with a named error

`otel-flags` exports one sentinel:

```go
var ErrInvalidFlagValue = errors.New("otel-flags: invalid boolean value")
```

Every module wraps it with its own package prefix, and every constructor returns it. Because `otel-flags` is a published module rather than an `internal/` package, consumers can `errors.Is` against the sentinel directly — which revision 1 could not offer, and which is why it needed one sentinel per module.

A caller can misconfigure more than one variable at once: one configuration file can carry every `OTEL_*_ENABLED` variable, and each is read independently. All of a constructor's reads therefore run before any error is returned, and the failures are combined with `errors.Join` in a fixed order (master first, then module tracing, then propagation). Returning the first failure alone would make the caller fix one and rediscover the next on the following run, which is the failure mode configuration errors are worst at.

Revision 1's `ErrTracingConfigConflict` and `ErrTracePropagationConfigConflict` are **deleted**. They reported a mutual-exclusion rule that D3 removes: with the option and the environment variable at different rungs of one ladder, supplying both is ordinary configuration, not a conflict.

### D16. `otelgorillaws.NewConn` returns an error

`NewConn(conn *websocket.Conn, opts ...Option) *Conn` becomes `(*Conn, error)`. It is the only option-accepting constructor in the repository that cannot report a failure, and D14 gives it one to report — an invalid environment value, on the entry point most likely to be reached by a caller who wrote their own handshake and never touched the rest of the configuration.

The decision survives revision 2 with its reason replaced: revision 1 introduced the error return for the mutual-exclusion conflict D3 has now deleted. The signature change stands on D14 alone.

*Alternatives considered.* A second constructor (`NewConnWithOptions`) or a renamed `WrapConn` keeps existing call sites compiling but leaves the failure undetectable exactly where it is most likely. Deferring the error to the first `WriteMessage`/`ReadMessage` avoids the signature change but turns a construction-time configuration mistake into a runtime error that a never-used connection never reports, and that callers must `errors.Is` to distinguish from a network failure.

*Consequence — BREAKING.* Every `NewConn` call site must change. In this repository that is four test call sites; the one known downstream consumer (`instrumentation-demo`) does not use `otel-gorilla-ws`.

### D17. The library installs a named provider from the environment when none exists

An application obtains relay control by setting environment variables. It writes no Go code, adds no import, and changes nothing but its deployment configuration.

```go
// inside Resolver.evaluator(), under the existing clientOnce
if endpoint := os.Getenv(EnvFlagsEndpoint); endpoint != "" &&
    openfeature.NamedProviderMetadata(FlagDomain).Name == "NoopProvider" {
    // ... construct, register (non-blocking), populate evalCtx per D12
}
r.client = openfeature.NewClient(FlagDomain)
```

Two conditions, both necessary. The endpoint variable is the operator's expression of intent; the `NoopProvider` check is what makes the install an *allowance* rather than a takeover — an application that installs its own provider before constructing any wrapper keeps it, and this path stands down.

The check is written against the **named** domain because `NamedProviderMetadata` falls back to the default's metadata when no named provider is bound. One call therefore covers all three ways an application can already have made its choice — a default provider, a named provider deliberately bound to this domain, or an earlier install by this same code — and only a process that has made none of them reads back `"NoopProvider"`.

| Setting | Source | Note |
|---|---|---|
| `Endpoint` | `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | Unset or empty ⇒ nothing is installed, no SDK state is touched, and `RelayPossible()` is false |
| `APIKey` | `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | Never included in a warning or error message |
| `FlagChangePollingInterval` | `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | `time.ParseDuration` only, default `60s` |
| `DataCollectorDisabled` | **hardcoded `true`** | D1's stall mechanism, now unmissable |
| `EvaluationType` | **hardcoded `INPROCESS`** | The unsupported remote mode is unreachable from this path |
| everything else | not exposed | `Headers`, `ExporterMetadata`, `HTTPClient`, `Logger`, and the six `DataCollector*`/`FlagCache*` fields, which the two hardcoded settings make inert anyway |

**Duration strings only, and a malformed one does not disable the flag plane.** `60` is rejected rather than read as 60 ms — the OTel convention of bare-integer milliseconds would turn a plausible value into a catastrophic misreading of a polling interval. A parse failure warns through `slog.Default()`, falls back to `60s`, and **still installs**. Note the deliberate asymmetry with D14: an invalid `_ENABLED` value fails construction, an invalid poll interval does not. The difference is that the interval has a safe fallback and the switch does not — refusing to install over a typo in a tuning knob would delete the entire control plane, the highest-severity outcome reachable from the lowest-severity mistake, whereas guessing at `OTEL_MONGO_TRACING_ENABLED=enabled` picks a behaviour nobody asked for.

**Why 60 s.** The provider's own default is 120 s (`evaluator/inprocess.go:21`, applied when the interval is `<= 0`). Two minutes is the wrong reaction time for a control plane whose job includes stopping an incident. At 60 s the poll is a conditional `GET` with an ETag returning 304 in the steady state, so the cost of halving it is negligible; D4's latency note records that this interval, not the resolver, is what flag-change latency is made of.

**Exactly one install, guaranteed.** Revision 1 had to accept that four vendored copies could race and both register. D5's shared module removes the possibility: one package, one `clientOnce`, one install. The paragraph revision 1 spent on replace-and-shutdown semantics is deleted rather than reworded.

*Alternatives considered — all failed on the same constraint.* The application may change `go.mod` only, not any `.go` file. Go initialises packages purely from the import graph, and `go mod tidy` deletes a `require` that nothing imports, so **no code can be made to run from `go.mod` alone** — not via `godebug` (stdlib toggles), `tool` (build-time), or build tags (a build-command and `.go` change). The trigger therefore has to live in a package the application already imports.

- *A separate module with a blank import* (`import _ ".../autoinstall"`) is the idiomatic Go answer and keeps the provider's dependency tree out of every consumer's build. One `.go` line, which is one too many. Note that D5 now creates a shared module anyway — but for the singleton, not for the trigger, and it is imported unconditionally by the four modules rather than blank-imported by the application.
- *A minimal in-house provider* on `net/http` and `encoding/json` keeps `go.mod` clean and needs no app code. Rejected on correctness: the relay's configuration format supports targeting rules, percentage rollouts and JSONLogic queries, and a minimal evaluator would silently ignore a flag change expressed as any of them.
- *The OFREP provider* has an effectively empty dependency tree and would evaluate correctly. Rejected because it has no cache and no poller — `internal/evaluate/resolver.go` issues one HTTP request per `Boolean` call, putting a network round trip on the path of every Mongo query and NATS publish.

*Consequence — `otel-flags`' `go.mod` gains the GO Feature Flag provider, and the four modules gain it transitively.* That brings roughly ten modules including `go-feature-flag/modules/core`, the ofrep provider, `bluele/gcache`, `diegoholiveira/jsonlogic`, `nikunjy/rules` and a full `antlr4-go/antlr` runtime into every consumer's build — including consumers that never set the endpoint variable. The cost is to `go.sum` length, vulnerability-scanning surface and licence review rather than to runtime, since the linker drops unreached code. D5 at least confines the declaration to one `go.mod` instead of four.

*Consequence — the poller outlives everything.* Nothing shuts the provider down: there is no handle to hand back on a path whose entire premise is that the application writes no code. One goroutine per process, ending with the process. An application that needs lifecycle control installs its own provider and owns it.

## Risks / Trade-offs

**The relay can enable instrumentation a deployment did not ask for.** This is the deliberate inverse of revision 1 and the change's largest risk. A misconfigured, compromised or stale relay can start `otel-mongo` writing permanent `_oteltrace` fields into the application's own documents — roughly 90 bytes per document across seven write methods, never stripped on read, cleanup by `$unset` migration, and a hard write failure against strict `$jsonSchema` validation. → Four partial mitigations, all stated in `feature-flags.md` and the module READMEs: the master veto (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` stops everything regardless of any relay value), the per-module environment variable (`OTEL_MONGO_PROPAGATION_ENABLED=false` cannot be overridden by application code, only by the relay — D3), the propagation tier's default of `false` (something must deliberately say `true`, and the relay key has to be created by the site that owns the relay), and `relayPossible` (a process with no endpoint and no bound provider cannot be reached at all).

**Relay control depends on construction order.** D7 resolves `relayPossible` when a wrapper is built, so an application that installs its own provider *after* constructing a wrapper leaves that wrapper permanently static. → A new documented ordering rule: install the provider, then construct wrappers. The zero-code path is unaffected, since the endpoint variable exists before the process starts. Tests carry the same rule (D13).

**A relay-configured deployment pays the dynamic cost in every module.** Setting the endpoint variable makes every module allocate both implementations, register the Mongo command monitor on every client, and resolve two or three flags per operation — including modules the operator never intends to enable. → Accepted in D7 as the price of a control plane, and bounded: it is charged per process, not per module, and a process without the endpoint keeps the previous release's zero-cost passthrough exactly.

**An invalid environment value stops the process from starting.** D14 turns `OTEL_MONGO_TRACING_ENABLED=enabled`, `=2`, `=y` and `=` into construction errors. An unexpanded `${SOMETHING}` in a Kubernetes manifest reaches the empty-string case. → BREAKING, with a pre-upgrade check in § Migration and an error message naming the variable and the value. The alternative — guessing — is what revision 1 did, and it puts the master veto one typo away from silently stopping a fleet.

**Per-operation cost is two to three `Boolean` calls.** Roughly 4–6 µs and 14–21 allocations on the instrumented path of a relay-configured process, against 2 µs and 7 in revision 1. → Measured, recorded in D4, and accepted as a deferral: a snapshot cache fits inside `Resolver` behind an unchanged `Value(i, local) bool` and would resolve all three of a Mongo write's switches from one read.

**Enabling `otel-gorilla-ws` does not reach live connections.** Negotiation is a handshake fact, so a relay enable applies only to connections opened afterwards; conversely a disable stops spans and inject/extract but not the envelope. → Accepted in D9 with both halves documented. The alternative — negotiating whenever a relay is configured — puts `marshalWire` and the read probe on every frame of every relay-configured deployment, permanently.

**The master relay key's `true` value does nothing.** Its default is `true`, so creating `otel-instrumentation-go-tracing: true` on the relay to "enable tracing" has no effect and will read as a broken flag. → Documented in D11, in `feature-flags.md`'s key table and in the README, as "set to `false` to stop everything".

**A relay change is not atomic across flags.** An operation reading master, module and propagation makes consecutive `Boolean` calls microseconds apart, so it can in principle see one old and one new value. → R19, accepted: the window is microseconds, and the composition is conjunctive, so a false master or tracing verdict short-circuits everything below it and the combination fails safe in the direction that matters.

**Flag changes are not immediate.** End-to-end latency is the provider's poll interval — 60 s by default under D17. → Stated in D4 and given its own section in `feature-flags.md`, replacing any wording that reads as "immediate".

**`otel-flags` couples the four modules' release cadence.** A change to the shared file now requires tagging `otel-flags` first and then bumping four `go.mod` files, where previously four copies changed in one commit. → Accepted in D5 as the cost of the singleton guarantee; § Migration gives the ordering, and a root `go.work` keeps local development and CI free of `replace` directives.

**One more module to release, review and scan.** `otel-flags` needs a version constant, a release-guard pattern, a CHANGELOG, a CI matrix entry and its own `README`. → Mechanical, carried in `tasks.md`, and offset by deleting four copies of `flags.go` and three copies of `flags_test.go`.

**Nothing shuts the auto-installed provider down.** One poller goroutine and one HTTP client live for the process lifetime. → Accepted in D17; applications needing lifecycle control install their own provider.

## Migration Plan

1. **Release `otel-flags` first.** Tag `otel-flags/v0.1.0`. It depends on nothing in this repo, so it can be tagged from the same commit that introduces it.
2. **Bump the four modules** to `require github.com/akira-core/instrumentation-go/otel-flags v0.1.0`, with no `replace` directive — a `replace` in a published module is ignored by consumers. Local development and CI use the root `go.work`; CI's per-module steps set `GOWORK=off` so each module is verified exactly as a consumer resolves it.
3. **Tag the four modules**: `otel-mongo/v0.9.0`, `otel-mongo/v2.9.0`, `otel-nats/v0.8.0`, `otel-gorilla-ws/v0.8.0`. Tags may be pushed sequentially; the release guard validates each against its version constant, and gains a fifth pattern for `otel-flags`.
4. **Before upgrading, audit every `OTEL_*_ENABLED` value in the deployment configuration.** D14 makes any value outside `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off` — including the empty string — a construction error. This is the one change that can stop a process from starting, and the check is a grep.
5. **Re-read what the defaults now mean.** Against released `0.7.0`:

   | Configuration | `0.7.0` | This change |
   |---|---|---|
   | nothing set | off | off |
   | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` only | off | off |
   | `OTEL_<MODULE>_TRACING_ENABLED=true` only | **off** (global gate closed) | **on** (master defaults to `true`) |
   | both set to `true` | on | on |
   | `WithTracingEnabled(true)`, no variables | on | on |
   | `WithTracingEnabled(true)` + `OTEL_<MODULE>_TRACING_ENABLED=false` | **on** (option won) | **off** (the variable wins) |
   | `WithTracingEnabled(false)` + `OTEL_<MODULE>_TRACING_ENABLED=true` | **off** (option won) | **on** (the variable wins) |
   | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` | off | off, and now unconditionally — it overrides options and per-module relay values |
   | `OTEL_MONGO_TRACING_ENABLED=` (empty) | off | **construction error** |

   Two behavioural changes to announce. The third row: a module variable that used to be inert without the global one now takes effect — it fixes a common "I set the flag and nothing happened" report, and it is still a change. Rows six and seven: the option no longer wins over the paired environment variable (D3), so an application that sets both flips. With the variable unset the option still decides, so per-connection differentiation is unaffected. `_oteltrace` is unaffected in every row — propagation defaults to `false` and needs its own explicit `true`.
6. **`otelgorillaws.NewConn` call sites must take the new error return** (D16). Two `otel-gorilla-ws` behaviours also change without a signature change, both fixes, both altering returned bytes in the affected case: `ReadMessage` on a connection that proved `otel-ws` with the feature off now returns the unwrapped payload instead of the peer's envelope bytes, and a JSON-object payload carrying neither trace key is returned byte-identical instead of re-marshalled with sorted keys (D9). `SubprotocolOTelWS` and `IsOTelNegotiated` are additive.
7. **Deployments adopting the relay** set `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` (plus `_API_KEY` and `_POLL_INTERVAL` if needed) and create the flags on the relay. No application code changes. Until a flag exists on the relay, the module runs at whatever its options and environment declare. Applications that install their own OpenFeature provider keep it and need not set the endpoint variable — but must install it **before** constructing any wrapper (D7).
8. **Deployments wanting per-service targeting** set `OTEL_SERVICE_NAME` (D12) and write the relay rule against `service.name`. Without it, a relay flag applies to every process in the fleet — which matters more now that a flag can enable.

**Rollback.** Pin the previous module version. There is no persisted state and no relay configuration that must be torn down — flags left on the relay are ignored by an older build. `_oteltrace` fields already written to documents are not removed by a rollback; that is a `$unset` migration either way. Rolling back `otel-flags` alone is not meaningful: it is only reachable through the four modules.

## Superseded decisions

### Revision 1 — the shape shipped in PR #27, replaced before release

| Original decision | Replaced by |
|---|---|
| The module env var is the OpenFeature evaluation default; the relay decides in both directions | D2 (revision 1) — the env var became a separate ANDed tier and the default was always `true`. **Restored in revision 2**, see below |
| `WithTracingEnabled` overrides the global switch in either direction | D3 (revision 1) — the two became mutually exclusive spellings of one tier. **Reversed again in revision 2**: the option moved to the module tier and the mutual exclusion is deleted |
| A connection carrying `WithTracingEnabled` is fully static | D8 — no connection is static. Stands |
| `mongoPropagationEnvOnly()` serves static clients | Deleted. Stands |
| `ContextFromDocument` / `ContextFromRawDocument` follow the relay | D10 — ungated. Stands |
| Implementation selection keys on the global switch alone | D7 (revision 1) — `gate1 && EnvEnabled(moduleEnv)`. **Replaced in revision 2** by `relayPossible` |
| Detecting whether the relay "has an opinion" (double `Boolean` evaluation) | Unnecessary under revision 1; unnecessary again under revision 2, for the opposite reason — D2 passes the local value as the evaluation default, so silence and fallback are the same code path |
| A per-module snapshot behind an `atomic.Pointer` with a one-second TTL | D4 — deferred, not rejected. Stands, with a stronger case now that the call count has doubled |
| The application owns the provider outright; the library never installs one | D17 — the library installs a **named** provider when the environment asks and none exists. Stands |
| Each module resolves through its own domain | D5 — one process-scoped `FlagDomain`. Stands, and is now enforced by a single shared module |
| Revocation takes effect immediately | D4 — latency is the provider's poll interval. Stands |

### Revision 2 — this document

| Revision 1 decision | Replaced by | Why |
|---|---|---|
| The relay is revoke-only; the evaluation default is a literal `true` | **D2** — the relay is authoritative and the locally-resolved value is the evaluation default | "Turn this on so I can see what is happening" is the operation an incident calls for, and revision 1 made it impossible. The defaults chosen here (module `false`, master veto `true`) recover most of the safety without the capability loss |
| `gate1` is a single tier with two mutually exclusive spellings; both present is an error | **D3** — the master accepts no option; `WithTracingEnabled` feeds the module tier *below* its environment variable; the mutual exclusion and both `Err*ConfigConflict` sentinels are deleted | A per-connection option cannot coherently spell a process-wide switch. Removing the overlap removes the conflict rather than diagnosing it. Placing the option below the environment also diverges from `0.7.0`, deliberately: it is what stops application code overriding an operator's `OTEL_MONGO_PROPAGATION_ENABLED=false` and writing permanent fields into their documents |
| Implementation selection keys on `gate1 && EnvEnabled(moduleEnv)` | **D7** — keys on `relayPossible` | Sound only while the relay cannot enable. `relayPossible` is the sound approximation that still preserves the zero-cost passthrough for every process without a relay |
| Negotiation keys on a fully static capability | **D9** — keys on the handshake-time effective value | Same cause. The alternative (negotiate whenever a relay exists) charges every relay-configured deployment for the envelope forever |
| `EnvEnabled` warns and returns `false` for an unrecognised value | **D14 / D15** — a tri-state `Lookup` and a construction error, with the empty string included | Under a precedence ladder the safe direction is not uniform: a typo in the master variable would read as `false` and silently stop a fleet, because that tier defaults to `true` |
| Four byte-identical `internal/flags` copies; two modules may race to install a provider | **D5** — one published `otel-flags` module | "Exactly one provider" cannot be guaranteed across four packages that share no state. MVS gives it structurally, and deletes the byte-identical rule, its drift table and three test copies on the way |
| The startup window is a hazard: flags read `true` until the first fetch | **D1** — the window falls back to the locally-resolved value | A direct consequence of D2. The window can no longer enable anything that was not configured on; it can only delay a relay-driven enable |
| `otel-mongo`'s document-write safety rests on the relay being unable to enable | **D10** — it rests on the propagation tier's default of `false` | The relay can enable now. Something must deliberately say `true`, and the risk that it is the relay is stated in § Risks |
| No relay key for `gate1` | **D11** — `otel-instrumentation-go-tracing`, default `true` | A veto is only useful if it can be pulled without a redeploy. Its `true` value is inert, which the documentation must say |

## Post-review remediation (PR #27 grill, 2026-08)

Source: `reviews/code-review-pr-27-openfeature-dynamic-flags.zh-TW.html` and a decision grill on each finding. Items unaffected by either revision stand as decided.

| ID | Topic | Decision | Status after revision 2 |
|----|--------|----------|----------------------|
| R1 | `NewConn` wire corruption when capability on + feature off | Envelope only if negotiated/proven; fail → raw wire; local spans OK; no force-negotiated option; clamp with the negotiated fact (R7) | Stands (D9) |
| R2 | `MessageBatch` freezes flag at Fetch | Always return a dynamic batch wrapper; per-message gate re-check | Stands (D8) |
| R3 | `Resolver.refresh` last-store-wins + late `at` | Stamp `at` at evaluation start; no CAS/mutex | **Moot** — D4 has no snapshot |
| R4 | otel-ws negotiation vs "no provider ⇒ no change" | Was: keep behavior, document exception | **Withdrawn** — D7's `relayPossible` makes a no-relay process byte-identical to `0.7.0` on the wire |
| R5 | Mongo single-call-chain torn read of tracing | Pass resolved tracing into propagation; no internal recompute | Stands, and now covers the master verdict as well |
| R6 | JetStream per-message rebuild of tracer/attrs | Hoist tracer/prop/baseAttrs to construction; gate stays per-message | Stands |
| R7 | `capable` / `tracingEnabled` no choke-point clamp | Subsumed into R1; clamp applies to the write decision only | Stands (D9) |
| R8 | Dead second return of `collectionImpl` Find/Aggregate/Watch | Drop second return; stop throwaway `New*` in impls | Stands |
| R9 | `tracedMessagesContext.Next` not gate-first | Gate-first delegate to `directMessagesContext` | Stands |
| R10 | Gate/propEnabledGate doc drift | Full sync: CLAUDE, test comments, jetstream godoc, main spec | Stands, widened again by revision 2 |
| R11 | `WriteMessage` nil span + dual guards | Feature-off uses noop span; drop nil guards | Stands |
| R12 | NATS Consume path triple `impl()` per message | Resolve once per message; pass down | Stands, and matters more: each `impl()` now costs two `Boolean` calls |
| R13 | `dynamicTracingPossible` duplication / parallel refresh | `flags.GlobalTracingPossible()` | **Amended** — becomes `otelflags.MasterEnabled(local)`; the parallel-refresh half is moot with no refresh |
| R14 | Six copies of facade `impl()` selection | Was: generics `selectImpl` per mongo module | **WONTFIX** — declined in D8 |
| R15 | Five copies of relay test helpers | Move to `otel-testkit/harness` | Stands; `otel-testkit/harness/flags.go` must follow `otel-flags` |
| R16 | Client/Database `effective*` duplication | Shared `gateState` | Stands, and now carries `relayPossible` and the three local values |
| R17 | otelnats `impl`/`msgHandler`/`traceEventMsgHandler` policy | WONTFIX extract; lockstep comment only | Stands |
| R18 | Dead nil-handler guard in `tracedConsumeHandler` | Delete dead guard | Stands |
| R19 | Same-refresh sequential Boolean micro-torn pair | WONTFIX | Stands, and widens from two calls to three |

## Open Questions

Both items previously listed here are **in scope** and carried in `tasks.md` as work, not as questions:

1. **Read-modify-write produces a duplicate `_oteltrace` field.** `InjectTraceIntoDocument` appends unconditionally (`internal/shared/tracing.go:55`), so a document read into a `bson.M`, modified and written back with `ReplaceOne` carries the field twice. The **read** side is deterministically wrong regardless of what the server does: `ExtractMetadataFromRaw` uses `bson.Raw.LookupErr`, which returns the **first** match, so extraction yields the stale trace context from the original write and a read-modify-write loop pins the linkage there permanently. Inject removes any existing key before appending, in both modules.
2. **`CLAUDE.md` claims `_oteltrace` is "stripped on read".** No such code exists in either module, and D10 depends on the opposite being true. Corrected wherever it appears.

Items deliberately excluded stay Non-Goals: dynamic sampling rates, per-request targeting, a harness-level flag-flip E2E assertion, `Resolver` CAS/singleflight, parallel `Boolean` fan-out, and per-module OpenFeature domains.
