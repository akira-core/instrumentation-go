# `otel-flags` error handling and evaluation fallback — decisions

**Status: implemented in `otel-flags` 0.2.0** (unreleased at the time of writing; the four wrapper
modules require it from their own `0.8.0`/`2.8.0`). It records thirteen decisions taken on
2026-08-05, the reasoning behind each, and the options that were rejected. The reasoning is why it
stays: the code carries the rules, not the alternatives that were weighed against them.

Where the implementation differs from the letter of a decision below, it is noted inline.

It follows `docs/otel-flags-review-2026-08.md`, which found and fixed fourteen defects. This document
is about the design those fixes exposed rather than about the defects themselves.

## The constraints that are not ours to choose

Verified against `go-sdk@v1.17.2` and the OpenFeature specification while these decisions were taken.
They bound every option below.

- **Evaluation cannot raise.** Spec **1.4.10**: client methods "MUST NOT throw exceptions, or
  otherwise abnormally terminate", and "must always return the `default value` in the event of
  abnormal execution". `Resolver.Value` returning `bool` with no error is therefore not a style
  choice, and cannot be changed.
- **The default value is ours to pick.** Spec **1.7.6** and **1.7.7** say an evaluation before
  initialisation returns the default with `PROVIDER_NOT_READY`, and one against an irrecoverable
  provider returns the default with `PROVIDER_FATAL`. Which value we pass as that default is not
  specified, so the fallback direction is a design decision (see decision 4).
- **Failures are observable through the details variant.** Spec **1.4.8** makes the error code a MUST
  and **1.4.9** makes the reason a SHOULD. `Client.BooleanValueDetails` returns `Value: defaultValue`
  on every error path and a non-nil error alongside it (`client.go:380`), so reading it costs nothing
  in correctness.
- **Nothing retries a failed provider initialisation.** The SDK discards the init result on the
  asynchronous path (`if async { return nil }`), and the GO Feature Flag in-process evaluator returns
  from `Init` before creating its ticker when the first fetch fails.
- **A provider instance cannot be identified.** The SDK exports `NamedProviderMetadata(name)
  Metadata` and no way to obtain the provider itself. `GetNamedProviders()` returns the live map
  without copying and races `SetNamedProvider`, so it is unusable. Every "is this provider ours"
  question can only be answered by comparing names.
- **`otel-flags/v0.1.0` is tagged.** The API changes below are breaking against a published module,
  so `instrumentationVersion` becomes `0.2.0` under the pre-1.0 breaking-goes-to-minor rule in
  `VERSIONING.md`.

## The decisions

### 1. An environment value whose intent cannot be read fails construction

No exceptions carved out by consequence severity. This reverses the treatment of
`OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL`, which warned and fell back to `60s`.

*Rejected:* keeping the status quo and documenting it as a severity rule, and splitting variables
into "switch" and "tuning" classes with different policies. Both were rejected for the same reason: a
rule keyed to how bad the consequence is has to be re-argued for every new variable, while "can the
operator's intent be read" is decidable by looking at the value.

`_POLL_INTERVAL` must parse as a positive Go duration and `_ENDPOINT` must parse with a scheme, both
validated whether or not a relay is configured — making the failure conditional on another variable
reintroduces exactly the unpredictability this removes. `_API_KEY` is not validated: any string can
be a legitimate key.

*As implemented*, two details the decision did not spell out. The endpoint must have a **host** as
well as a scheme, because `relay:1031` parses cleanly — scheme `relay`, opaque `1031` — and would
otherwise pass while producing a provider that reaches nothing. And a **blank** value for either
variable means "not configured" rather than being an error: unlike a boolean, a duration and a URL
have no second reading for `export VAR=` to be mistaken for, which is the entire reason `Lookup`
rejects it. Which schemes the relay actually speaks is left to the provider; a well-formed but
exotic one fails at the transport, where the retry loop logs it.

