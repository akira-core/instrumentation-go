# Tracing feature flags

> 繁體中文版本:[feature-flags.zh-TW.md](feature-flags.zh-TW.md) ·
> Hands-on tutorial:[otel-nats-kill-switch.en-US.html](otel-nats-kill-switch.en-US.html) /
> [otel-nats-kill-switch.zh-TW.html](otel-nats-kill-switch.zh-TW.html)

Every instrumentation module can be switched on or off. This document is the single reference for
how that decision is made — what each switch does, who owns it, which ones can change while a
process is running, and which cannot.

Applies to `otel-flags` 0.1.0, `otel-mongo` 0.8.0, `otel-mongo/v2` 2.8.0, `otel-nats` 0.8.0 and
`otel-gorilla-ws` 0.8.0 and later. Design record:
`openspec/changes/openfeature-dynamic-flags/design.md`.

## The model in one paragraph

Each switch is resolved down a four-step ladder, and the first source with an opinion wins. A
[GO Feature Flag] relay proxy, reached through [OpenFeature], sits at the top and is **authoritative
in both directions**: it can turn a running module off, and it can turn one on that the deployment
left off. Below it, an environment variable, then a constructor option, then a hardcoded default.
What keeps that safe is the defaults, not a restriction on the relay: every per-module switch
defaults to **off**, so a process that configures nothing traces nothing, and the process-wide
master switch defaults to **on** only because it is a veto rather than an enabler. When no relay is
configured, no OpenFeature code runs at all and the environment and options alone decide.

[GO Feature Flag]: https://gofeatureflag.org
[OpenFeature]: https://openfeature.dev

## Which setting wins

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

The order is **how late each source is decided**: the default is compiled in, the option is written
when the wrapper is constructed, the environment variable is set when the process is deployed, and
the relay changes while it runs. Each later stage overrides the earlier ones. That is the ordinary
layering, and it needs no separate rule to remember.

