## 1. Shared `internal/flags` foundation

- [x] 1.1 In `otel-nats/otelnats/internal/flags/flags.go`, delete `Gate`, `NewGate`, and `ResetForTest`; keep `EnvEnabled` unchanged.
- [x] 1.2 In the same file, add `Spec{Key, EnvVar}`, an unexported `snapshot{at, values}`, and `Resolver` holding an `openfeature.IClient`, the specs, the TTL, an injectable `now func() time.Time`, and an `atomic.Pointer[snapshot]`.
- [x] 1.3 Add `NewResolver(domain string, opts ...ResolverOption) *Resolver` with `WithSpecs(...Spec)` and `WithClock(func() time.Time)`; default the clock to `time.Now` and fix the TTL at one second with no exported or env-based override.
- [x] 1.4 Implement `Resolver.Enabled(i int) bool`: load the snapshot, return the cached value when younger than the TTL, otherwise evaluate every spec via `client.Boolean(ctx, spec.Key, EnvEnabled(spec.EnvVar), openfeature.EvaluationContext{})` and store a fresh snapshot. Do not serialize concurrent refreshes.
- [x] 1.5 Update the package doc comment: state that the file must stay byte-identical across the four copies, that the `Resolver` logic is the highest-drift-risk part, and that the package never installs a provider.
- [x] 1.6 Rewrite `flags_test.go` for the new surface: `EnvEnabled` golden table unchanged; add TTL-boundary tests with an injected clock (900 ms stale, 1100 ms refreshed), snapshot-consistency tests across two specs, and fallback tests with no provider / missing flag / erroring provider. No `t.Parallel` in tests that install a provider or call `t.Setenv`.
- [x] 1.7 Copy the finished `flags.go` and `flags_test.go` verbatim (changing only the `package` line if needed) into `otel-mongo/otelmongo/internal/flags/`, `otel-mongo/v2/internal/flags/`, and `otel-gorilla-ws/internal/flags/`; verify byte-identity of all four excluding the `package` line.
- [x] 1.8 Add `github.com/open-feature/go-sdk` to all four modules' `go.mod` and run `go mod tidy` in each.

## 2. otel-nats — dual-implementation strategy split

> Correction to the original breakdown: `otel-nats` does **not** use the cached-gate
> pattern described in `CLAUDE.md`. `otelnats.Conn` already holds a polymorphic
> `connImpl` (`directConn` / `tracedConn`) selected once at construction, and
> `oteljetstream` derives every wrapper from `conn.TracingEnabled()` /
> `conn.TraceContext()`. It therefore needs the same dual-implementation treatment
> as `otel-mongo` (design decision D8), not a field-to-method rename.

- [x] 2.1 In `otel-nats/otelnats/env_flags.go`, replace `natsGate` with a package-level `Resolver` (domain `otel-nats`, one `Spec{Key: "otel-nats-tracing", EnvVar: envNATSTracingEnabled}`) and keep `natsTracingEnabled()` returning `EnvEnabled(global) && resolver.Enabled(idxTracing)`.
- [x] 2.2 Give `Conn` a `static connImpl` field (non-nil when `WithTracingEnabled` was passed, or when the global switch was off at construction) plus `direct`/`traced` fields for the dynamic case, and an `impl()` selector read per call. Convert every `c.impl.X()` call site to `c.impl().X()`.
- [x] 2.3 Apply the same dual-implementation shape to the `oteljetstream` wrappers built from a `Conn` — `JetStream`, `Consumer`, `PushConsumer`, `Stream`, `MessagesContext`, and the batch forwarder goroutines — so long-lived consumers observe relay changes per message rather than inheriting a choice made at construction.
- [x] 2.4 Add unit tests: relay flips tracing off on a live `Conn` (spans stop, headers stop); relay flips on; `WithTracingEnabled` pins behavior against a contradicting relay value; long-lived `MessagesContext` picks up a mid-stream change.
- [x] 2.5 Run `go build ./...`, `go test -v -race ./...`, and `golangci-lint run ./...` in `otel-nats/` until all three pass with zero issues.

## 3. otel-gorilla-ws — dynamic spans, capability-gated negotiation

