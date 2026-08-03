## Why

Today every tracing and propagation switch in this repo is an environment variable read once at process start. When tracing is the cause of a production incident — a noisy exporter, a hot instrumented path, or `_oteltrace` bloating documents — turning it off requires a redeploy. Operators need a brake that works faster than the deployment pipeline.

OpenFeature (with the GO Feature Flag provider and its relay proxy) gives us a standard, vendor-neutral way to resolve that brake at runtime. This change wires it in as a **revoke-only** control: an operator can turn instrumentation off without restarting, and nothing on the relay can turn anything on.

## What Changes

### The relay becomes a kill switch

- `internal/flags` gains a `Resolver` that resolves each module's flags through the process-global OpenFeature client with an evaluation default of `true`, and the module's environment variable is ANDed separately rather than passed as that default. A relay `false` disables a module the deployment enabled; no relay value enables a module the deployment left off.
- Every failure path — no provider, provider not ready, flag absent, evaluation error, relay unreachable — resolves to `true`, meaning *do not interfere*, so an application that never configures a provider behaves exactly as its environment says.
- Resolved verdicts are cached in a per-module immutable snapshot held in an `atomic.Pointer`, refreshed lazily on read at most once per second, under a bounded context. Hot paths pay one atomic load, one monotonic clock read and one env read; they never enter the OpenFeature evaluation pipeline per operation.
- The application owns the provider: this repo never calls `openfeature.SetProvider` and never sets an evaluation context, exactly as it never initializes a `TracerProvider`.

### The first-tier switch may be set in exactly one place

- **BREAKING** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `WithTracingEnabled(v bool)` become two spellings of the same tier and are mutually exclusive. Supplying both — even with the same value — returns an error from the constructor. Each module exports a sentinel for `errors.Is`.
- **BREAKING** `otel-mongo` applies the same rule to `OTEL_MONGO_PROPAGATION_ENABLED` and `WithTracePropagationEnabled`.
- **BREAKING** `WithTracingEnabled` no longer makes a connection static. A connection carrying it still reads the module environment variable and the relay verdict per operation, and still stops when the relay revokes. There is no way to opt a connection out of a revocation.

### Environment truthiness becomes an allow-list

- **BREAKING** `flags.EnvEnabled` treats only `1`, `true`, `yes`, `on` (trimmed, case-insensitive) as enabled. Every other set value — the empty string, `enabled`, `2`, `y` — now reads as disabled, where previously anything outside a four-item falsy list enabled the switch. `export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=` no longer opens the gate.
- A new `flags.EnvSet` predicate reports presence only, so the mutual-exclusion check can distinguish "unset" from "set to something falsy".

### Strategy selection keys on the static part of the decision

- `gate1 && OTEL_<MODULE>_TRACING_ENABLED` — both environment-derived and fixed at construction — decides whether a wrapper is built on the passthrough (`internal/direct`, no OTel SDK imports) or instrumented (`internal/traced`) path. All four modules now use the same expression; it is the one `otel-gorilla-ws` already needed for subprotocol negotiation.
- Every configuration that took the zero-cost passthrough path before this change still takes it. That is only possible because the relay can no longer enable: a module switched off in the environment can never need the instrumented path, so it is not allocated.

### otel-gorilla-ws negotiation stays on the static capability

- `Dial` offers, and `Upgrader.Upgrade` confirms, `otel-ws` when `gate1 && OTEL_GORILLA_WS_TRACING_ENABLED` is on — the relay verdict is excluded, which costs nothing because the relay can never enable. Upgrading without a provider therefore changes **nothing** on the wire, with no exception.
- **BREAKING** `NewConn` becomes `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)` so it can report a configuration conflict, matching every other option-accepting constructor.
- `NewConn` enables the envelope only when the raw connection's negotiated subprotocol proves `otel-ws`; it no longer forces envelope wrapping.

### Mongo document helpers are no longer gated

- **BREAKING** `ContextFromDocument` and `ContextFromRawDocument` lose their feature-flag gate entirely. Neither emits a span, writes to a document, or touches the OTel SDK — they read a `_oteltrace` field the caller already has and return the span context it encodes. The flags exist to stop the library doing work on the caller's behalf; these two do work the caller explicitly asked for at the call site. A process with every switch off now gets a valid span context from them where it previously got nothing.

### Removals

- **BREAKING (internal)** `flags.Gate`, `flags.NewGate`, and `Gate.ResetForTest` are deleted. Their three call sites (`natsGate`, `wsGate`, `propEnabledGate`) are replaced by `Resolver`. The package is `internal/`, so no consumer-visible API is removed.
- `mongoPropagationEnvOnly()` is deleted: it existed to keep the relay away from static clients, and there are no static clients.