### 2. The error surfaces through a process-level validation function

Joined into each consumer's `resolveGates` alongside the existing `errors.Join(masterErr,
tracingErr)`, so all configuration errors are reported together and all of them are reported at
construction.

*Rejected:* `func init()`, which has no way to return an error and cannot panic in a library.

### 3. Evaluation reads the details variant and logs error-code transitions

`Value` switches to `BooleanValueDetails`, returns `details.Value` — which the SDK guarantees is the
local value on every failure — and uses `details.ErrorCode` for logging. The returned error is used
for logging only and never propagates, keeping 1.4.10 intact.

One line per flag key when its error code *changes*, not per evaluation. Steady state is silent.

*Why:* the two highest-severity findings in the review were both invisible for the same reason. A
provider that failed to initialise reported `PROVIDER_NOT_READY`/`PROVIDER_FATAL` on every
evaluation, and a missing targeting key reported `TARGETING_KEY_MISSING` on every evaluation. The
error code was populated the whole time and nothing read it. The previous doctrine — that relay
silence and relay failure are deliberately indistinguishable — is amended: **the return value stays
indistinguishable, the log does not.**

### 4. The startup window still resolves to the local value

`PROVIDER_NOT_READY` keeps passing `local` as the evaluation default, which preserves the documented
asymmetry: a relay *disable* does not survive a restart, while a relay *enable* can never appear
during the window.

*Rejected:* an opt-in "relay authoritative" mode that passes `false` for module keys during the
window, and persisting the last-known-good relay verdict to disk the way the relay proxy's
`persistentFlagConfigurationFile` does. The second is the only design that makes a relay disable
durable without making the relay an availability dependency, and it remains the answer if that
requirement ever becomes real. Today the answer is that durable state belongs in the environment
variable, and the relay's job is to stop the bleeding without a redeploy.

### 5. The provider install moves to construction

`ValidateAndInstall` triggers the one-time install, so `Value` only evaluates.

*Why:* the install ran lazily inside the first evaluation, under `installMu`. Since `SetNamedProvider`
(decision 9) holds that mutex across a blocking provider initialisation, a first evaluation
concurrent with an application's own install could block for as long as that provider's HTTP timeout
— on the hot path of an instrumented operation.

*Rejected:* `TryLock` on the evaluation path, which makes install timing non-deterministic and
requires rewriting the `sync.Once` into something retryable.

### 6. No health API is added

An application that wants to alarm on a dead relay reads the state the SDK already maintains:

```go
state := openfeature.NewClient(otelflags.FlagDomain).State()
```

*Rejected:* exporting a `RelayState()`/`RelayHealthy()` wrapper. It adds surface for something that
already exists, and invites the misuse of gating startup on relay readiness — which is precisely the
availability dependency decision 4 refuses.

The documentation must show the read **and** say not to gate startup on it.

### 7. Flag keys are passed by name, not by index

`Value(key string, local bool)`. `WithFlagKeys`, the `keys` field, and every module's `idx*` constant
block are deleted.

*Why:* the index bought nothing. `client.Boolean` takes a key string, so `Value` converted the index
straight back into one. What it cost was a positional coupling with no compile-time check: swapping
the two lines in `otel-mongo`'s `WithFlagKeys` call compiles, passes tests, and silently makes the
propagation flag control tracing. Out-of-range was the least dangerous member of that family, and
this deletes the family rather than deciding what out-of-range should return.

*Rejected:* a typed `FlagKey` handle, which buys type safety that the module's existing key constants
already provide, and a typed index, which fixes nothing.

### 8. `relayPossible` stays a construction-time snapshot

A provider bound to `FlagDomain` after the first wrapper is constructed leaves earlier wrappers deaf
for their whole lives. `otelflags.SetNamedProvider` warns when it detects that wrappers already
exist; a raw `openfeature.SetNamedProviderAndWait` is an accepted blind spot, because nothing tells
us it happened. The failure mode is documented as a failure mode, not as a recommendation.

This only bites when `_ENDPOINT` is unset — with it set, `RelayPossible()` is true from the start and
ordering is irrelevant.

*Rejected:* an atomic latch on the hot path, and a per-operation `providerBound()` check. Both share a
defect that makes them ineffective as written: a wrapper whose `tracedPossible()` was false has no
instrumented implementation allocated at all, so a gate that later says "on" has nothing to switch
on. Making them work means deferring that allocation across four modules, and `otel-mongo`'s
`shared.NewCommandMonitor` is registered on the client at `Connect` and cannot be added afterwards,
so late-enabled tracing would permanently lack real server-address capture.

*Also rejected:* removing the static gate entirely. It would end the ordering problem for three of
the four modules — `otel-gorilla-ws` negotiates otel-ws at handshake time, so its existing
connections stay deaf regardless — at the price of every Mongo client running the command monitor on
every command forever, in deployments that have no relay and tracing switched off. That cost has no
escape hatch; the ordering problem has two (set `_ENDPOINT`, or bind before constructing).

### 9. `InstallProvider` becomes `SetNamedProvider`

*Why:* "install" implies a one-time, idempotent operation, and the real semantics are set-or-replace —
which matters more now that sharing one provider across the application and this library is an
explicitly supported story.

*Rejected:* `SetProvider`, which collides with `openfeature.SetProvider` while meaning the opposite
slot; the review already found one comment that had confused the two. `SetNamedProvider` mirrors the
SDK's verb for the operation actually performed, with no domain parameter because ours is fixed.

**Sharing one provider instance requires this function.** An application that binds the same instance
as both its default provider and this library's domain reads back with identical metadata names, so
`boundToDomain` treats the domain as unbound: with an endpoint set the auto-install replaces the
application's provider, and without one the wrappers never consult it. Since a provider instance
cannot be identified through the SDK, this false negative cannot be detected away — going through
`SetNamedProvider`, which records the binding exactly, is the only reliable path.

### 10. Error codes are logged in two tiers

| Tier | Codes | Level |
|---|---|---|
| The relay has no opinion | `FLAG_NOT_FOUND`, `PROVIDER_NOT_READY` | debug |
| Something is broken | `TARGETING_KEY_MISSING`, `TYPE_MISMATCH`, `PARSE_ERROR`, `INVALID_CONTEXT`, `PROVIDER_FATAL`, `GENERAL` | warn |
| Recovery — the code cleared | — | info |

*Why:* a deployment that creates only the master kill switch and none of the module keys is entirely
reasonable, and would emit four warnings per process under a uniform rule. Logs that are noisy in the
normal case train people to ignore them, which is the failure decision 3 exists to prevent. The
`FLAG_NOT_FOUND` line survives at debug because it is the only signal available to someone who
mistyped a key name on the relay.

The rule belongs in a comment at the classification, not spread across call sites.

### 11. The 250 ms evaluation timeout stays hardcoded

It applies only to a provider the application installed — the auto-installed one evaluates in process
and skips building a context at all.

*Rejected:* a new environment variable. Under decision 1 it would carry validation and fail-fast
weight, in exchange for a knob that belongs on the application's own provider, where an HTTP client
can distinguish connect, TLS and read timeouts far more precisely than one number here can.

### 12. The entry point is named `ValidateAndInstall() error`

*Rejected:* `Prepare`, which says nothing about what it guarantees, and `InitProcess`, whose "Init"
collides with the provider lifecycle verb and suggests it waits for initialisation — which it does
not. The install stays non-blocking, and the name plus one line of documentation has to make that
unmistakable.

### 13. Three commits on one branch

1. **API and behaviour** — the rename, `Value(key string, local bool)`, removal of `WithFlagKeys` and
   the `idx*` constants, `ValidateAndInstall`, the install moving to construction, environment
   fail-fast; all four consumers updated in lockstep.
2. **Observability** — `BooleanValueDetails`, the two-tier classification, per-key transition
   tracking.
3. **Documentation** — `CLAUDE.md`, both `feature-flags` documents, both READMEs, the CHANGELOGs, and
   the status table in `docs/otel-flags-review-2026-08.md`.

*Rejected:* one commit, which mixes a five-module mechanical rename with two behavioural changes and
cannot be reverted piecewise; and splitting across two releases, which would publish a version where
construction fails on a malformed poll interval before the diagnostics that make such failures
readable exist.

## Files this touches

- `otel-flags/`: `flags.go`, `flags_test.go`, `version.go` (`0.2.0`), `CHANGELOG.md` (a new `0.2.0`
  section; the `0.1.0` section stays as released history), `README.md`
- Each consumer: `env_flags.go` (drop the index constants, change the `Value` call sites, join
  `ValidateAndInstall` into `resolveGates`), `go.mod` (`require` moves to `v0.2.0`), `CHANGELOG.md`
- `CLAUDE.md`, `docs/feature-flags.md`, `docs/feature-flags.zh-TW.md`,
  `docs/otel-flags-review-2026-08.md`

## The `require` bump waits for the tag

**Corrected during implementation.** The plan was to move the consumers' `require` lines to
`otel-flags v0.2.0` in the same change set and accept red CI until `git tag otel-flags/v0.2.0`, on
the assumption that the repo-root `go.work` covers the window locally. It does not: a `require` on a
version that does not exist fails module-graph loading for the whole workspace, so **every module
stops building locally too**, including `otel-flags` itself.

```
flags.go:81:2: github.com/akira-core/instrumentation-go/otel-flags@v0.2.0:
  reading .../otel-flags/go.mod at revision otel-flags/v0.2.0: unknown revision
