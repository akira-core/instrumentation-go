## Why

Today every tracing and propagation switch in this repo is an environment variable read once at process start. Turning tracing on to investigate a production incident — or off because it is the cause of one — requires a redeploy. Operators need to flip these switches without restarting the application.

OpenFeature (with the GO Feature Flag provider and its relay proxy) gives us a standard, vendor-neutral way to resolve those switches at runtime while keeping the existing environment variables as the fallback, so nothing changes for deployments that never configure a provider.

## What Changes

### Flag resolution becomes dynamic

- `internal/flags` gains a `Resolver` that resolves each module's flags through the process-global OpenFeature client, using the module's existing environment variable as the OpenFeature **default value**. When no provider is configured (the OpenFeature default is a no-op provider), every read returns the environment value — behavior identical to today.
- Resolved values are cached in a per-module immutable snapshot held in an `atomic.Pointer`, refreshed lazily on read at most once per second. Hot paths pay one atomic load plus one monotonic clock read (~25 ns); they never enter the OpenFeature evaluation pipeline.
- The applications owns the provider: this repo never calls `openfeature.SetProvider` and never sets an evaluation context, exactly as it never initializes a `TracerProvider`.

### Precedence changes

- **BREAKING** The per-module environment variables (`OTEL_MONGO_TRACING_ENABLED`, `OTEL_MONGO_PROPAGATION_ENABLED`, `OTEL_NATS_TRACING_ENABLED`, `OTEL_GORILLA_WS_TRACING_ENABLED`) are demoted from *final say* to *default value used when the relay has no opinion*. A relay flag can now turn a module on that its environment variable leaves off, and vice versa.
- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` becomes a hard, out-of-band kill switch: when it is off, no OpenFeature lookup happens at all and no relay value can enable tracing. It has no counterpart flag on the relay.
- `WithTracingEnabled(v)` remains authoritative and unchanged: when present on a connection, OpenFeature is never consulted for it.

### Strategy selection keys on the global switch alone

- **BREAKING** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` alone now decides whether a wrapper is built on the passthrough (`internal/direct`, no OTel SDK imports) or instrumented (`internal/traced`) path. Processes running with the global switch on and a module switch off previously took the zero-cost passthrough path; they now allocate the instrumented wrapper and perform the runtime check on each call. They still emit no spans.

### otel-gorilla-ws negotiates otel-ws whenever the global switch is on

- **BREAKING** `Dial` offers, and `Upgrader.Upgrade` confirms, the `otel-ws` subprotocol whenever `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is on, independent of the dynamic value. The subprotocol negotiation is the one thing that cannot be changed after a connection is established, so it must be available for a relay flag to be able to enable propagation on a live connection. Consequence: connections between two peers that both use this library with the global switch on now carry the JSON envelope on every message even while tracing is off. Peers that do not negotiate `otel-ws` are unaffected — the wire is unchanged for them.

### Mongo document helpers follow the relay

- **BREAKING** `ContextFromDocument` and `ContextFromRawDocument` resolve through the same per-module snapshot instead of a permanently cached, environment-only gate. A relay flag that disables Mongo propagation now stops change-stream readers from extracting trace context, matching the `Collection` path.

### Removals

- **BREAKING (internal)** `flags.Gate`, `flags.NewGate`, and `Gate.ResetForTest` are deleted. Their three call sites (`natsGate`, `wsGate`, `propEnabledGate`) are replaced by `Resolver`. The package is `internal/`, so no consumer-visible API is removed.

### Not changing

- No new exported types, functions, or functional options in any module.
- No new environment variables. The refresh interval is fixed at one second — relay polling (minutes) dominates the end-to-end latency, so exposing a knob would add surface without changing behavior.
- No business logic, span shapes, attributes, or wire formats other than the otel-ws negotiation consequence above.
- `otel-sampler` is untouched. Dynamic sampling rates are out of scope.

## Capabilities

### New Capabilities

- `dynamic-feature-flags`: OpenFeature-backed runtime resolution of the instrumentation feature flags — the `Resolver`/`Snapshot` primitives, the environment-variable-as-default contract, the TTL refresh semantics, the flag key naming scheme, provider ownership, and the failure/fallback behavior when no provider is configured or evaluation errors.

### Modified Capabilities

- `shared-feature-flags`: `Gate`/`NewGate`/`ResetForTest` removed and their three composed-gate requirements retired; the byte-identical vendoring rule now covers the new `Resolver` code; `EnvEnabled` is unchanged.
- `mongodb-tracing`: three-tier gating restated with the relay as the deciding tier above the module environment variables; trace-context restoration from documents now follows the dynamic snapshot; the disabled-mode strategy split now keys on the global switch alone.
- `nats-jetstream-tracing`: two-tier gating restated with the relay tier; strategy selection keys on the global switch alone.
- `websocket-tracing`: two-tier gating restated with the relay tier; `otel-ws` negotiation gated on the global switch rather than the effective feature flag.

## Impact

**Modules** (all four released together — the byte-identical `internal/flags` copies must change in one commit):

| Module | Version |
|---|---|
| `otel-mongo` | 0.8.0 → 0.9.0 |
| `otel-mongo/v2` | 2.8.0 → 2.9.0 |
| `otel-nats` | 0.7.0 → 0.8.0 |
| `otel-gorilla-ws` | 0.7.0 → 0.8.0 |

**Dependencies**: `github.com/open-feature/go-sdk` enters all four modules' `go.mod`. The GO Feature Flag provider is an application-side dependency, not a library one — this repo depends only on the OpenFeature SDK.

**Code**:
- `*/internal/flags/flags.go` — four byte-identical copies: `Gate` removed, `Resolver`/`Snapshot`/`Spec` added with an injectable clock.
- `otel-mongo/otelmongo/env_flags.go`, `otel-mongo/v2/env_flags.go`, `otel-nats/otelnats/env_flags.go`, `otel-gorilla-ws/env_flags.go` — construct the module's `Resolver` with its flag keys.
- `otel-mongo/{otelmongo,v2}/{client,collection,tracing}.go` and `internal/traced/*` — read the snapshot per call instead of a construction-time boolean.
- `otel-nats/otelnats/*.go`, `otel-nats/oteljetstream/*.go` — same.
- `otel-gorilla-ws/{conn,options,upgrader}.go` — dynamic span gating; negotiation gated on the global switch.

**Testing**: unit tests in all four modules using an in-memory OpenFeature provider and a fake clock; one integration test standing up a real relay proxy container to verify the documented wiring recipe. `otel-testkit` gains test helpers only.

**Documentation**: `CLAUDE.md`, `README.md`, `README.zh-TW.md`, and each module's `CHANGELOG.md` — the precedence table, the flag key reference, and the application-side wiring snippet.