| Source | Owner | Scope | Changeable without a redeploy? |
|---|---|---|---|
| relay flag | operator | fleet, or one service (see [Targeting](#targeting-one-service-instead-of-the-fleet)) | **Yes — the only one that can** |
| `OTEL_*` environment variable | deployer | one process | No |
| `With*Enabled` option | the caller that constructs the wrapper | one connection or client | No |
| hardcoded default | this library | everywhere nobody spoke | No |

**The option sits below its environment variable.** This is deliberate, and it is the one place this
release breaks with `0.7.0`, where the option won. Three reasons, in order of weight:

1. It gives an operator a **per-module** setting that application code cannot override.
   `OTEL_MONGO_TRACING_ENABLED=false` disables that module for that deployment even when the Go
   code passed `WithTracingEnabled(true)` — without silencing the whole process, and without
   deploying a relay.
2. It closes the asymmetry on the one switch that writes data. `WithTracePropagationEnabled(true)`
   would otherwise override `OTEL_MONGO_PROPAGATION_ENABLED=false` and start appending permanent
   `_oteltrace` fields to the operator's own documents. Every other switch only produces or
   withholds telemetry; that one leaves state behind.
3. The ladder stays monotonic in deployment order, so it is one sentence rather than a specificity
   rule that reverses between two adjacent rungs.

**What it costs.** An option is consulted only when its environment variable is unset, so a process
that sets the variable cannot differentiate two connections through options. That is the one thing
the option uniquely expresses — trace one of two Mongo clients — and it survives on the condition
that the deployment leaves the variable unset. Under a default of `off`, that is the ordinary state,
not a sacrifice.

## The three switches

| Switch | Relay key | Option | Environment variable | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| per-module tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` |
| Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

They compose by conjunction:

```
tracing     = master && moduleTracing
propagation = tracing && mongoPropagation
```

**The master switch is a veto, not an enabler.** Its default is `true`, which means "express no
objection". Setting it to `true` — in the environment or on the relay — changes nothing at all. The
only value with an effect is `false`, which stops every module in the process, including connections
whose Go code passed an option. Do not create `otel-instrumentation-go-tracing: true` on a relay
expecting it to enable something; it will read as a broken flag.

**Nothing turns on because the master is `true`.** The per-module default of `false` is what keeps a
zero-configuration process silent.

### Worked examples

| Configuration | Result |
|---|---|
| nothing set anywhere | off |
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` only | off — the master enables nothing |
| `OTEL_NATS_TRACING_ENABLED=true` only | **NATS on** — the master defaults to `true` |
| `WithTracingEnabled(true)`, no variables | on for that connection |
| `WithTracingEnabled(true)` + `OTEL_NATS_TRACING_ENABLED=false` | **off** — the variable outranks the option |
| `WithTracingEnabled(false)` + `OTEL_NATS_TRACING_ENABLED=true` | **on** — same rule, other direction |
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` + everything else on | off — the veto beats everything |
| relay `otel-mongo-tracing: true`, `OTEL_MONGO_TRACING_ENABLED` unset | **Mongo on** — the relay can enable |
| relay `otel-mongo-tracing: false`, `OTEL_MONGO_TRACING_ENABLED=true` | off — the relay can disable |
| `OTEL_MONGO_TRACING_ENABLED=` (empty) | **construction error** — see below |

## Environment values are a strict tri-state

An environment variable has exactly three outcomes:

| Value | Outcome |
|---|---|
| unset | no opinion — resolution falls through to the option, then the default |
| `1` `true` `yes` `on` / `0` `false` `no` `off` (trimmed, case-insensitive) | this source decides |
| anything else, **including the empty string** | the constructor returns an error wrapping `otelflags.ErrInvalidFlagValue` |

**There is no guessing.** Under a ladder there is no safe direction to guess in: the master tier
defaults to `true` and every other tier to `false`, so a value silently read as `false` would stop a
whole fleet on one tier and change nothing on the others — the same input meaning two different
things, with a log line as the only evidence.

**`export VAR=` is invalid**, not falsy. Both available readings are wrong somewhere: as `false` it
lets an unexpanded `${SOMETHING}` template variable express an opinion the deployment never had; as
unset it silently reverses meaning for anyone who used it as an off switch. The rule has no
exceptions: **set it to a recognised value, or do not set it.**

The error names the variable and the observed value. A constructor that reads several switches
reports **all** of the bad ones in one joined error, so one run tells you everything to fix.

**The relay-connection variables follow the same principle with their own shapes.** They are
validated at construction whether or not a relay is configured, and an unreadable one fails the
constructor with the same sentinel:

| Variable | Accepted | Rejected |
|---|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | unset, blank, or a URL with a scheme **and** a host | `relay:1031`, `relay`, `http://` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | unset, blank, or a positive Go duration | `60`, `soon`, `0s`, `-5s` |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | anything | — (never validated, never logged) |

Blank means "not configured" for those two rather than being an error, and that is not a carve-out: a
duration and a URL have no second reading for `export VAR=` to be mistaken for, which is the whole
reason a **boolean** rejects it.

## Before you upgrade

**Grep your deployment configuration for `OTEL_*_ENABLED` and confirm every value is in one of the
two accepted lists.** This is the one change in this release that can stop a process from starting:
`=enabled`, `=2`, `=y` and `=` (empty) were previously tolerated and now fail at the first
constructor. An unexpanded `${SOMETHING}` in a Kubernetes manifest reaches exactly the empty case.

**Then grep for `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL`.** A malformed value used to warn and
fall back to `60s`; it now fails the same constructor. `=60` is the case to look for — it was never
read as seconds, and it is now rejected rather than silently ignored.

Then re-read what the defaults mean. Against `0.7.0`:

- A **module** variable set without the global one now takes effect. It used to be inert, which was
  a common "I set the flag and nothing happened" report; it is still a change.
- An option alongside its paired environment variable now loses to the variable.
- `_oteltrace` is unaffected in both cases: propagation defaults to `false` and needs its own
  explicit `true`.

## Flag keys

| Flag key | Paired environment variable | Option | Default | Modules |
|---|---|---|---|---|
| `otel-instrumentation-go-tracing` | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | — | `true` | all |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `WithTracePropagationEnabled` | `false` | `otel-mongo`, `otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-gorilla-ws` |

Keys are fixed and not overridable at runtime. `otel-mongo` and `otel-mongo/v2` share both keys, so
one relay change reaches both.

## otel-mongo: `_oteltrace` document propagation

This is the one switch whose "on" state leaves something behind, and the one to read carefully.

When propagation is on, `InsertOne`, `InsertMany`, `ReplaceOne`, `UpdateOne`, `UpdateMany`,
`UpdateByID` and `BulkWrite` append an `_oteltrace` subdocument — roughly 90 bytes of BSON, more with
a `tracestate` — to the documents they write.

- **It is never stripped on read.** Once written, the field is visible to your application on every
  subsequent read of that document, permanently.
- **Turning the switch back off does not undo anything.** New writes stop carrying it; existing
  documents keep it. Cleanup is a `$unset` migration you run yourself.
- **Against a collection with `$jsonSchema` and `additionalProperties: false`, the write fails
  outright.**

### The relay can enable this

Unlike the superseded revoke-only model, a relay flag can start these writes. Four things bound it,
and a site that cannot accept the risk uses one of them:

1. **The master veto.** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` stops everything in the
   process regardless of any relay value.
2. **The environment variable.** `OTEL_MONGO_PROPAGATION_ENABLED=false` cannot be overridden by
   application code — only by the relay. This is why the option sits below it.
3. **The default of `false`.** Absence in every source can never enable it. Something must
   deliberately say `true`: an option, a variable, or a relay flag that somebody created — and the
   relay configuration is written by your own site.
4. **No relay, no reach.** A process with no `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` and no provider
   bound to `otelflags.FlagDomain` cannot be reached by a relay at all — see
   [Install your provider first](#install-your-provider-before-constructing-wrappers). A provider you
   installed for your application's own flags does not count.

## otel-gorilla-ws: negotiation is a handshake fact

Three distinct booleans, easily confused:

| | What it means | When it is decided |
|---|---|---|
| effective tracing | should this call create spans and inject/extract? | per `WriteMessage`/`ReadMessage` |
| negotiated (`otel-ws`) | does the peer parse every frame as an envelope? | once, during the handshake |
| capability | could any OTel SDK path ever run on this connection? | once, at construction |

`Dial` offers, and `Upgrader.Upgrade` confirms, the `otel-ws` subprotocol when the connection's
effective tracing value — master, module, relay included — is on **immediately before the
handshake**. A handshake cannot be revisited, so this produces an asymmetry that must be planned
around:

- **Enabling reaches only connections opened afterwards.** A long-lived connection opened while this
  module was off never gains the envelope, and `WithTracingEnabled(true)` cannot restore it: a peer
  that did not negotiate `otel-ws` will not parse one. Such a connection can still emit **local**
  send/receive spans once the flag is on — it simply cannot inject or extract. An operator who needs
  an existing connection instrumented must cycle it.
- **Disabling reaches every connection immediately for spans and inject/extract, but not for the
  envelope.** A disabled connection that negotiated `otel-ws` keeps writing the envelope and keeps
  running the read probe, because the peer is still parsing every frame as one. This is the one
  module of the four that does not return to the zero-cost path when you turn it off; removing that
  wire overhead requires cycling the connection.

`NewConn` has no handshake of its own: it enables the envelope only when the raw connection's
negotiated subprotocol proves `otel-ws`. Callers running their own handshake use the exported
`SubprotocolOTelWS` token and can verify with `IsOTelNegotiated(conn)`. See `otel-ws.md` for the full
negotiation matrix.

## What is not gated

`ContextFromDocument` and `ContextFromRawDocument` (`otel-mongo`, v1 and v2) carry **no switch at
all** — not the master, not the module variables, not the options, not the relay.

They start no span, build no attributes, initialise nothing in the OTel SDK, write nothing anywhere,
and perform no OpenFeature evaluation. They read a field out of a value you already hold and return
what it encodes. The switches exist to stop the library doing work on your behalf as a side effect of
a business operation; these two do only the thing you invoked them for.

**Turning a module off therefore does not stop trace-context extraction.** That is deliberate, and it
is the supported way to keep trace linking while the library is silenced: use `Decode` followed by
`ContextFromDocument` rather than `DecodeAndTrace`.

`Cursor.DecodeAndTrace` and `ChangeStream.DecodeAndTrace` **are** gated, because each starts and ends
a real `mongo.cursor.decode` span.

## Connecting to a relay

### The zero-code path

Set environment variables. No import, no Go code, nothing else to remember.

| Variable | Meaning |
|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | relay proxy URL, with a scheme and a host (`http://relay:1031`). Unset ⇒ nothing is installed, no OpenFeature state is written, and no evaluation ever happens. A value that is set but is not such a URL **fails construction** |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | optional; never logged |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | optional; a positive Go duration (`30s`, `2m`), default `60s`. A value that is set and cannot be read **fails construction** (0.2.0+; it used to warn and fall back). The value sets the centre of the polling period, not an exact one: it is deviated by at most ±10%, drawn once per process |
| `OTEL_SERVICE_NAME` | optional; supplies `serviceName` and `service.name` targeting attributes, on this path only. Rules must key on the dot-free spelling |

The library installs a GO Feature Flag provider as a **named** provider on the domain
`otel-instrumentation-go`, and only when your application has installed no provider of its own. It
hardcodes `DataCollectorDisabled: true` and in-process evaluation, so this path cannot be
misconfigured into the failures described below.

Exactly one provider is installed per process, however many modules the binary links: the shared
`otel-flags` module holds the install behind a single lock.

The library never touches the **default** provider, the global evaluation context, hooks, or
shutdown. Nothing it does can change how your own feature flags resolve.

### Installing your own provider instead

```go
provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint: "http://relay:1031",

    // Required. See "Disable the data collector" below.
    DataCollectorDisabled: true,
})
if err != nil {
    slog.Warn("feature flag provider unavailable; switches are environment-only", "error", err)
} else if err := otelflags.SetNamedProvider(provider); err != nil {
    // Log and continue: the relay is a control plane, not a prerequisite.
    slog.Warn("feature flag provider registration failed", "error", err)
}
```

`otelflags.SetNamedProvider` binds the **named** domain `otel-instrumentation-go`
(`otelflags.FlagDomain`), waits for the provider to finish initialising, and records that this
process deliberately gave the instrumentation switches a relay. When you call it, the zero-code
install stands down and you own the provider's lifecycle.

The provider does not have to be GO Feature Flag — any OpenFeature provider works. This is the seam
an embedding SDK uses to own initialisation, evaluation context, logger and shutdown outright; see
[Embedding SDKs](#embedding-sdks-owning-the-provider-yourself).

`openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, provider)` still works and is still
detected. `SetNamedProvider` is preferred for one reason: detecting a raw binding means asking the
OpenFeature SDK which provider is bound to a domain, and that question has no exact answer (see
below), whereas the record `SetNamedProvider` keeps is exact. If you want ONE provider instance to
serve both your own flags and these switches, this function is the only way: bound to both slots, a
provider reads back identical either way and the heuristic sees an unbound domain.

**Never bind the default provider for this purpose.** A provider you installed with
`openfeature.SetProvider` — your application's own flags — is deliberately **not** treated as a relay
for the instrumentation switches. It does not make `RelayPossible()` true, it does not cause the
instrumented implementation to be built, and it never has instrumentation keys evaluated against it.

### Embedding SDKs: owning the provider yourself

If you ship an SDK that wraps these modules and want to own the OpenFeature lifecycle rather than
inherit the zero-code path, install any provider you like with `otelflags.SetNamedProvider` during
your own initialisation, before you construct any wrapper. From then on:

- **Initialisation, logger, poll interval and shutdown are yours.** This library installs nothing,
  and it never calls `Shutdown` on a provider it did not install.
- **The evaluation context is almost entirely yours.** The library attaches the `serviceName` and
  `service.name` attributes *only* on the zero-code auto-install path, so an API-level global context
  you set with `openfeature.SetEvaluationContext` reaches every evaluation unaltered. The one thing it
  always supplies is a targeting key of `<hostname>-<pid>`, on its own domain and its own keys, because
  without one every bucketing rule fails outright. See
  [Targeting](#targeting-one-service-instead-of-the-fleet).
- **The default provider is untouched, in both directions.** Your application's own flags keep
  resolving through whatever you bound to the default slot, and that provider is never asked about an
  instrumentation key.

### Install your provider before constructing wrappers

Whether a relay can exist is resolved **once, when a wrapper is constructed** — an endpoint is
configured, or a provider is bound to `otelflags.FlagDomain`. A wrapper built before either is true
resolves from its environment and options for the rest of its life and **never consults the relay**,
even if you install a provider a moment later.

So: install the provider, *then* construct your clients and connections. Applications using the
zero-code path are unaffected, since the endpoint variable exists before the process starts.

This is also what keeps the pre-dynamic cost profile for everyone else: a process with no relay
allocates no instrumented implementation it cannot use, registers no MongoDB command monitor, and
never initialises any part of the OpenFeature SDK.

### Disable the data collector

`DataCollectorDisabled: true` is not optional on the path where you configure it yourself.

The provider's data collector is on by default. It appends one event per evaluation to a bounded
in-memory buffer and does not clear that buffer when a flush fails. Once the buffer fills, **every
subsequent append flushes synchronously, on the evaluating goroutine, holding the buffer's mutex** —
so a relay outage stalls every instrumented operation behind a failing 10-second request. Because
flags are resolved per operation, the buffer fills in proportion to your traffic.

Nothing is lost by disabling it: it feeds the relay's evaluation-analytics dashboards, and for
process-wide flags evaluated per operation those analytics are a copy of your traffic volume.

### The relay is not a startup dependency

If the relay is unreachable when the provider is installed, log it and carry on. The process comes up
at the state its environment and options declare, with no relay control until the provider fetches
successfully.

Once a fetch has succeeded, an outage changes nothing: the in-process evaluator serves its last
fetched configuration, and no evaluation performs network I/O.

### Knowing that the relay is dead

There is no health API in `otel-flags`. Read the state the OpenFeature SDK already keeps:

```go
state := openfeature.NewClient(otelflags.FlagDomain).State()
```

**Do not gate startup on it.** A process that refuses to come up until the relay answers has turned a
telemetry control plane into an availability dependency — the outcome the whole design refuses.

What a running process reports on its own is a log line, emitted when a flag key's evaluation
outcome **changes** and never per evaluation:

| Error code | Level | What it means |
|---|---|---|
| `FLAG_NOT_FOUND`, `PROVIDER_NOT_READY` | debug | the relay has no opinion. Ordinary — and the only signal you get if you mistyped a key name on the relay |
| `TARGETING_KEY_MISSING`, `TYPE_MISMATCH`, `PARSE_ERROR`, `INVALID_CONTEXT`, `PROVIDER_FATAL`, `GENERAL` | warn | something is broken; the local value stands and the relay cannot change this switch |
| the code clears | info | the relay decides this switch again |

The **returned value** stays the same on every one of those paths — that is what makes the ladder
total — but the silence is gone. Both of the highest-severity findings in the August 2026 review were
invisible for exactly this reason: the code was populated the whole time and nothing read it.

### The startup window

The zero-code install is non-blocking, so between it and the provider's first successful fetch every
switch resolves to its **local** value — environment, option, default.

For *enabling* this is fail-safe. The window can delay a relay-driven enable; it can never introduce
one, and for `otel-mongo` it can never write an `_oteltrace` field your deployment did not configure.

**For disabling it is not, and that is deliberate: a relay `false` does not survive a restart.** If
`OTEL_NATS_TRACING_ENABLED=true` is in your deployment and you turned the module off on the relay, a
restarted process traces again until its first successful fetch — and indefinitely if the relay is
unreachable.

That asymmetry is a design decision, not an oversight. Reading "provider not ready" as `false` would
apply per key, and the master key's local default is `true`, so every restart of every
relay-configured process would be **fully vetoed** until its first fetch, and stay dark for as long as
the relay was down. A control plane must not become an availability dependency, and a source with no
data must not outrank one the deployment wrote down.

So: **the relay is runtime control; durable state belongs in the environment variable.** The
incident-brake procedure is two steps, in this order:

1. Flip the relay flag. It takes effect within the poll interval, on every running process.
2. Land the same value in the deployment's environment variable, before anything restarts. Once both
   agree, the window is harmless.

If you want the relay's answer before the first operation, install your own provider with
`otelflags.SetNamedProvider`, which waits for initialisation and also makes the zero-code install
stand down.

### In-process evaluation only

The provider's in-process mode — background polling, local lookups — is the only supported one. The
zero-code path hardcodes it. Remote evaluation turns every evaluation into an HTTP request, which
would put two or three synchronous network round trips on the path of every Mongo query and NATS
publish.

### Targeting one service instead of the fleet

Set `OTEL_SERVICE_NAME` and write the relay rule against `serviceName`:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  targeting:
    - query: serviceName eq "checkout-api"
      variation: enabled
  defaultRule: { variation: disabled }
```

**Write `serviceName`, not `service.name`.** The library supplies both spellings and they carry the
same value, but only the dot-free one can be matched: a dot is a nested-path separator in both query
languages the relay supports, so `service.name eq "checkout-api"` looks for a field `name` inside an
attribute `service`, finds nothing, and silently falls through to the default rule. `service.name` is
kept because it is the name a reader expects to see in an evaluation context.

Without a rule, a relay flag applies to **every process the relay serves** — which matters more now
that a flag can enable.

Bucketing rules work too. The library supplies a targeting key of `<hostname>-<pid>` on every path,
so `percentage` and `progressiveRollout` canary per process:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  defaultRule:
    percentage: { enabled: 10, disabled: 90 }
```

Per process, not per service — a key derived from the service name would make every percentage
all-or-nothing across the fleet. The key is stable for the life of a container, so a process that
lands in the canary stays there across a restart rather than re-drawing its verdict.

**If you set `service.name` programmatically rather than through the environment**, that value cannot
reach this library on its own. OpenTelemetry's Resource is built from the environment, never written
back to it, and neither the `TracerProvider` interface nor the SDK's concrete type exposes a way to
read a Resource back. So a `service.name` you passed in Go code is, by construction, invisible here.
Two ways to close the gap:

- **You own the provider** (the usual answer for an SDK): set an API-level global evaluation context
  once, and it reaches every evaluation this library makes. Nothing here overrides it — on that path
  the library passes an empty invocation context.

  ```go
  openfeature.SetEvaluationContext(openfeature.NewTargetlessEvaluationContext(
      map[string]any{"service.name": "checkout-api"}))
  ```

- **You use the zero-code path**: set `OTEL_SERVICE_NAME` from the same value during your own
  initialisation, before the first instrumented operation. Setting it only when it is absent leaves a
  deployment that configured it explicitly in charge.

  ```go
  if _, ok := os.LookupEnv("OTEL_SERVICE_NAME"); !ok {
      os.Setenv("OTEL_SERVICE_NAME", serviceName)
  }
  ```

The service-name attributes are supplied by this library only on the zero-code path. The targeting
key is supplied on every path, including one where you installed the provider yourself: it applies
only to this library's own keys on its own domain, and without it every bucketing rule fails with
`TARGETING_KEY_MISSING` and resolves to the local value with no diagnostic anywhere.

Per-request targeting is not supported. The resolver holds no request state.

## How long a change takes to take effect

**The provider's poll interval — 60 seconds by default, widened by up to 10%.** The library
re-resolves on every operation, so the moment the provider has the new configuration, the next
operation uses it; the jitter below is the only delay it adds.

| | Delay |
|---|---|
| The modules' own resolution | none — every operation reads the current value |
| Provider poll interval (zero-code path) | **up to 66 s** — the configured 60 s deviated by at most ±10%, tunable with `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` |
| Provider poll interval (your own provider) | whatever you set; the GO Feature Flag default is **120 s**. This library does not jitter an interval you configured yourself |

The deviation is drawn once per process and fixed for its lifetime, so a fleet started from one
deployment does not keep polling the relay on a shared period. It follows the same rule as the relay
proxy's `enablePollingJitter`, which does this one hop further up, between the relay proxy and your
flag storage.

Be clear about what neither of them spreads: the provider fetches the whole configuration once,
unconditionally, while it initialises, and only then starts its ticker. That request's timing is the
process's own start time, so a simultaneous fleet restart still arrives at the relay together.
Delaying it to scatter that burst is deliberately not done — the window it would lengthen is the one
in which every switch resolves to its local value, and that window is fail-safe for enabling but not
for disabling.

Plan an incident response around the poll interval, not around "immediately" — in **both**
directions, enabling as well as disabling. If 60 s is too slow for your risk profile, lower it: the
poll is a conditional `GET` that returns 304 when nothing changed, so tightening it is cheap.

## What resolution costs

An instrumented operation makes more than one evaluation:

| Module | Evaluations per instrumented operation |
|---|---|
| `otel-nats`, `otel-gorilla-ws` | 2 — master, module |
| `otel-mongo` read | 2 — master, tracing |
| `otel-mongo` write | 3 — master, tracing, propagation |

**Order of magnitude, on developer hardware, one evaluation costs single-digit microseconds and a
handful of allocations** — this repository ships no benchmark for it, so treat that as a shape rather
than a number and measure on your own workload before planning capacity around it.

The cost is the OpenFeature SDK's evaluation pipeline — hook chains, evaluation-context merging, the
provider registry lock — not the flag lookup, so keeping the configuration in memory does not make it
cheaper. Against a Mongo round trip it is noise; against a NATS publish, which already pays a
comparable amount to create a span, it is not. Note that the cost is paid **whatever the flag's
value**: a relay-reachable connection whose module flag is `false` still evaluates on every
operation, and only a process where no relay is possible skips the pipeline entirely.

**A process with no relay configured pays none of it**, and allocates no instrumented implementation
it cannot reach.

The diagnostic above costs nothing in the steady state: one map lookup per evaluation, and a log line
only when a key's outcome changes.

Nothing is cached, deliberately: a cache would make a flag change take effect later than the poll
interval already implies. It sits behind an unchanged internal signature, so it can be added without
affecting any API if a benchmark on a real workload shows it matters. The reasoning is recorded in
the design document.

## What the flags do not control

The switches govern **this library's instrumentation path**, and nothing else. Four boundaries are
worth stating outright, because each one has been mistaken for a bug:

**Turning a module off stops trace-context propagation, not only spans.** The disabled path does not
inject `traceparent` into NATS headers, WebSocket envelopes or Mongo documents, and does not extract
it on the way in. A distributed trace therefore **breaks at that boundary**: work on the far side
starts a new trace rather than continuing yours. If you want to keep trace linking while the library
is silenced, `otel-mongo` supports it explicitly — see [What is not gated](#what-is-not-gated).

**Turning a module on cannot create telemetry your application is not exporting.** The wrappers use
the `TracerProvider` you give them, or the global one. If that is a no-op provider — because the
application never configured the OTel SDK, or configured it off — a relay-driven enable changes which
code path runs and what it costs, but no span is ever exported. Enabling instrumentation and
enabling export are two separate decisions, and the relay only makes the first one.

**The master flag governs these modules only.** `otel-instrumentation-go-tracing` stops every
instrumentation module in this repository. It has no effect on an embedding SDK's own provider, its
other integrations, your application's feature flags, or your export pipeline.

**A handshake cannot be revisited.** For `otel-gorilla-ws`, enabling reaches only connections opened
afterwards, and disabling stops spans and inject/extract but not the envelope. See
[otel-gorilla-ws](#otel-gorilla-ws-negotiation-is-a-handshake-fact).

## Per-connection options

`WithTracingEnabled(v bool)` supplies the **module** tier for one connection or client, for callers
that cannot set a process environment variable — tests, or several independently configured clients
in one binary. It is accepted by `otelnats.ConnectWithOptions` and its TLS and credentials variants,
`otelmongo.ConnectWithOptions` and `NewClient` (v1 and v2), and `otelgorillaws.NewConn` / `Dial` /
`Upgrader.Upgrade`. `otel-mongo` also accepts `WithTracePropagationEnabled(v bool)`.

- It **cannot** supply the master switch, which is process-scoped and accepts no option. A connection
  carrying `WithTracingEnabled(true)` still stops when the master is vetoed.
- It **loses** to its paired environment variable, and to the relay.
- It does **not** make a connection static. A connection carrying one still resolves the master
  switch and the relay on every operation.
- Supplying it alongside its environment variable is legal; the variable wins. (This replaced a rule
  that made it a construction error.)
- An unreadable environment value is still an error even when an option was supplied — the option
  does not excuse a variable that outranks it.

With the variable unset, the option is the deciding rung, so a tracing and a non-tracing connection
can coexist in one process.

## Operational summary

**To turn a module on right now:** set its relay flag to `true`. It takes effect within the poll
interval. For `otel-gorilla-ws` this reaches only connections opened afterwards.

**To turn a module off right now:** set its relay flag to `false`. Four limits to know:

- It stops **trace-context propagation as well as spans**, so distributed traces break at that
  boundary; see *What the flags do not control*.
- It does not stop `ContextFromDocument` / `ContextFromRawDocument` — those are ungated by design;
  see *What is not gated*.
- For `otel-gorilla-ws` it stops spans and inject/extract but **not** the JSON envelope, so the wire
  overhead remains until the connection is cycled.
- It is not durable across a restart. Land the same value in the environment variable; see
  *The startup window*.

**To stop everything in a fleet:** set `otel-instrumentation-go-tracing` to `false`. Nothing below it
can escape that, including connections whose Go code passed an option.

**To stop everything in one deployment, without a relay:** set
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false`.

**To stop one module in one deployment, without a relay:** set `OTEL_<MODULE>_TRACING_ENABLED=false`.
Application code cannot override it.

**To stop one service rather than the fleet:** set `OTEL_SERVICE_NAME` and target `serviceName` in
the relay rule.

**To make a module relay-controllable:** set `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` and create its
flag on the relay. No application code. Deploy at whatever resting state you want — the relay can
move it in either direction from there.

**If you have no relay at all:** the environment and options decide, exactly as before dynamic flags
existed, at exactly the previous cost.
