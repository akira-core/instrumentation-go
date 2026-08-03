# Tracing feature flags

> 繁體中文版本:[feature-flags.zh-TW.md](feature-flags.zh-TW.md)

Every instrumentation module can be switched off. This document is the single reference for how
that decision is made — what each switch does, who owns it, and what can and cannot change while
a process is running.

Applies to `otel-mongo` 0.9.0, `otel-mongo/v2` 2.9.0, `otel-nats` 0.8.0 and `otel-gorilla-ws`
0.8.0 and later. Design record: `openspec/changes/openfeature-dynamic-flags/design.md`.

## The model in one paragraph

Instrumentation is enabled by a deployment and can be revoked by an operator. A [GO Feature Flag]
relay proxy, reached through [OpenFeature], acts as a **kill switch only**: a flag set to `false`
turns a running module off as soon as the provider observes it, and nothing on the relay can turn
anything on.
Everything that is on was turned on by a deployment that someone reviewed. When the relay is
unreachable, misconfigured, or absent, every flag reads as "do not interfere" and the
environment alone decides — so an application that never installs a provider behaves exactly as
its environment says.

[GO Feature Flag]: https://gofeatureflag.org
[OpenFeature]: https://openfeature.dev

## Three tiers

```
tracing = gate1 && OTEL_<MODULE>_TRACING_ENABLED && relay verdict
```