- [x] 3.1 In `otel-gorilla-ws/env_flags.go`, replace `wsGate` with a package-level `Resolver` (domain `otel-gorilla-ws`, one `Spec{Key: "otel-gorilla-ws-tracing", EnvVar: envWSTracingEnabled}`).
- [x] 3.2 In `options.go`, split the current `effectiveFeatureEnabled` into a **negotiation capability** (option value when present, otherwise `EnvEnabled(global)` — no resolver read) and a **dynamic span gate** (option value when present, otherwise `EnvEnabled(global) && resolver.Enabled(idxTracing)`).
- [x] 3.3 In `conn.go`, turn `Conn.featureEnabled` from a bool field into a method reading the dynamic span gate; leave `Conn.tracingEnabled` (the negotiation outcome) a plain bool set at construction. Keep the two names distinct and document the difference.
- [x] 3.4 In `upgrader.go` and `Dial`, gate offering/confirming `otel-ws` on the negotiation capability from 3.2, resolved before the handshake.
- [x] 3.5 Verify the envelope-while-disabled path: when `tracingEnabled` (negotiated) is true and the dynamic gate is false, `WriteMessage` still wraps the payload, injects nothing, and creates no span; `ReadMessage` still unwraps and creates no span.
- [x] 3.6 Add unit tests: negotiation happens with the global switch on and the relay flag off; relay flip on/off mid-connection changes span emission without breaking the peer; `WithTracingEnabled(false)` suppresses negotiation and all evaluation. Keep the existing `TestUpgrader_TracingDisabled_DoesNotNegotiateOTelWS` and `TestDial_TracingDisabled_DoesNotOfferOTelWS` passing against the global switch rather than the module flag.
- [x] 3.7 Update `otel-ws.md` §5 negotiation scenario tables for the capability-vs-dynamic-flag distinction.
- [x] 3.8 Run `go build ./...`, `go test -v -race ./...`, and `golangci-lint run ./...` in `otel-gorilla-ws/` until all three pass with zero issues.

## 4. otel-mongo v1 — dual-implementation strategy split

- [x] 4.1 In `otel-mongo/otelmongo/env_flags.go`, replace `propEnabledGate` and the plain env reads with a package-level `Resolver` (domain `otel-mongo`, specs `otel-mongo-tracing`/`OTEL_MONGO_TRACING_ENABLED` and `otel-mongo-propagation`/`OTEL_MONGO_PROPAGATION_ENABLED`); keep `mongoTracingEnabled()` and `cachedPropagationEnabled()` names and signatures so call sites are untouched.
- [x] 4.2 Keep `resolveDocumentPropagation(tracingEnabled bool, override *bool)` taking the caller's already-resolved tracing state as a parameter — do not reintroduce an internal recompute — and change its env default to the resolver's propagation value.
- [x] 4.3 In `client.go`, store the `WithTracingEnabled` override on `Client` instead of a resolved bool; add an `effectiveTracing() bool` method. Select the strategy at construction as `override != nil ? *override : EnvEnabled(global)`, and register the command monitor on that same condition.
- [x] 4.4 Change `collectionImpl` so `Find`/`Aggregate`/`Watch` return raw driver types only and the facade constructs both `direct` and `traced` wrappers itself. `FindOne` keeps returning a `shared.SingleResultImpl` — see 4.5 for why.
- [x] 4.5 Give facade `Collection`, `Cursor`, and `ChangeStream` both a `direct` and a `traced` field plus an `impl()` selector reading the effective tracing state per call. When the global switch is off and no option is present, construct only the `direct` implementation.
  - **Correction to the original breakdown:** `SingleResult` is excluded and keeps a single impl fixed by the path its `FindOne` ran through. `traced.SingleResult` holds the live `FindOne` span (ended once via `endOnce` on the first `Decode`/`TraceContext`/`Raw`), so a passthrough `FindOne` leaves nothing to build an instrumented wrapper around, and a mid-flight flip would strand an unended span. Recorded in design.md D8 and the `mongodb-tracing` delta.
