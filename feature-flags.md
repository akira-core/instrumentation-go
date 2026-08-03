# Tracing feature flags

> **Status: target behaviour for `otel-mongo` 0.9.0 / `otel-mongo/v2` 2.9.0 / `otel-nats` 0.8.0 /
> `otel-gorilla-ws` 0.8.0. Implementation is in progress.** The code currently in this repository
> still follows the previous model, in which the relay decided flags in both directions and
> `WithTracingEnabled` pinned a connection static. Design record:
> `openspec/changes/openfeature-dynamic-flags/design.md`; remaining work: that change's
> `tasks.md` § 9. Until it lands, treat the shipped release notes as authoritative for behaviour
> and this document as authoritative for intent.

Every instrumentation module can be switched off. This document is the single reference for how
that decision is made — what each switch does, who owns it, and what can and cannot change while
a process is running.

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
| **negotiation outcome** | the handshake result, or `otel-ws` proven on the raw connection's subprotocol for `NewConn`, clamped by capability | whether the JSON envelope is written on the wire | Yes — a handshake cannot be revisited |
| **span gate** | capability `&&` relay verdict | whether spans are created and trace context injected or extracted | **No — re-read on every read and write** |

| capability | negotiated | span gate | Wire | Spans | Trace propagation |
|---|---|---|---|---|---|
| false | (clamped false) | not evaluated | raw payload | none | none |
| true | false | false | raw payload | none | none |
| true | false | true | raw payload | local only | none — no carrier |
| true | true | false | envelope, empty header | none | none |
| true | true | true | envelope with `traceparent` | yes | yes |

Row 4 is the cost of keeping propagation possible: a connection that negotiated `otel-ws` keeps
writing the envelope after a revocation, because its peer parses every frame as one. It carries
no trace context and creates no span.

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

## Wiring a provider

The GO Feature Flag provider is an **application** dependency. The instrumentation modules depend
only on `github.com/open-feature/go-sdk` and never install a provider, set an evaluation context,
or shut the SDK down — the same rule they follow for `TracerProvider`.

```go
import (
    gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
    "github.com/open-feature/go-sdk/openfeature"
)

provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint: "http://relay:1031",
})
if err != nil {
    return err
}
// SetProviderAndWait, not SetProvider — see below.
if err := openfeature.SetProviderAndWait(provider); err != nil {
    return err
}
```

Install it at startup, next to your `otelsetup.Init()` call.

### `SetProviderAndWait` is required, not preferred

An unresolvable flag means "allow", so a provider that has not yet fetched its configuration
**cannot revoke anything**. With the non-blocking `SetProvider`, every flag reads as enabled
between that call and the provider's first successful fetch.

The case that makes this matter is ordinary: an operator revokes a module to stop an incident,
and the process restarts for an unrelated reason. Block on readiness before serving traffic and
the restart comes back revoked; do not, and it comes back instrumented until the provider
catches up.

### In-process evaluation only

Use the provider's default `EvaluationType` (`INPROCESS`), in which it polls the relay in the
background and every flag lookup is local. **Remote evaluation is not supported**: it turns each
lookup into an HTTP request, which would put network I/O on the path of a Mongo query or a NATS
publish.

### Targeting

The modules pass an empty evaluation context. Applications that want targeting install a global
one:

```go
openfeature.SetEvaluationContext(openfeature.NewTargetlessEvaluationContext(map[string]any{
    "service.name": "checkout-api",
}))
```

The modules resolve a verdict on every instrumented operation and cache nothing, so a revocation
takes effect on the next operation with no additional delay. The evaluation context is
process-wide, so targeting can key on process-level attributes — service, environment, host — but
**not** on per-request attributes.

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
environment variable and the relay verdict on every operation, and still stops when the relay
revokes. There is no way to opt a connection out of a revocation.

## Operational summary

- To make a module revocable: deploy with `gate1` and the module switch on, and create its flag
  on the relay.
- To stop a module now: set its relay flag to `false`. It takes effect as soon as your provider
  picks the change up — the library adds no delay of its own.
- To stop everything now: revoke all four relay flags. There is no single key.
- To stop a module permanently: change its environment variable and redeploy.
- To investigate an incident by turning tracing **on**: not possible from the relay. Change the
  environment and redeploy.