| Tier | Owner | When it is off | Changeable without a redeploy? |
|---|---|---|---|
| **`gate1`** — `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **or** `WithTracingEnabled`, never both | whoever deploys, or whoever constructs the wrapper | every module in the process is off, only passthrough implementations are allocated, and no OpenFeature code path is reachable | No |
| **`OTEL_<MODULE>_TRACING_ENABLED`** | whoever deploys | that module is off, only its passthrough implementation is allocated, and its relay flag is never evaluated | No |
| **relay flag `otel-<module>-tracing`** | whoever operates | that module stops emitting on a running process, from its next operation | **Yes — this is the only tier that can** |

The first two tiers are both environment-derived and both fixed at construction. They differ only
in scope — whole process versus one module — and in who owns them. The third is the only dynamic
one, and it can only subtract from what the first two allow.

## Resolving `gate1`

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and the `WithTracingEnabled(v bool)` constructor option
are two spellings of the same switch. Set **exactly one**. Supplying both is a configuration
error, reported by the constructor, even when the two agree — the rule is "set one", not "make
them match".

| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `WithTracingEnabled` | `gate1` |
|---|---|---|
| unset | not passed | disabled |
| unset | `true` | enabled |
| unset | `false` | disabled |
| set, truthy | not passed | enabled |
| set, falsy | not passed | disabled |
| **set, any value** | **passed, any value** | **construction error** |

The error wraps a per-module sentinel, so it can be matched with `errors.Is`, and names both
observed values:

```go
conn, err := otelnats.ConnectWithOptions(url, nil, otelnats.WithTracingEnabled(true))
if errors.Is(err, otelnats.ErrTracingConfigConflict) {
    // OTEL_INSTRUMENTATION_GO_TRACING_ENABLED is also set — remove one of them
}
```

`otel-mongo` applies the same rule to `OTEL_MONGO_PROPAGATION_ENABLED` and
`WithTracePropagationEnabled`, with its own sentinel. A call that violates both rules gets a
single `errors.Join`ed error matching both sentinels, so you do not fix one conflict only to
discover the other on the next run.

## Effective tracing

| `gate1` | `OTEL_<MODULE>_TRACING_ENABLED` | relay `otel-<module>-tracing` | Tracing | Relay consulted? |
|---|---|---|---|---|
| disabled | anything | anything | **off** | No |
| enabled | unset or falsy | anything | **off** | No |
| enabled | truthy | `false` | **off** | Yes |
| enabled | truthy | `true` | **on** | Yes |
| enabled | truthy | no opinion | **on** | Yes |

Two properties fall out of this table and are worth stating on their own:

- **The relay cannot enable anything.** Row 2 holds no matter what the relay serves. To put a
  module under relay control you must deploy it with its environment switch on and use the relay
  to hold it off.
- **A module switched off in the environment costs nothing.** Rows 1 and 2 allocate only the
  passthrough implementation and never evaluate a flag, so the zero-cost path is preserved.

## What "no opinion" covers

The relay verdict is resolved with an evaluation default of `true`, and OpenFeature returns that
default on every failure path. All of the following therefore mean *do not interfere*:

- no OpenFeature provider was installed
- a provider is installed but not yet ready
- the relay configuration contains no flag with that key
- evaluation returned an error
- the flag exists but is not a boolean
- the relay is unreachable and the provider has no cached configuration

There is no way to distinguish these from a relay that explicitly serves `true`, and no reason
to: both mean the environment decides.

## Truthiness

A switch is enabled only when it is set to one of `1`, `true`, `yes`, `on` — lowercased and
whitespace-trimmed before comparison. **Everything else is disabled**, including an unset
variable and the empty string.

| Value | Result |
|---|---|
| unset | disabled |
| `1` / `true` / `yes` / `on` | enabled |
| `TRUE` / `On` / `  yes  ` | enabled |
| `0` / `false` / `no` / `off` | disabled |
| `` (set but empty, `export VAR=`) | **disabled** |
| `enabled` / `2` / `y` / `t` | **disabled** |

The last two rows are the ones that catch people. `export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=`
does not open the gate, and neither does `=enabled`. If a switch is not doing what you expect,
check its value against the four accepted words before looking anywhere else.

## Flag keys

Each relay flag is paired with one environment variable. The pairing is a convention for
operators — the environment variable is a separate conjunctive tier, **not** the flag's
evaluation default.

| Relay flag key | Paired environment variable | Modules |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

There is **no relay key for `gate1`**, and therefore no single switch that silences a whole
process from the relay. Stopping every module means revoking all four flags.

`otel-mongo` v1 and v2 share both keys: revoking `otel-mongo-tracing` stops both.

Three further variables configure the connection to the relay rather than any module's behaviour,
and have no relay counterpart:

| Variable | Purpose |
|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | relay proxy URL; unset ⇒ no provider is installed |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | relay API key |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | poll interval, Go duration string, default `60s` |

All four modules resolve through a single OpenFeature domain, `otel-instrumentation-go`. One
provider serves all of them; there is no per-module provider.

## otel-mongo: `_oteltrace` document propagation

Mongo has a second switch, one level below tracing, controlling whether an `_oteltrace`
subdocument is written into your documents.

| Effective tracing | `gateProp` (`OTEL_MONGO_PROPAGATION_ENABLED` or `WithTracePropagationEnabled`) | relay `otel-mongo-propagation` | `_oteltrace` written and read |
|---|---|---|---|
| **off** | anything | anything | **no** |
| on | disabled | anything | **no** |
| on | enabled | `false` | **no** |
| on | enabled | `true` or no opinion | **yes** |

Read this switch more carefully than the others, because it is the only one that changes what is
**persisted**:

- The field is roughly 90 bytes of BSON per document, more when a `tracestate` is present. It is
  written by `InsertOne`, `InsertMany`, `UpdateOne`, `UpdateMany`, `UpdateByID`, `ReplaceOne` and
  `BulkWrite`.
- **Nothing removes it.** The module reads `_oteltrace` to restore trace context but never strips
  it from a decoded document, so once written it is visible to your application on every
  subsequent read.
- **Turning it off does not undo anything.** New writes stop carrying the field; documents that
  already have it keep it. Cleaning up is an application-side `$unset` migration.
- A collection with `$jsonSchema` validation and `additionalProperties: false` will **reject
  every write** while this is on.

Because of that, only a deployment can turn it on. As with tracing, the relay can only revoke.

## otel-gorilla-ws: three distinct booleans

The WebSocket module has three values with similar names and different lifetimes. Only the last
one is dynamic.

| Name | Resolved as | Decides | Fixed for the connection? |
|---|---|---|---|
| **capability** | `gate1 && OTEL_GORILLA_WS_TRACING_ENABLED` | whether to offer (`Dial`) or confirm (`Upgrade`) the `otel-ws` subprotocol, and whether to build a real tracer | Yes — resolved before the handshake |
| **negotiation outcome** | the handshake result, or `otel-ws` proven on the raw connection's subprotocol for `NewConn` | whether the **peer** envelopes every frame | Yes — a handshake cannot be revisited |
| **span gate** | capability `&&` relay verdict | whether spans are created and trace context injected or extracted | **No — re-read on every read and write** |

Capability clamps the **write** decision only. Whether the peer envelopes is a fact of the
handshake, not something this side's gate has authority over, so a capability-off wrapper of a
negotiated connection writes raw frames — safe, because the peer's probe falls back to the payload
— while still **unwrapping on read**. Keying the read path on capability would hand raw
`{"header":…,"data":…}` bytes to your application.

| capability | negotiated | span gate | Wire out | Wire in | Spans | Trace propagation |
|---|---|---|---|---|---|---|
| false | false | not evaluated | raw | raw | none | none |
| false | true | not evaluated | raw | **unwrapped** | none | none |
| true | false | false | raw | raw | none | none |
| true | false | true | raw | raw | local only | none — no carrier |
| true | true | false | envelope, empty header | unwrapped | none | none |
| true | true | true | envelope with `traceparent` | unwrapped | yes | yes |

Row 5 is the cost of keeping propagation possible: a connection that negotiated `otel-ws` keeps
writing the envelope after a revocation, because its peer parses every frame as one. It carries
no trace context and creates no span.

**Revoking `otel-gorilla-ws` stops telemetry, not overhead.** It is the one module of the four
that does not return to the zero-cost path: the envelope is still marshalled on every write and
probed on every read. Removing that wire cost requires a redeploy with
`OTEL_GORILLA_WS_TRACING_ENABLED` off. Dropping the envelope on revocation instead would
desynchronise the wire from a peer that is still enveloping, and silently dismember any
application payload shaped like one.

The envelope structure `{"header":…,"data":…}` is **reserved** on an `otel-ws` connection — see
[otel-ws.md](otel-ws.md).

Negotiation deliberately ignores the relay verdict. That costs nothing under a revoke-only relay:
a connection whose environment switch was off at handshake time could never be switched on
later, so there is no future state in which it would need the envelope. Upgrading without a
provider therefore changes nothing on the wire.

## What is not gated

`otelmongo.ContextFromDocument` and `otelmongo.ContextFromRawDocument` carry **no flag gate at
all**. They read an `_oteltrace` field out of a document you already hold and return the span
context it encodes. They start no span, allocate no attributes, initialise nothing in the OTel
SDK and write nothing anywhere — and you only call them when you want trace extraction, so there
is nothing for a kill switch to protect you from.

`Cursor.DecodeAndTrace` and `ChangeStream.DecodeAndTrace` look similar but **are** gated: each
starts and ends a `mongo.cursor.decode` span on every call, so they emit telemetry and belong
under the switch.

**A revocation therefore does not stop trace-context extraction.** That is worth stating plainly,
because "to stop a module now, set its relay flag to `false`" otherwise reads as though everything
stops:

| | after a revocation |
|---|---|
| `Collection.InsertOne` and siblings | no span, no `_oteltrace` written |
| `Cursor.DecodeAndTrace` | no span, and **no extraction** — returns `ctx` unchanged |
| `ContextFromDocument` / `ContextFromRawDocument` | **still extract**, exactly as before |

The gate on `DecodeAndTrace` governs the span it emits, not the linking. If you want linking to
survive the library being silenced, call `Decode` followed by `ContextFromDocument` — that is the
supported way to do it, not a loophole.

## Connecting to a relay

**Set an environment variable. There is no code to write.**

```sh
OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT=http://relay:1031
```

With that set and no OpenFeature provider of your own installed, the first instrumented operation
builds a GO Feature Flag provider, binds it to the OpenFeature domain `otel-instrumentation-go`,
and every module resolves through it from then on. Two further variables tune it:

| Variable | Meaning | Default |
|---|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | Relay proxy URL. **Unset ⇒ nothing is installed** and no OpenFeature state is written | unset |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | API key, if your relay authenticates. Never logged | empty |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | How often the provider polls the relay. **Go duration strings only** — `60` is rejected, `60s` is not | `60s` |

A malformed poll interval warns and falls back to `60s`; it does **not** abort the install,
because a typo in an optional tuning value must not silently delete your kill switch.

Two settings are hardcoded and deliberately not exposed, because getting either wrong turns a
relay outage into an application stall: `DataCollectorDisabled: true` and in-process evaluation.
Both are explained below.

### Installing your own provider instead

The library stands down entirely when you have already installed a provider — the trigger requires
that no provider is bound to `otel-instrumentation-go` and none is installed as the default. Do
this when you need lifecycle control, a shared provider with your own business flags, or a blocking
install:

```go
import (
    gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
    "github.com/open-feature/go-sdk/openfeature"
)

provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint:              "http://relay:1031",
    DataCollectorDisabled: true,   // required — see below
})
if err != nil {
    return err
}
if err := openfeature.SetProviderAndWait(provider); err != nil {
    // Log and continue. Do not fail startup — see below.
    logger.Error("feature flag provider unavailable; continuing without relay control", "error", err)
}
```

Install it at startup, next to your `otelsetup.Init()` call, and **before constructing any
wrapper**. Do not also set `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`: it would be ignored, which is
harmless but misleading.

Whichever path you take, the library never touches the **default** provider, the global evaluation
context, hooks, or `Shutdown`. Nothing it does can change how your own feature flags resolve.

### Disable the data collector

On the zero-code path this is hardcoded. It matters if you install your own provider, where
`DataCollectorDisabled: true` is **required**, not tuning.

The provider's data collector is on by default. It appends one event per evaluation to an
in-memory buffer and flushes it to the relay on a two-minute ticker. Two details make that
dangerous for this library's usage pattern, in which one evaluation happens per instrumented
operation:

- A **failed** flush does not clear the buffer.
- Once the buffer reaches its cap (100,000 events by default), **every subsequent `AddEvent`
  flushes synchronously, on the evaluating goroutine, while holding the buffer's mutex.**

With the relay down, that synchronous flush fails after the HTTP client's timeout — 10 seconds by
default — and the buffer is never drained, so it happens again on the next evaluation, with every
other evaluating goroutine queued behind the same mutex. A relay outage would then stall the
application's own Mongo queries and NATS publishes, which is exactly the thing the rest of this
design is built to prevent.

Nothing is lost by disabling it. The collector reports flag-evaluation analytics to the relay's
dashboards; with process-wide flags evaluated once per operation, those analytics are a copy of
your traffic volume.

### The relay is not a startup dependency

If the relay is unreachable when the process starts, the provider's first fetch fails. The
zero-code path logs and carries on; if you install your own provider, **log and continue** rather
than returning the error. Aborting startup makes the relay a hard dependency of your service,
which inverts the point of a brake.

Continuing costs exactly one thing, and it is unavoidable: a process that starts while the relay
is down cannot know about an active revocation, so it comes up at the state its environment
declares. There is no way to read a revocation you cannot reach.

Once the provider is installed and has fetched successfully, a later relay outage changes nothing:
the in-process evaluator keeps serving its last successfully fetched configuration, so an active
revocation survives the outage. Only evaluation errors — which cannot happen for an in-process
provider holding a configuration — fall back to "allow".

### The startup window, and how to close it

An unresolvable flag means "allow", so a provider that has not yet fetched its configuration
**cannot revoke anything**. The zero-code install is non-blocking on purpose — a brake must not
become a latency source, and blocking would put a relay round trip in front of your first Mongo
query — so between the install and the provider's first fetch every flag reads as enabled.

The case that makes this matter is ordinary: an operator revokes a module to stop an incident, and
the process restarts for an unrelated reason. It comes back instrumented until the provider
catches up. For `otel-mongo` that window is not only spans: any `_oteltrace` written during it is
permanent, and cleaning up is a `$unset` migration.

**To close it, install your own provider with `openfeature.SetProviderAndWait` before constructing
any wrapper.** That both blocks on readiness and makes the auto-install stand down. Whether the
window is acceptable is your decision, not the library's — which is why it is documented here
rather than enforced.

### In-process evaluation only

The provider polls the relay in the background and every flag lookup is local. On the zero-code
path `EvaluationType` is hardcoded to `INPROCESS`; if you install your own provider, use that
default. **Remote evaluation is not supported**: it turns each lookup into an HTTP request, which
would put network I/O on the path of a Mongo query or a NATS publish.

### Targeting one service instead of the fleet

A relay flag applies to every process that resolves it, so `otel-mongo-tracing: false` stops
tracing in your whole fleet unless the rule can tell services apart.

Set **`OTEL_SERVICE_NAME`** — the OpenTelemetry-specified variable your exporter already reads —
and the zero-code path supplies it as a `service.name` attribute on every evaluation:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  targeting:
    - query: service.name eq "checkout-api"
      variation: disabled     # only this service stops
  defaultRule:
    variation: enabled
```

