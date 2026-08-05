# `otel-flags` code review — August 2026

Scope: the `otel-flags` module (`flags.go`, `flags_test.go`) as of commit `8ac2112`, cross-checked
against `go-sdk@v1.17.2`, the GO Feature Flag provider `v1.1.1`, its in-process evaluator, and the
four consumer modules that carry the `relayPossible` short-circuit.

Fourteen findings. Severity is about what an operator loses, not about how hard the fix is: a switch
that silently stops being a switch outranks a race that needs two goroutines and bad luck.

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | High | A relay unreachable at process start leaves the provider permanently inert | Fixed |
| 2 | High | `InstallProvider` does not serialise against the environment auto-install | Fixed |
| 3 | Medium | `ResolveLocal` — the ladder's central rule — has no test in this module | Open |
| 4 | Low | Package doc cites a symbol, `installOnce`, that does not exist | Fixed |
| 5 | Low | `installProviderFromEnv`'s doc recommends `SetProviderAndWait`, which no longer stands the install down | Fixed |
| 6 | High | The `service.name` targeting attribute cannot be matched by any relay rule | Open — needs a decision |
| 7 | High | No targeting key is supplied, so percentage and progressive rollouts never apply | Open — needs a decision |
| 8 | High | With `FlagDomain` unbound, evaluation falls back to the application's default provider | Open — needs a decision |
| 9 | Medium | `Value` hardcodes `context.Background()` on the instrumented hot path | Open — needs a decision |
| 10 | Medium | `providerBound` compares two independently-locked SDK reads | Open |
| 11 | Medium | `captureLogs` shares a `bytes.Buffer` with the provider's async init goroutine | Open |
| 12 | Medium | `clearProvider` binds a no-op provider, so the domain-unbound state is untested | Open |
| 13 | Medium | The "do not evaluate when no relay is possible" guard lives in the consumers, not here | Open |
| 14 | Low | `Value` reads `r.evalCtx` as an operand of the call that initialises it | Fixed |

## Fixed in this pass

### 1. A relay unreachable at process start left the provider permanently inert (High)

`installProviderFromEnv` registered the provider with the non-blocking
`openfeature.SetNamedProvider`. In the SDK that discards the init result outright —
`openfeature_api.go`: `initCh := api.setNamedProviderWithContext(...); if async { return nil }` — and
the provider is bound whether or not initialisation succeeded.

What made that permanent rather than transient is one line inside the provider. `InProcess.Init`
fetches the whole configuration first and returns on failure **before** it creates the polling
goroutine:

```go
status, err := i.loadConfiguration(ctx)
if err != nil {
	return err            // ← no ticker is ever started
}
...
go func() { ticker := time.NewTicker(interval); ... }()
```

Nothing in the SDK retries a failed init, and nothing in `otel-flags` retried either: `installDone`
was already latched. So an endpoint that was down, or a DNS name that was not yet resolvable, during
a fleet rollout produced processes in which every switch resolved to its local value for the rest of
their lives, the kill switch was dead, and the only operator-visible signal was silence. Both the
module doc and the README described this as a window that ends at the first successful fetch.

**Fix.** The bind itself is unchanged — still asynchronous, still immediate, so no relay round trip
sits in front of the first instrumented operation and the startup window keeps exactly the semantics
the documentation describes. What is new is `watchProviderInit`: a goroutine that reads the domain's
provider state, and on `ERROR` binds a freshly built provider in place of the dead one, backing off
from one second to the poll interval. A failed in-process evaluator cannot be repaired in place —
there is no ticker to restart and no retry entry point — so recovery means a new provider instance.

It ends on the first `READY`, and stands down rather than fighting for the domain if an
`InstallProvider` call or a directly-bound application provider has taken it over in the meantime.
The first failure logs at warn with the endpoint, later ones at debug so an hour-long outage cannot
flood, and the recovery at info.

One detail worth keeping: the poll interval is read once, on the constructing goroutine, and passed
down. Re-reading it inside the goroutine made the malformed-value warning fire from a goroutine the
caller does not control, which turned finding 11 from a latent race into a reproducible `-race`
failure under `go test -count=3`.

### 2. `InstallProvider` did not serialise against the auto-install (High)

`installMu` exists to guarantee one provider per process, but `InstallProvider` never took it: it
called `SetNamedProviderAndWait` directly. An application calling it concurrently with the first
instrumented operation could interleave with `installProviderFromEnv`, whose `providerBound()` check
had already read "nothing bound". Either the application's provider was silently replaced, or the
SDK dispatched `shutdownOld` and `initNew` on separate goroutines such that the auto-installed
provider's `Shutdown` ran against a pre-closed channel and returned instantly, after which its
`Init` installed a fresh `stopPolling`, reset `shutdownOnce`, and started a polling goroutine with
no reachable handle — a relay poller for the life of the process.

**Fix.** `InstallProvider` now takes `installMu`, so every path that binds `FlagDomain` — the
auto-install, its retries, and an application's own call — goes through the one lock the module
already had for the purpose. It also latches `installDone`, so an application that installs its own
provider can never be followed by the environment auto-install, whichever of the two the process
reaches first. Holding the lock across the wait does block a wrapper being constructed concurrently
until the provider has initialised; that is the right outcome, since the alternative is a wrapper
resolving its first operations against a provider the call is in the middle of replacing.

A second lock was considered and rejected. Binding under a lock the construction path does not hold
would keep a stalled relay from blocking construction, but it inverts against `installMu` — the
auto-install binds while holding it — and a lock-order inversion in the install path is a worse
failure than a slow constructor. The retry goroutine binds asynchronously, so its critical section
is microseconds, not an HTTP timeout.