```

So the `require` lines stay at `v0.1.0` in this change set, and the bump happens at release, which is
the two-stage procedure `VERSIONING.md` and `CLAUDE.md` already describe: tag `otel-flags/v0.2.0`,
then bump the four `require` lines, `GOWORK=off go mod tidy`, then tag the wrappers. Local
development keeps working against the working tree the whole time; the API mismatch the original
reasoning worried about does not arise, because the workspace resolves `otel-flags` from source
rather than from the version named in `require`.

**Done.** `otel-flags/v0.2.0` is tagged, and **thirteen** `require` lines moved with it — not four.
The count is the part worth carrying forward: each wrapper's `examples/` and `tests/integration/`
sub-module has its own `go.mod` naming `otel-flags` as an indirect requirement, and so does
`otel-testkit` and both of its `examples/httpdirect*` templates.

What the window cost while it was open is the reason this is not a footnote. Four modules —
`otel-mongo`, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws` — carried code written against the
0.2.0 API against a `require` on `0.1.0`, so **none of them built the way a consumer resolves them**,
and CI's `test-and-lint` matrix, which sets `GOWORK=off` for exactly this reason, was red for each.
The workspace hid it locally: `go build ./...` inside a module directory resolves through `go.work`
and passes.

## After the release: a second review pass

`otel-flags` 0.2.0 shipped with seven fixes that a review of the code implementing these decisions
found — including one that inverted decision 10's whole purpose, the SDK's codeless `NOT_READY`
short-circuit being reported at **warn** as a fault on every flag key of every relay-configured
process. Both passes are recorded in
[`otel-flags-review-2026-08.md`](otel-flags-review-2026-08.md); the findings still open are
summarised for operators under **Known limitations** in [`feature-flags.md`](feature-flags.md).

The lesson for decision 10 specifically: the tier classification is only as good as the code the SDK
hands you, and the SDK does not populate one on the paths where it answers without consulting a
provider. A rule that maps "no code" onto the most severe tier will therefore fire hardest in the
most ordinary state.