Two limits. The attribute is supplied **only** on the zero-code path: if you install your own
provider you own your evaluation context and set it yourself, with
`openfeature.SetEvaluationContext`. And it is process-scoped, so targeting can key on
process-level attributes — service, environment, host — but **not** on per-request attributes.

## Revocation latency

A revocation is **not instantaneous**, and the delay is not in this library.

The modules resolve a verdict on every instrumented operation and cache nothing, so they add no
delay of their own. What you wait for is the **provider's poll interval**: the relay is polled in
the background, and a flag change is invisible until the next poll lands.

| | Delay |
|---|---|
| The modules' own resolution | none — every operation reads the current value |
| Provider poll interval (zero-code path) | **up to 60 s**, tunable with `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` |
| Provider poll interval (your own provider) | whatever you set; the GO Feature Flag default is **120 s** |

Plan an incident response around the poll interval, not around "immediately". If 60 s is too slow
for your risk profile, lower it — the poll is a conditional `GET` that returns 304 when nothing
changed, so tightening it is cheap.

## What resolution costs

The relay verdict is resolved on **every instrumented operation**; nothing is cached. Measured
against an in-memory provider, one evaluation is roughly **2 µs and 7 allocations**. That is not
the flag lookup — it is the OpenFeature SDK's evaluation pipeline around it (hook chains,
evaluation-context merging, the provider registry lock), and it does not get cheaper because the
provider keeps its configuration in memory.