### 4, 5, 14. Documentation and evaluation order (Low)

- The package doc claimed "there is one instance of `installOnce` below". The mechanism is
  `installMu` plus `installDone`; the move off `sync.Once` was deliberate, because the tests need to
  clear the latch. Corrected to name what is actually there.
- `installProviderFromEnv`'s doc told applications that `SetProviderAndWait` "also makes this stand
  down". It does not, and has not since `providerBound` was narrowed to a `FlagDomain` binding —
  eight lines further down the same comment says so. An application following the old advice got a
  provider that was never consulted for instrumentation keys, an auto-install that fired anyway, and
  the startup window it was trying to close still open. Corrected to `InstallProvider` /
  `SetNamedProviderAndWait(FlagDomain, p)`.
- `Value` passed `r.evalCtx` as an argument of the same call expression that lazily initialises it
  via `r.evaluator()`. Go orders function calls left to right relative to each other but leaves a
  plain field load unordered against them, so the zero evaluation context could reach the first
  evaluation of a `Resolver`. The client is now bound to a local before the call.

## Open — mechanical, no decision needed

**3. `ResolveLocal` has no test here.** The symbol does not appear in `flags_test.go` at all. The
behaviours it uniquely owns are unpinned in this module: env outranks the option — the ordering the
package doc calls the case that forces the order, and the one thing standing between
`WithTracePropagationEnabled(true)` and permanent `_oteltrace` fields in an operator's documents —
the option outranks the default, and a `Lookup` error is returned even when a non-nil option was
supplied. The only coverage is indirect, in four modules that are versioned and released separately.

**10. `providerBound` compares two independently-locked reads.** `NamedProviderMetadata(FlagDomain)`
takes and releases the SDK's lock, then `ProviderMetadata()` takes it again. With the domain
unbound, the first returns the current default's metadata through the SDK's fallback; if the
application swaps its default provider between the two calls, the two names differ and neither is
`NoopProvider`, so the function reports a domain binding that does not exist. `RelayPossible()` then
returns true with nothing bound, and the auto-install stands down over an endpoint the operator
configured — the exact silent failure the fallback check was written to eliminate.

**11. `captureLogs` races the provider's init goroutine.** It swaps `slog.Default()` for a handler
writing into a local `bytes.Buffer`; `gofeatureflag.NewProvider` captures that logger permanently,
and initialisation runs on a goroutine that outlives the test's `Value` call. The test then reads
`buf.String()` from the test goroutine with no synchronisation. CI runs `go test -race`.

**12. `clearProvider` makes the dangerous state untestable.** It binds `NoopProvider` to
`FlagDomain`, and the SDK offers no unbind, so after the first call every test in the binary
evaluates against a bound no-op provider. The state that actually matters — `FlagDomain` absent from
the map, evaluation silently routed to the application's own provider — is unreachable from this
suite. `TestValue_NoProviderReturnsLocal` and `TestValue_MissingFlagReturnsLocal` both read as
covering it and cover neither, which is why finding 8 is invisible to CI.

## Open — needs a decision before any code changes

These four are not bugs in the sense of a wrong line; each asks what the module is supposed to
promise.

**6 and 7 together: targeting does not work, and there are two ways to fix it that mean different
things.** The evaluation context is either zero or `NewTargetlessEvaluationContext`, so there is
never a targeting key: a relay flag using `percentage` or `progressiveRollout` — the canonical way
to canary a kill switch — fails with `TARGETING_KEY_MISSING`, and `Client.Boolean` swallows that and
returns the local value. Separately, the one attribute the module does supply is unusable: the SDK
flattens it to the literal key `service.name`, but both supported query languages read a dot as a
nested-path separator, so the documented rule `service.name eq "checkout-api"` matches nothing.
Supplying the service name as the targeting key would fix both at once, at the cost of making every
process of a service bucket identically — a percentage rollout would become all-or-nothing per
service. A per-process identifier buckets properly but re-buckets on every restart. Which one is
right depends on what a rollout is supposed to mean here, and that is a product decision.

**8. Where an evaluation goes when `FlagDomain` is unbound.** `ForEvaluation` falls back to the
default provider, so `MasterEnabled` and `Resolver.Value` — both exported, neither checking
`RelayPossible` — evaluate instrumentation keys against whatever the application installed for its
own flags. Narrowing `providerBound` fixed detection, not routing. The four consumers hand-roll the
short-circuit that hides this; the module that owns the ladder does not enforce it (finding 13).

**9. `Value` hardcodes `context.Background()`.** `InstallProvider` accepts any provider, and the
README recommends it. With a remote-evaluation provider, every evaluation becomes an HTTP request on
the hot path of `InsertOne` or `Publish` — two per operation, three on a Mongo write — carrying no
deadline and no cancellation. The hardcoded `DataCollectorDisabled` and in-process settings that
prevent this stall apply only to the auto-install path.

**13. The guard belongs behind `Value`.** `if !g.relayPossible { return masterLocal && tracingLocal }`
is duplicated in `otel-nats/otelnats/env_flags.go`, both Mongo `gate_state.go` files, and
`otel-gorilla-ws/env_flags.go`. `NewResolver`'s doc asserts the property as though it were
guaranteed. A fifth instrumentation module that omits it — and the repository rule is explicitly
that adding one must not require changing `otel-flags` — pays full SDK evaluation cost per operation
and inherits finding 8.
