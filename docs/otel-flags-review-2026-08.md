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
| 3 | Medium | `ResolveLocal` — the ladder's central rule — has no test in this module | Fixed |
| 4 | Low | Package doc cites a symbol, `installOnce`, that does not exist | Fixed |
| 5 | Low | `installProviderFromEnv`'s doc recommends `SetProviderAndWait`, which no longer stands the install down | Fixed |
| 6 | High | The `service.name` targeting attribute cannot be matched by any relay rule | Fixed |
| 7 | High | No targeting key is supplied, so percentage and progressive rollouts never apply | Fixed |
| 8 | High | With `FlagDomain` unbound, evaluation falls back to the application's default provider | Fixed |
| 9 | Medium | `Value` hardcodes `context.Background()` on the instrumented hot path | Fixed |
| 10 | Medium | `providerBound` compares two independently-locked SDK reads | Fixed |
| 11 | Medium | `captureLogs` shares a `bytes.Buffer` with the provider's async init goroutine | Fixed |
| 12 | Medium | `clearProvider` binds a no-op provider, so the domain-unbound state is untested | Mitigated — see below |
| 13 | Medium | The "do not evaluate when no relay is possible" guard lives in the consumers, not here | Fixed |
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

## Also fixed in a second pass

**3. `ResolveLocal` now has a test here.** `TestResolveLocal` pins the three rungs it owns: env
outranks the option — the ordering the package doc calls the case that forces the order, and the one
thing standing between `WithTracePropagationEnabled(true)` and permanent `_oteltrace` fields in an
operator's documents — the option outranks the default, and a `Lookup` error is returned, with no
usable value, even when a non-nil option was supplied.

**6 and 7. Targeting works now, and the choice was per process.** A targeting key of
`<hostname>-<pid>` is supplied on every path, so `percentage` and `progressiveRollout` bucket per
process. The alternative — deriving the key from the service name — would have made every percentage
rollout all-or-nothing across a service, which is not what "enable tracing on 10% of the fleet"
means. Host plus PID rather than a random value so the verdict is stable across a restart of the
same container instead of being re-drawn each time.

Separately, `serviceName` is now supplied alongside `service.name`, both carrying `OTEL_SERVICE_NAME`
and both still confined to the auto-install path. Only the dot-free spelling is matchable: nikunjy's
parser splits `service.name` into attribute `service` with sub-attribute `name` and finds nothing,
and JSONLogic resolves `{"var": "service.name"}` as a path too. `service.name` stays because it is
the name a reader expects in an evaluation context. Both documents and the README now tell operators
to write the rule against `serviceName`.

**8 and 13. The guard moved behind `Value`.** `Value` short-circuits to the local value unless a
provider is bound to `FlagDomain`, so an unbound domain can no longer route an instrumentation key to
the application's own flag backend. The four wrappers keep their `relayPossible` short-circuits —
those also decide which implementation to allocate, which is a different question — but the module
that owns the ladder now enforces the rule for the wrapper that forgets and for direct callers of the
exported `MasterEnabled`.

**9. Evaluations against an application-installed provider are bounded at 250 ms.** The auto-installed
provider evaluates in process, so it skips the deadline entirely and the zero-code path pays nothing.
The caller's context is still deliberately not threaded through: cancelling a Mongo operation must not
change what an instrumentation switch resolves to, and a caller's deadline is about their work. If
propagating the caller's context is wanted later, it is an API change across four modules, not a
tweak here.

**10. `providerBound` validates its reads.** The default provider is read twice, around the named
read, and the answer is trusted only when it did not move — the trick a seqlock plays, for the same
reason. Three attempts; a process swapping its default provider faster than that gets the
conservative answer "bound", which leaves an endpoint inert rather than replacing a provider the
application may have just bound.

**11. `captureLogs` returns a mutex-guarded buffer.** Writes and reads now take the same lock, which
is what the provider's initialisation goroutine and the test goroutine needed between them.

## Mitigated, not fixed

**12. The domain-unbound state stays untestable.** The SDK offers no way to unbind a domain, so once
anything in the process binds `FlagDomain` — as `clearProvider` does — the state where evaluation
falls back to the application's default provider is unreachable for the rest of the test binary. The
`Value` guard is what makes that state harmless in production; no test in this package can enter it
to prove the guard fires. `clearProvider` now says so in its doc comment, so the next reader does not
mistake `TestValue_NoProviderReturnsLocal` for coverage of it.