Two things bound where that cost lands:

- It is paid only by wrappers that are **actively instrumenting**. A module switched off in the
  environment allocates only its passthrough implementation and never evaluates anything.
- Against a Mongo round trip it is noise. Against a NATS publish, which already pays 1–3 µs to
  create a span, it roughly doubles the instrumentation overhead.

Caching sits behind an unchanged internal signature, so it can be added without affecting any
API if a benchmark on a real workload shows it matters. It is deliberately deferred rather than
ruled out; the reasoning is recorded in the design document.

## Per-connection options

`WithTracingEnabled(v bool)` supplies `gate1` for one connection or client, for callers that
cannot set a process environment variable — tests, or several independently configured clients in
one binary. It is accepted by `otelnats.ConnectWithOptions` and its TLS and credentials variants,
`otelmongo.ConnectWithOptions` and `NewClient` (v1 and v2), and
`otelgorillaws.NewConn` / `Dial` / `Upgrader.Upgrade`.

It supplies one tier and nothing more. A connection carrying it still reads its module
environment variable at construction and the relay verdict on every operation, and still stops when
the relay revokes. There is no way to opt a connection out of a revocation.

It is an alternative **spelling** of `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, not an override of
it: supplying both is a configuration error, reported by the constructor. `otel-mongo` applies the
same rule to `WithTracePropagationEnabled` and `OTEL_MONGO_PROPAGATION_ENABLED`. See
*Resolving `gate1`*.

## Operational summary

- **To make a module revocable:** deploy with `gate1` and the module switch on, set
  `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`, and create its flag on the relay. No application code.
- **To stop a module now:** set its relay flag to `false`. It takes effect on the next poll —
  **up to 60 s** by default, not instantly. See *Revocation latency*.
- **To stop one service rather than the fleet:** set `OTEL_SERVICE_NAME` and target
  `service.name` in the relay rule.
- **To stop everything now:** revoke all four relay flags. There is no single key.
- **To stop a module permanently:** change its environment variable and redeploy.
- **To investigate an incident by turning tracing on:** not possible from the relay. Change the
  environment and redeploy.

Two things a revocation does **not** do, both easy to assume it does:

- It does not stop `otelmongo.ContextFromDocument` / `ContextFromRawDocument`. Those are ungated —
  see *What is not gated*.
- It does not remove `otel-gorilla-ws`'s per-message envelope cost on an already-negotiated
  connection. That needs a redeploy — see *otel-gorilla-ws: three distinct booleans*.
