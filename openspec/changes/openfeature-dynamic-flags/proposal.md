## Why

Today every tracing and propagation switch in this repo is an environment variable read once at process start. When tracing is the cause of a production incident — a noisy exporter, a hot instrumented path, or `_oteltrace` bloating documents — turning it off requires a redeploy. And when an incident needs *more* visibility than the deployment shipped with, turning it on requires one too. Operators need a control plane that works faster than the deployment pipeline, in both directions.

OpenFeature (with the GO Feature Flag provider and its relay proxy) gives us a standard, vendor-neutral way to resolve those switches at runtime. This change wires it in as a **feature toggle**: each switch resolves down a precedence ladder — relay, then the environment variable, then the constructor option, then a hardcoded default — and the relay, being at the top, can both enable and disable without a restart.

The defaults are what keep that safe. Every per-module switch defaults to **off**, so a process that configures nothing traces nothing and no single misplaced variable turns on more than it names. The process-wide master switch defaults to **on** because it is a veto, not an enabler: its only useful value is `false`, and it is the one setting that stops every module in the process regardless of what any option or relay flag says.

## What Changes

### Each switch resolves down a precedence ladder

- `relay > env > option > default`, resolved independently per switch. The mechanism is a single `client.Boolean(ctx, key, local, evalCtx)` call in which `local` is the env-or-option-or-default value computed at construction: the OpenFeature SDK returns that value on every path where the relay has no usable answer — no provider, provider not ready, key absent, evaluation error, type mismatch — so "the relay is silent" and "the relay is unreachable" both fall through to the next rung down.
- Three switches, three roles:

  | Switch | Relay key | Option | Env | Default |
  |---|---|---|---|---|
  | master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
  | per-module tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` |
  | Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

  They compose by conjunction: `tracing = master && moduleTracing`, and `propagation = tracing && mongoPropagation`.
- **BREAKING** The master switch gains a relay key, and its `true` value is inert — creating `otel-instrumentation-go-tracing: true` on a relay does nothing, because that is already the default. Setting it to `false` stops every module in every process the relay serves, overriding options and per-module relay values alike.
- **BREAKING** `WithTracingEnabled` now supplies the **per-module** tier for the connection it is passed to, not the process-wide master. It sits *below* that module's environment variable, so a deployment can override what the application's Go code hardcoded, and above only the hardcoded default. This diverges from released `0.7.0`, where the option won: an application that sets both flips. With the variable unset the option still decides, so two connections in one process can still differ.
- **BREAKING** The mutual-exclusion rule between an environment variable and its option is deleted, together with `ErrTracingConfigConflict` and `ErrTracePropagationConfigConflict`. There are no longer two spellings of one tier, so setting both is ordinary configuration with a defined winner.
- Placing the option below the environment variable is what keeps `OTEL_MONGO_PROPAGATION_ENABLED=false` unbypassable by application code. Every other switch only produces or withholds telemetry; that one appends permanent `_oteltrace` fields to the operator's own documents, so the operator needs a setting the code cannot override.
- Verdicts are **not** cached: the resolver evaluates on every operation, so a relay change takes effect on the next one. This now costs two `Boolean` calls per instrumented operation (three on a Mongo write) at a measured 2.0 µs and 7 allocations each, and is accepted as a deferral — caching sits behind an unchanged `Value(i, local) bool` and can be added later without touching a call site.

### `internal/flags` becomes a published `otel-flags` module

- **BREAKING (internal)** The four byte-identical `internal/flags` copies are deleted. Their contents move to a new module, `github.com/akira-core/instrumentation-go/otel-flags`, which the four instrumentation modules require.
- The forcing requirement is the provider singleton. Four `internal/` packages in four modules share no state, so two of them could observe "no provider installed" concurrently and both register one — the SDK replaces the loser and shuts it down, leaving one live provider eventually but two briefly, and one duplicated relay fetch. Go's minimal version selection resolves one module path to one version per build, so one shared module means one package instance, one `sync.Once`, one provider — structurally.
- Deleted along with the copies: the byte-identical rule, its "maintained by code review, not by a check" caveat, the drift table, the proposed CI hash check, and three redundant copies of `flags_test.go`.
- The module-vocabulary rule survives: `otel-flags` names only process-scoped things (the master switch, the three provider variables, `OTEL_SERVICE_NAME`, `FlagDomain`). Module flag keys, module environment variables and module defaults stay in each module's own `env_flags.go`.
- Local development uses a root `go.work`; CI sets `GOWORK=off` per module so each is verified exactly as a consumer resolves it. A published module cannot carry a `replace` directive.

### Environment values become a strict tri-state

- **BREAKING** `Lookup(name) (value, set bool, err error)` replaces `EnvEnabled(name) bool`. Three outcomes and only three: unset (this source has no opinion, fall through), a recognised value from `1`/`true`/`yes`/`on` or `0`/`false`/`no`/`off` (this source decides), and **anything else — including the empty string — is a construction error**.
- **BREAKING** A deployment carrying `OTEL_MONGO_TRACING_ENABLED=enabled`, `=2`, `=y` or `=` now fails at the first constructor. Under a precedence ladder the safe direction is not uniform — a typo in the master variable would read as `false` and silently stop a fleet, because that tier defaults to `true` — so guessing is worse than failing.
- One exported sentinel, `otelflags.ErrInvalidFlagValue`, wrapped by each module and matchable with `errors.Is`. All of a constructor's reads run before any error is returned, joined in a fixed order, so one run reports every bad value.

### Relay control needs no application code

- Setting `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` makes `otel-flags` construct a GO Feature Flag provider and register it as a **named** provider on the single domain `otel-instrumentation-go`, but only when the application has installed no provider of its own. An application that installs its own keeps it; nothing the library does can change how the application's own flags resolve.
- Two further variables tune it: `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` and `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` (Go duration strings only, default `60s`). `DataCollectorDisabled: true` and in-process evaluation are hardcoded, so the two settings that turn a relay outage into an application stall cannot be omitted on this path. A malformed interval warns and falls back to `60s` rather than removing the control plane.
- The install is non-blocking. The window between it and the provider's first fetch now falls back to the locally-resolved value, so a process starts at exactly the state its environment and options declare and the relay's opinion arrives one fetch later. The window can no longer enable anything that was not already configured on.
- Setting `OTEL_SERVICE_NAME` adds a `service.name` attribute at the invocation site on this path only, so a relay rule can scope itself to one service instead of the whole fleet — which matters more now that a rule can enable.
- Nothing shuts the provider down; its polling goroutine lives for the process lifetime.

### Implementation selection keys on whether a relay can exist at all

- `relayPossible` — the endpoint variable is set, **or** a provider is already bound to our domain — is resolved once per construction. False means the relay is structurally incapable of speaking, so the module resolves from `env > option > default` alone, allocates the instrumented implementation only if that answer is on, and never touches the OpenFeature SDK. **Every configuration that took the zero-cost passthrough path before this change still takes it.**
- True means both implementations are allocated and the per-operation resolution selects between them, because the relay may enable a module the environment left off.
- **New ordering requirement**: an application that installs its own provider must do so **before** constructing any wrapper. A wrapper built earlier resolves statically for its whole life. The zero-code path is unaffected.

### otel-gorilla-ws negotiates on the handshake-time effective value

- `Dial` offers, and `Upgrader.Upgrade` confirms, `otel-ws` when the connection's effective tracing value — master and module, relay included — is on at handshake time. A relay enable therefore reaches connections opened afterwards, not live ones; a relay disable stops spans and inject/extract on live connections but not the envelope, which the peer is still parsing.
- With no relay configured the effective value is exactly what `0.7.0` computed, so such deployments see the previous release's wire byte for byte.
- **BREAKING** `NewConn` becomes `NewConn(conn *websocket.Conn, opts ...Option) (*Conn, error)` so it can report an invalid environment value, matching every other option-accepting constructor.
- `NewConn` enables the envelope only when the raw connection's negotiated subprotocol proves `otel-ws`; it no longer forces envelope wrapping.
- `SubprotocolOTelWS` and `IsOTelNegotiated` are exported so callers running their own handshake can satisfy that requirement instead of hardcoding an internal string. Additive; neither can force an envelope onto a peer that did not negotiate one.
- Two read-path fixes that change returned bytes in the affected cases. The negotiated fact now clamps the **write** decision only: a connection that proved `otel-ws` in a feature-off process writes raw frames but still unwraps on read, instead of handing the peer's `{"header":…,"data":…}` bytes to the application. And `tryUnmarshalWire`'s legacy branch returns the original bytes when neither trace key is present, instead of re-marshalling a map and reordering the caller's JSON keys.

### Mongo document helpers are no longer gated

- **BREAKING** `ContextFromDocument` and `ContextFromRawDocument` lose their feature-flag gate entirely. Neither emits a span, writes to a document, or touches the OTel SDK — they read a `_oteltrace` field the caller already has and return the span context it encodes. The flags exist to stop the library doing work on the caller's behalf; these two do work the caller explicitly asked for at the call site. A process with every switch off now gets a valid span context from them where it previously got nothing, so turning a module off does not stop trace-context extraction.
- `InjectTraceIntoDocument` removes any existing `_oteltrace` before appending. It appended unconditionally, so an ordinary read-modify-write produced two copies — and because extraction resolves the field with `bson.Raw.LookupErr`, which returns the first match, such a loop pinned the trace linkage to the original write permanently.

### Removals

- **BREAKING (internal)** `flags.Gate`, `flags.NewGate`, and `Gate.ResetForTest` are deleted. Their four call sites (`natsGate`, `wsGate`, and one `propEnabledGate` per Mongo module) are replaced by `Resolver`.
- **BREAKING** `ErrTracingConfigConflict` and `ErrTracePropagationConfigConflict` are deleted, together with the mutual-exclusion rule they reported.
- `mongoPropagationEnvOnly()` is deleted: it existed to keep the relay away from static clients, and there are no static clients.

### Not changing

- No new **module-scoped** environment variables. The three added are process-scoped and configure the provider, not any module's behaviour.
- No business logic, span shapes, or attributes. The WebSocket wire format is unchanged in what it emits; two read-path defects are fixed.
- `otel-sampler` is untouched. Dynamic sampling rates are out of scope.

## Capabilities

### New Capabilities

- `dynamic-feature-flags`: OpenFeature-backed runtime resolution of the instrumentation feature flags — the precedence ladder and the single `Boolean` call that implements it, the `Resolver` primitive and its per-call, uncached resolution, the three switches with their defaults and the conjunction that composes them, the `relayPossible` static approximation, the tri-state environment read and its construction error, the flag key naming scheme, the single OpenFeature domain and the single-provider guarantee, provider ownership and the environment-driven auto-install, evaluation-context ownership and the one attribute the auto-install path supplies, the supported evaluation mode, and the fallback behaviour when no provider is configured or evaluation errors.

### Modified Capabilities

- `shared-feature-flags`: `Gate`/`NewGate`/`ResetForTest` removed; the vendored byte-identical `internal/flags` replaced by the published `otel-flags` module and its single-provider guarantee; `EnvEnabled` replaced by a tri-state `Lookup` that errors on an unrecognised or empty value; the composed-gate requirements replaced by the per-switch precedence ladder; the per-connection option becomes the module tier's rung rather than a spelling of the master; the master switch gains a relay key with an inert `true`.
- `mongodb-tracing`: gating restated as `master && tracing` with the relay authoritative and `relayPossible` deciding allocation; document propagation records that `_oteltrace` is never stripped on read, that its default is `false` so something must deliberately enable it, that the relay is now one of the things that can, and that injection must produce exactly one field; trace-context restoration loses its gate entirely; the strategy split keys on `relayPossible` and no wrapper is pinned by an option.
- `nats-jetstream-tracing`: gating restated as `master && tracing` with the relay authoritative; strategy selection keys on `relayPossible`; option-carrying connections still observe relay changes.
- `websocket-tracing`: gating restated as `master && tracing` with the relay authoritative; negotiation gated on the handshake-time effective value, with the asymmetry (enable reaches new connections only, disable leaves the envelope) stated in both directions; `NewConn` no longer forces the envelope and now returns an error; the negotiated fact clamps the write path only, so the read path unwraps on it; the envelope probe is byte-transparent for payloads carrying no trace key; the subprotocol token and negotiation test are exported.

## Impact

**Modules**:

| Module | Version |
|---|---|
| `otel-flags` | **new** — `0.1.0` |
| `otel-mongo` | 0.7.0 → 0.8.0 |
| `otel-mongo/v2` | 2.7.0 → 2.8.0 |
| `otel-nats` | 0.7.0 → 0.8.0 |
| `otel-gorilla-ws` | 0.7.0 → 0.8.0 |

Release ordering is now forced: `otel-flags/v0.1.0` is tagged first, the four modules then require that version with no `replace` directive, and are tagged after. A root `go.work` covers local development and CI sets `GOWORK=off` per module.

**Dependencies**: `github.com/open-feature/go-sdk` and the GO Feature Flag provider enter `otel-flags`' `go.mod`, and the four modules acquire them transitively. The provider brings roughly ten further modules — `go-feature-flag/modules/core`, the ofrep provider, `bluele/gcache`, `diegoholiveira/jsonlogic`, `nikunjy/rules` and a full `antlr4-go/antlr` runtime — into every consumer's build, including consumers that never set the endpoint variable. The cost lands on `go.sum`, vulnerability-scanning surface and licence review rather than runtime, and it is the price of relay control without an application code change: Go initialises packages only from the import graph, so nothing can be made to run from `go.mod` alone. The shared module at least confines the declaration to one `go.mod` instead of four.

**Code**:
- `otel-flags/` — new module: the tri-state `Lookup`, `ErrInvalidFlagValue`, `Resolver` with a lazy client and per-call evaluation, `RelayPossible`, `MasterLocal`/`MasterEnabled`, `FlagDomain`, the master switch's variable and key, the three `OTEL_INSTRUMENTATION_GO_FLAGS_*` names, and the environment-driven provider install on the same `sync.Once` as the lazy client. One `flags_test.go`, one `version.go`, one `CHANGELOG.md`, one `README.md`.
- `*/internal/flags/` — four copies deleted.
- `otel-mongo/otelmongo/env_flags.go`, `otel-mongo/v2/env_flags.go`, `otel-nats/otelnats/env_flags.go`, `otel-gorilla-ws/env_flags.go` — module keys, variables and hardcoded defaults; construct the module's `Resolver`; the conflict sentinels are removed.
- `otel-mongo/{otelmongo,v2}/{client,collection,tracing,gate_state}.go` and `internal/traced/*` — `gateState` carries `relayPossible` and the three local values; per-operation conjunction; static-client paths deleted.
- `otel-nats/otelnats/*.go`, `otel-nats/oteljetstream/*.go` — same; `Conn.static` deleted.
- `otel-gorilla-ws/{conn,options,upgrader}.go` — handshake-time negotiation, per-call span gate, `NewConn` signature.
- `otel-testkit/harness/flags.go` — follows `otel-flags`.

**Testing**: unit tests in all four modules using an in-memory OpenFeature provider installed with `SetNamedProviderAndWait` on the shared domain — no fake clock and no reset hook, since a change is visible on the next operation — covering both directions of the ladder for every switch, the master veto, `relayPossible` in both states including a wrapper built before the provider, the auto-install's fire and stand-down conditions, every invalid environment value, and the two WebSocket read-path fixes. One integration test stands up a real relay proxy container and asserts **both** enable and revoke. Roughly 89 existing call sites that combine an environment variable with a constructor option no longer fail — they must be rewritten to assert that the **environment variable** wins, which is the opposite of released `0.7.0` and therefore needs reading rather than mechanical editing. Tests must construct their wrapper *after* installing the provider, or `relayPossible` freezes them static.

**Documentation**: `feature-flags.md`, `feature-flags.zh-TW.md`, `otel-ws.md`, `CLAUDE.md`, `README.md`, `README.zh-TW.md`, the six module READMEs and each module's `CHANGELOG.md` — the precedence ladder and the three switches with their defaults, the flag key reference including the master's inert `true`, the risk that a relay can now enable `_oteltrace` writes and the three things that bound it, the zero-code wiring path and the three provider variables, the provider-before-wrapper ordering rule, flag-change latency as the provider's poll interval rather than "immediate", the two limits an operator would otherwise misread (extraction is not stopped; the WebSocket envelope survives a disable and an enable does not reach live connections), the reserved `{"header":…,"data":…}` wire structure, the supported provider evaluation mode, the pre-upgrade environment-value audit, and the correction of the false "`_oteltrace` is stripped on read" claim. `VERSIONING.md` gains `otel-flags` and the two-stage release ordering.

**CI**: `.github/workflows/ci.yml` gains a seventh `test-and-lint` matrix entry for `otel-flags` and sets `GOWORK=off` on the per-module steps; `.github/workflows/release-guard.yml` gains an `otel-flags/v[0-9]*` tag pattern validated against `otel-flags/version.go`.

**Downstream**: a deployment that set only a module variable and relied on the global gate to keep it inert now gets that module enabled — the master defaults to `true`. A deployment carrying any `OTEL_*_ENABLED` value outside the two accepted lists, including the empty string, fails at construction and must be corrected before upgrading. The `instrumentation-demo` parent project's NATS demo, which runs with `OTEL_NATS_TRACING_ENABLED=false` and flips the flag on at the relay, works as written under this revision.
