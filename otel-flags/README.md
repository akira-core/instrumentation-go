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
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | GO Feature Flag relay proxy URL, with a scheme and a host (`http://relay:1031`). Unset ⇒ nothing is installed, no OpenFeature state is written, `RelayPossible()` is false |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | optional; never logged |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | optional; a positive Go duration, default `60s`. A value that cannot be read **fails construction** — see below. It sets the centre of the polling period: the effective interval is deviated by at most ±10%, drawn once per process |
| `OTEL_SERVICE_NAME` | optional; supplies `serviceName` and `service.name` targeting attributes, on this path only. Relay rules must key on the dot-free spelling — a dot is a nested-path separator in both query languages |

The install fires only when the application has bound no provider to `otel-instrumentation-go`, and registers a **named** provider on that domain. `DataCollectorDisabled: true` and in-process evaluation are hardcoded, so the zero-code path cannot be misconfigured into the stall those two settings otherwise cause during a relay outage.

A provider the application installed in the **default** slot — its own feature flags — is not treated as a relay for these switches: it does not make `RelayPossible()` true, it never has an instrumentation key evaluated against it, and it does not stand this install down.

It happens during **construction**, in `ValidateAndInstall`, which every wrapper constructor calls and whose error it joins with its own. It does not wait for the provider's first fetch — the startup window below is the consequence — but it is off the evaluation path, so no instrumented operation can be parked behind a provider initialisation.

## A value nobody can read fails construction

The two variables above are validated whether or not a relay is configured, and an unreadable one fails the constructor with an error wrapping `ErrInvalidFlagValue`:

| Variable | Rejected | Why not warn and fall back |
|---|---|---|
| `…_POLL_INTERVAL` | `60`, `soon`, `0s`, `-5s` | a bare integer read as milliseconds turns `60` into `60ms`, and a fleet polling the relay sixty times a second is worse than a failed startup |
| `…_ENDPOINT` | `relay:1031`, `relay`, `http://` | `relay:1031` parses cleanly — as scheme `relay` with opaque `1031` — and yields a provider that can never reach anything |

Blank means "not configured" for both. Unlike a boolean, a duration and a URL have no second reading for `export VAR=` to be misread as, which is why `Lookup` rejects it and these do not. The API key is never validated: any string can be a legitimate key, and it never appears in an error or a log line.

## Installing your own provider

```go
if err := otelflags.SetNamedProvider(provider); err != nil {
    slog.Warn("feature flag provider registration failed", "error", err)
}
```

`SetNamedProvider` binds any OpenFeature provider to `FlagDomain`, waits for it to initialise — so there is no startup window — and records that this process installed one. That record is what makes detection exact: `openfeature.NamedProviderMetadata` falls back to the default provider's metadata when the domain is unbound, so a heuristic alone cannot distinguish an application that bound the same provider to both slots. **Sharing one provider instance between your own flags and these switches requires this function**, for exactly that reason.

Raw `openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, p)` remains supported and is still detected.

Install **before constructing any wrapper** — `RelayPossible` is resolved at construction, and a wrapper built earlier resolves statically for the rest of its life. `SetNamedProvider` warns when it can see that this happened; a raw binding cannot be detected, so nothing warns there.

## Alarming on a dead relay

There is no health API here. Read the state the SDK already maintains:

```go
state := openfeature.NewClient(otelflags.FlagDomain).State()
```

**Do not gate startup on it.** A process that refuses to start until the relay answers has made a telemetry control plane an availability dependency, which is the outcome every other decision in this module exists to avoid.

What a running process reports instead is a log line, and only when something changes. `Value` reads the evaluation's error code and reports a **transition**, never an evaluation:

| Code | Level | Meaning |
|---|---|---|
| `FLAG_NOT_FOUND`, `PROVIDER_NOT_READY` | debug | the relay has no opinion — an ordinary state, and the only signal available to someone who mistyped a key name |
| `TARGETING_KEY_MISSING`, `TYPE_MISMATCH`, `PARSE_ERROR`, `INVALID_CONTEXT`, `PROVIDER_FATAL`, `GENERAL` | warn | something is broken and the relay cannot change this switch |
| the code clears | info | the relay decides this switch again |

## Things worth knowing

- **A flag change is not immediate.** End-to-end latency is the provider's poll interval, 60 s by default, deviated by at most ±10% so that a fleet does not poll the relay on a shared period. The deviation is drawn once per process and is the only delay this module adds; it follows the same rule as the relay proxy's own `enablePollingJitter` one hop further up. The provider's first fetch, during initialisation, is deliberately not delayed — see `docs/feature-flags.md`.
- **Nothing is evaluated unless a provider is bound to `FlagDomain`.** The SDK's `ForEvaluation` falls back to the DEFAULT provider for an unbound domain, so evaluating regardless would resolve instrumentation keys against the application's own flag backend — a network call per instrumented operation if that backend is remote, and a wrong answer outright if it defines a key by the same name. `Value` and `MasterEnabled` short-circuit to the local value instead.
- **An evaluation against a provider you installed is bounded at 250 ms**, after which the local value decides. The auto-installed provider evaluates in process and skips the deadline entirely. The caller's context is deliberately not threaded through: cancelling a Mongo operation must not change what an instrumentation switch resolves to.
- **An instrumented operation makes two evaluations** (three on a Mongo write), and pays for them whatever the flag's value — only a process where no relay is possible skips the pipeline. Order of magnitude on developer hardware: single-digit microseconds each; this repository ships no benchmark, so measure on your own workload. Nothing is cached; a cache would fit inside `Resolver` without changing `Value`'s signature.
- **This module never touches the default provider**, the global evaluation context, hooks or shutdown — the same rule the instrumentation packages follow for `TracerProvider`.
- **Nothing shuts the auto-installed provider down.** One poller goroutine per process, ending with the process. An application needing lifecycle control installs its own provider.
