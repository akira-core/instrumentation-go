# otel-flags

The feature-switch layer shared by this repository's OpenTelemetry instrumentation modules.

It emits no spans and wraps no client. Applications do not normally import it — `otel-mongo`, `otel-mongo/v2`, `otel-nats` and `otel-gorilla-ws` do, and configuration reaches it through environment variables and constructor options. Import it directly only to match on `ErrInvalidFlagValue`.

For the operator-facing reference — every switch, every variable, worked examples and the relay wiring — see [`docs/feature-flags.md`](../docs/feature-flags.md) ([繁體中文](../docs/feature-flags.zh-TW.md)). Hands-on tutorial: [`docs/otel-nats-kill-switch.en-US.html`](../docs/otel-nats-kill-switch.en-US.html).

## The precedence ladder

Every switch resolves down four rungs, first source with an opinion winning:

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

The order is by how late each source is decided: compiled in, written when the wrapper is constructed, set when the process is deployed, changed while it runs. Each later stage overrides the earlier ones.

The option sits **below** its environment variable deliberately. A deployment must be able to disable one module without silencing the process and without a relay, even when the application's Go code asked for it. The case that forces it is `otel-mongo`'s document propagation, which appends a permanent field to the operator's own documents — every other switch merely produces or withholds telemetry.

The whole ladder is one call. `Client.Boolean` returns the value passed to it on every path where the relay has no usable answer — no provider, not ready, key absent, evaluation error, type mismatch — so this package hands it the already-resolved local value and lets the SDK perform the fallback.

## The switches

| Switch | Relay key | Option | Environment variable | Default |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| per-module tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` |
| Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

They compose by conjunction: `tracing = master && moduleTracing`, and `propagation = tracing && mongoPropagation`.

**The master defaults to enabled because it is a veto, not an enabler.** Setting it truthy — in the environment or on the relay — changes nothing. The only value with an effect is `false`, which stops every module in the process, including connections whose Go code passed an option. Do not document it as an enable; it will read as a broken flag.

Nothing turns on because the master is `true`. The per-module default of `false` is what keeps a zero-configuration process silent.

## Environment values are a strict tri-state

`Lookup` has three outcomes and only three:

| Value | Outcome |
|---|---|
| unset | no opinion — resolution falls through to the option, then the default |
| `1` `true` `yes` `on` / `0` `false` `no` `off` (trimmed, case-insensitive) | this source decides |
| anything else, **including the empty string** | `ErrInvalidFlagValue` — the constructor fails |

Guessing is prohibited because under a ladder there is no safe direction to guess in: the master tier defaults to `true` and every other tier to `false`, so a value silently read as `false` would stop a whole fleet on one tier and change nothing on the others.

`export VAR=` is invalid for the same reason. Both readings are wrong somewhere — as `false` it lets an unexpanded `${SOMETHING}` template variable express an opinion the deployment never had; as unset it silently reverses meaning for anyone who used it as an off switch. The rule has no exceptions: **set it to a recognised value, or do not set it.**

## Relay control without Go code

Set three environment variables and nothing else:

| Variable | Meaning |
|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | GO Feature Flag relay proxy URL. Unset ⇒ nothing is installed, no OpenFeature state is written, `RelayPossible()` is false |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | optional; never logged |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | optional; Go duration strings only, default `60s`. A malformed value warns, falls back and still installs |
| `OTEL_SERVICE_NAME` | optional; supplies a `service.name` targeting attribute, on this path only |

The install fires only when the application has installed no provider of its own, and registers a **named** provider on `otel-instrumentation-go`. `DataCollectorDisabled: true` and in-process evaluation are hardcoded, so the zero-code path cannot be misconfigured into the stall those two settings otherwise cause during a relay outage.

An application that installs its own provider keeps it, and must do so **before constructing any wrapper** — `RelayPossible` is resolved at construction, and a wrapper built earlier resolves statically for the rest of its life.

## Things worth knowing

- **A flag change is not immediate.** End-to-end latency is the provider's poll interval, 60 s by default. This module adds none of its own.
- **An instrumented operation makes two evaluations** (three on a Mongo write), at roughly 2 µs and 7 allocations each. Nothing is cached; a cache would fit inside `Resolver` without changing `Value`'s signature.
- **This module never touches the default provider**, the global evaluation context, hooks or shutdown — the same rule the instrumentation packages follow for `TracerProvider`.
- **Nothing shuts the auto-installed provider down.** One poller goroutine per process, ending with the process. An application needing lifecycle control installs its own provider.