- [x] 4.6 Change `traced.Collection.PropagationEnabled` from `bool` to `func() bool` and update every facade-package test that builds a `traced.Collection` literal.
- [x] 4.7 In `tracing.go`, point `ContextFromDocument`/`ContextFromRawDocument` at the resolver snapshot; keep them ignoring per-connection options.
- [x] 4.8 Add unit tests: relay enables/disables tracing on a running `Client`; a long-lived `ChangeStream` switches implementations mid-iteration; `ContextFromDocument` follows the relay within the TTL; `WithTracingEnabled` pins the implementation against a contradicting relay value; `TestConnectWithOptions_DoesNotMutateCallerOptions` still passes.
- [x] 4.9 Run `go build ./...`, `go test -v -race ./...`, and `golangci-lint run ./...` in `otel-mongo/` until all three pass with zero issues.
- [x] 4.10 Verify `internal/direct/` still imports no `go.opentelemetry.io/otel` package by running the CI grep locally.

## 5. otel-mongo v2 — parity

- [x] 5.1 Apply tasks 4.1 through 4.7 identically to `otel-mongo/v2/`, including its `internal/{direct,traced,shared}/` trees.
- [x] 5.2 Preserve the v2-specific `options.MergeClientOptions` workaround: merge through a fresh `options.Client()` base before `SetMonitor` so caller-owned options are never mutated.
- [x] 5.3 Port every test added in 4.8 to v2 and confirm both modules' behavior matches scenario by scenario.
- [x] 5.4 Run `go build ./...`, `go test -v -race ./...`, and `golangci-lint run ./...` in `otel-mongo/v2/` until all three pass with zero issues.
- [x] 5.5 Verify `otel-mongo/v2/internal/direct/` still imports no `go.opentelemetry.io/otel` package.

## 6. Integration test against a real relay

- [x] 6.1 Choose one module (`otel-nats`, the cheapest container footprint) and add a testcontainers-based test that starts a GO Feature Flag relay proxy with a configuration file defining `otel-nats-tracing`.
- [x] 6.2 In that test, install the GO Feature Flag provider through `openfeature.SetProviderAndWait`, assert tracing is on, flip the flag in the relay configuration, wait for the provider's poll plus the resolver TTL, and assert tracing is off.
- [x] 6.3 Add the relay-proxy dependency and the configuration fixture to that module's `tests/integration/` sub-module only; leave the other three integration suites unchanged.
- [x] 6.4 Confirm the new test is picked up by the existing `integration-test` CI job's `go list ./...` minus `/sampling` selection, and that it fits the 300 s timeout.

## 7. Documentation and release

- [x] 7.1 Bump version constants: `otel-mongo/otelmongo/version.go` → `0.9.0`, `otel-mongo/v2/version.go` → `2.9.0`, `otel-nats/otelnats/conn.go` → `0.8.0`, `otel-gorilla-ws/version.go` → `0.8.0`.
- [x] 7.2 Write each module's `CHANGELOG.md` entry, marking BREAKING for the demoted module env vars, the global-switch-only strategy selection, the otel-ws negotiation change (otel-gorilla-ws), and the `ContextFromDocument` behavior change (otel-mongo).
- [x] 7.3 Update `CLAUDE.md`: the feature-flag sections, the precedence table, the flag key table, the `internal/flags` description (Gate removed, Resolver added), the disabled-mode invariant restated on the global switch, and the dual-implementation strategy-split layout.
- [x] 7.4 Update `README.md` and `README.zh-TW.md` with the flag key reference and the application-side wiring snippet (`openfeature.SetProviderAndWait(gofeatureflag.NewProvider(...))` next to `otelsetup.Init()`), stating that the GO Feature Flag provider is an application dependency, not a library one.
- [x] 7.5 Run `go build`, `go test -race`, and `golangci-lint` across all six modules one final time and confirm zero issues everywhere.

## 8. Post-review remediation (PR #27 grill decisions)

> Decisions locked in `design.md` § "Post-review remediation". Implement after design/spec updates; do not start code until those artifacts match.

### 8.1 Correctness