### Not changing

- No new environment variables. The refresh interval and timeout are fixed — relay polling (minutes) dominates the end-to-end latency, so exposing knobs would add surface without changing behavior.
- No business logic, span shapes, attributes, or wire formats.
- `otel-sampler` is untouched. Dynamic sampling rates are out of scope.

## Capabilities

### New Capabilities

- `dynamic-feature-flags`: OpenFeature-backed runtime **revocation** of the instrumentation feature flags — the `Resolver`/`Snapshot` primitives, the always-`true` evaluation default and its kill-switch semantics, the TTL and refresh-timeout behavior, the truthiness allow-list, the mutual-exclusion rule, the flag key naming scheme, provider ownership and supported evaluation mode, and the failure/fallback behavior when no provider is configured or evaluation errors.

### Modified Capabilities

- `shared-feature-flags`: `Gate`/`NewGate`/`ResetForTest` removed; the three-tier conjunction replaces the composed-gate requirements; `EnvEnabled` gains an allow-list and `EnvSet` is added; the per-connection option becomes a mutually exclusive spelling of the first tier rather than an override; the byte-identical vendoring rule now covers the `Resolver` code.
- `mongodb-tracing`: three-tier gating restated with the relay as a revoke-only tier; document propagation records that `_oteltrace` is never stripped on read and that only a deployment can enable it; trace-context restoration follows the snapshot; the strategy split keys on the first tier alone and no wrapper is pinned by an option.
- `nats-jetstream-tracing`: two-tier gating restated with the revoke-only relay tier; strategy selection keys on the first tier alone; option-carrying connections still obey revocations.
- `websocket-tracing`: two-tier gating restated with the revoke-only relay tier; negotiation gated on the static capability with no wire-format exception; `NewConn` no longer forces the envelope and now returns an error.

## Impact

**Modules** (all four released together — the byte-identical `internal/flags` copies must change in one commit):

| Module | Version |
|---|---|
| `otel-mongo` | 0.8.0 → 0.9.0 |
| `otel-mongo/v2` | 2.8.0 → 2.9.0 |
| `otel-nats` | 0.7.0 → 0.8.0 |
| `otel-gorilla-ws` | 0.7.0 → 0.8.0 |

**Dependencies**: `github.com/open-feature/go-sdk` enters all four modules' `go.mod`, ending `internal/flags`'s zero-dependency property. The GO Feature Flag provider is an application-side dependency, not a library one.

**Code**:
- `*/internal/flags/flags.go` — four byte-identical copies: `Gate` removed; `Resolver`/`Snapshot`, `EnvSet`, the truthiness allow-list, the multi-value accessor and the bounded refresh added.
- `otel-mongo/otelmongo/env_flags.go`, `otel-mongo/v2/env_flags.go`, `otel-nats/otelnats/env_flags.go`, `otel-gorilla-ws/env_flags.go` — construct the module's `Resolver`, own the conflict sentinels.
- `otel-mongo/{otelmongo,v2}/{client,collection,tracing,gate_state}.go` and `internal/traced/*` — three-tier conjunction per call; static-client paths deleted.
- `otel-nats/otelnats/*.go`, `otel-nats/oteljetstream/*.go` — same; `Conn.static` deleted.
- `otel-gorilla-ws/{conn,options,upgrader}.go` — static capability, per-call span gate, `NewConn` signature.

**Testing**: unit tests in all four modules using an in-memory OpenFeature provider and a fake clock, covering both directions of the kill-switch asymmetry; one integration test standing up a real relay proxy container to verify the documented wiring recipe in the revoke direction. Roughly 89 existing call sites that combine an environment variable with a constructor option must be rewritten to use exactly one of them.

**Documentation**: `CLAUDE.md`, `README.md`, `README.zh-TW.md`, and each module's `CHANGELOG.md` — the resolution table, the flag key reference, the kill-switch semantics and the capability that is deliberately absent (remote enablement), the supported provider evaluation mode, and the correction of the false "`_oteltrace` is stripped on read" claim.

**Downstream**: a deployment that relied on the relay to *enable* instrumentation must move that decision into its environment configuration. The `instrumentation-demo` parent project is in this position: its NATS demo runs with `OTEL_NATS_TRACING_ENABLED=false` and flips the flag on at the relay, which this change makes impossible; the demo inverts to "revoke, then restore".