- [x] 8.1.1 **R1+R7 otel-gorilla-ws NewConn / envelope:** Set `NewConn` negotiation from `isOTelWireProtocol(conn.Subprotocol())`; clamp `tracingEnabled && capable` in `configureConn`; feature off + unnegotiated ⇒ raw passthrough; negotiated + feature off ⇒ empty-header envelope (D9); S1 local spans when capable+feature on without negotiation; no force-negotiated Option. Add regression test: global on + module/relay off + NewConn + raw peer payload bytes.
- [x] 8.1.2 **R2 MessageBatch dynamic:** Always return a dispatching batch wrapper; forwarder re-checks connection gate per message; off path skips tracer/attrs/propagator. Tests: same batch true→false and false→true after TTL (mirror Consume/Messages).
- [x] 8.1.3 **R3 Resolver `at`:** In all four `internal/flags` copies, take snapshot `at` at the start of `refresh` (before Boolean loop). Keep unsynchronized last-store-wins; no CAS/mutex.
- [x] 8.1.4 **R5+R16 Mongo torn read / gateState:** Pass `impl()`'s resolved tracing into propagation; ban internal `effectiveTracing()` recompute on that path; same for `ContextFromDocument`/`ContextFromRawDocument`. Factor Client/Database `effective*` into shared gateState (v1+v2).
- [x] 8.1.5 **R9 tracedMessagesContext.Next:** Gate-first delegate to `directMessagesContext` when off.
- [x] 8.1.6 **R12 Consume single resolve:** One flag/impl resolution per message in dynamic consume path; pass tracer/attrs down (coordinate with 8.2.1).

### 8.2 Performance / structure cleanup

- [x] 8.2.1 **R6 JetStream hoist:** Restore construction-time tracer/prop/baseAttrs on tracedConsumeHandler path, tracedMessagesContext, tracedConsumer.Next (mirror `newTracedMessageBatch`); gate remains per-message.
- [x] 8.2.2 **R8 collectionImpl second returns:** Change Find/Aggregate/Watch to `(raw, error)` only; stop throwaway NewCursor/NewChangeStream in direct/traced; keep FindOne dual return. v1+v2.
- [x] 8.2.3 **R11 WriteMessage noop span:** On feature-off capable path use noop span; remove `span != nil` guards.
- [x] 8.2.4 **R13-B1 GlobalTracingPossible:** Add `EnvGlobalTracing` + `GlobalTracingPossible` to four flags copies; delete module `dynamicTracingPossible`/`wsNegotiationPossible`; call sites use `flags.GlobalTracingPossible()`; move D9 prose to Dial/Upgrade/capability docs. Do **not** parallelize refresh Booleans.
- [x] 8.2.5 **R14 selectImpl:** Generics helper for Collection/Cursor/ChangeStream `impl()` in v1 and v2.
- [x] 8.2.6 **R15 relay test helpers:** Move setRelay/InMemoryFlag lifecycle into `otel-testkit/harness`; switch five `dynamic_flags_test.go` files; reset via callback.
- [x] 8.2.7 **R18 dead nil guard:** Remove unreachable nil check in `tracedConsumeHandler`.

### 8.3 Documentation and specs (may land with or just before 8.1)

- [x] 8.3.1 **R4:** CHANGELOG (all affected modules), CLAUDE.md, README(s): "no provider ⇒ no change" must include otel-ws negotiation exception (global-only).
- [x] 8.3.2 **R10:** Remove remaining Gate/propEnabledGate/permanent-cache language from CLAUDE.md, client_option_test comments (v1/v2), oteljetstream consumer/stream godoc; sync main `openspec/specs/shared-feature-flags/spec.md` to Resolver.
- [x] 8.3.3 **R17:** Optional one-line lockstep comment on otelnats `impl`/`msgHandler`/`traceEventMsgHandler` only — no refactor.
- [x] 8.3.4 Confirm delta specs under this change match R1–R3, R5, R8, R13 (websocket, nats-jetstream, mongodb, dynamic-feature-flags, shared-feature-flags).
- [x] 8.3.5 Run `go build`, `go test -race`, `golangci-lint` in every touched module until clean.

### Explicit non-work

- **R19** same-refresh Boolean micro-torn: WONTFIX.
- **R17** policy extract: WONTFIX (comment only).
- Resolver CAS/singleflight, parallel Boolean fan-out: out of scope.
