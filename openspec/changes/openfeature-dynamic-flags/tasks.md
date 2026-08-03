> **Sections 1–8 record the first implementation** (commits `f9f363d`, `1d124f1`), built against the
> earlier model in which the relay decided in both directions and `WithTracingEnabled` pinned a
> connection static. That model was replaced during design review; see `design.md` §
> "Superseded decisions". Those sections are kept as the historical record of what shipped —
> **section 9 revises it** and is the work that remains. Where the two disagree, section 9 wins.

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
- [x] 6.2 In that test, install the GO Feature Flag provider through `openfeature.SetProviderAndWait`, assert tracing is on, flip the flag in the relay configuration, wait for the provider's poll, and assert tracing is off.
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

## 9. Kill-switch model rework

> Locked in `design.md` D2/D3/D14/D15/D16 and the five delta specs. Do not start code until those
> artifacts are reviewed. All four modules land in one commit.

### 9.1 Shared `internal/flags` (four byte-identical copies)

- [ ] 9.1.1 Rewrite `EnvEnabled` as a truthy allow-list: enabled only for `1`/`true`/`yes`/`on` after `strings.ToLower(strings.TrimSpace(v))`; unset and every other value disabled. Update the doc comment to state the allow-list, not the falsy list.
- [ ] 9.1.2 Add `EnvSet(name string) bool` (bare `os.LookupEnv` presence) plus `GlobalTracingSet()`. Document that `EnvSet` is for the mutual-exclusion check only and must never decide whether a switch is enabled.
- [ ] 9.1.3 Change the resolver's evaluation default to a literal `true`. Delete the `Spec` type and replace `WithSpecs(...Spec)` with `WithFlagKeys(keys ...string)` — with the env var no longer the evaluation default, `Spec.EnvVar` has no reader and would rot. Rename `Enabled(i)` to `Allowed(i)` so the call site reads as a relay verdict, not a final answer.
- [ ] 9.1.4 **Delete the cache.** Remove `snapshot`, the `atomic.Pointer`, `refreshTTL`, `refresh`, `now`/`WithClock`, and the TTL comparison. `Allowed(i)` becomes a bounds check plus one `client.Boolean(context.Background(), r.keys[i], true, openfeature.EvaluationContext{})`. Keep the lazy `clientOnce` construction. See design D4 for the measured cost this accepts (2.0 µs / 7 allocs per call vs 82 ns cached) and why it is a deferral rather than a rejection.
- [ ] 9.1.5 Update the package doc comment: the file is still the highest-drift-risk shared code, it still never installs a provider, and it now caches nothing — with a pointer to D4 so anyone reintroducing a cache knows which questions come back with it.
- [ ] 9.1.6 Rewrite `flags_test.go`: allow-list golden table including the empty string and `enabled`/`2`; `EnvSet` vs `EnvEnabled` divergence; `Allowed` returning `true` with no provider installed; `Allowed` returning `false` for an out-of-range index; a provider mutation observed on the very next call with no sleep. No `t.Parallel` where a provider or `t.Setenv` is involved.
- [ ] 9.1.7 Copy `flags.go` and `flags_test.go` verbatim into the other three modules; verify byte-identity excluding the `package` line.

### 9.2 Per-module composition and conflict errors

- [ ] 9.2.1 In each module's `env_flags.go`, compose `gate1 && EnvEnabled(moduleEnv) && resolver.Allowed(idx)` with short-circuit ordering so a falsy module env var never reaches the resolver.
- [ ] 9.2.2 Export `ErrTracingConfigConflict` per module, and `ErrTracePropagationConfigConflict` in `otel-mongo` v1 and v2. Returned errors wrap the sentinel and name both observed values. In `otel-mongo`, run both checks before returning and combine with `errors.Join` in a fixed order (tracing, then propagation) — never return on the first failure.
- [ ] 9.2.3 Add the presence-based mutual-exclusion check to every option-accepting constructor: `otelnats.ConnectWithOptions`/`ConnectTLSWithOptions`/`ConnectWithCredentialsWithOptions`, `otelmongo.ConnectWithOptions` (v1 and v2), `otelgorillaws.NewConn`/`Dial`/`Upgrader.Upgrade`. Check before any other work so a rejected construction opens no connection. `otelmongo.NewClient` (v1 and v2) delegates to `ConnectWithOptions` and inherits the check — do not duplicate it there.

### 9.3 Remove static connections

- [ ] 9.3.1 `otel-nats`: delete `Conn.static`; `impl()` selects on the per-operation conjunction. Keep the lockstep comment tying `impl`/`msgHandler`/`traceEventMsgHandler` together.
- [ ] 9.3.2 `otel-mongo` v1 and v2: delete `mongoPropagationEnvOnly()` and `gateState`'s static branch; `effectiveTracing`/`propagationGiven` read the relay verdict per call; `tracedBuilt` keys on `gate1 && EnvEnabled(envMongoTracingEnabled)` so a module-off process allocates no instrumented wrapper (D7). Same for `otel-nats`'s `traced` field in 9.3.1.
- [ ] 9.3.3 `otel-gorilla-ws`: `capable = gate1 && EnvEnabled(envWSTracingEnabled)` resolved once at construction; `featureEnabled()` = `capable && wsResolver.Allowed(idxTracing)` per call; delete the `featureOverride` short-circuit in `featureEnabled`.
- [ ] 9.3.4 Remove the feature-flag gate from `ContextFromDocument`/`ContextFromRawDocument` (v1+v2): delete the `if !cachedPropagationEnabled()` early return from both, then delete `cachedPropagationEnabled()` and `mongoFlagsPair()`, which lose their only caller. Neither helper emits a span or writes a document (D10).

### 9.4 otel-gorilla-ws surface

- [ ] 9.4.1 Change `NewConn` to `(*Conn, error)`; update the four in-repo call sites and both module READMEs.
- [ ] 9.4.2 Gate negotiation on the static capability from 9.3.3 in `Dial` and `Upgrader.Upgrade`; confirm `TestUpgrader_TracingDisabled_DoesNotNegotiateOTelWS` and `TestDial_TracingDisabled_DoesNotOfferOTelWS` still pass and add the module-switch-off case.
- [ ] 9.4.3 Update `otel-ws.md` §5: capability is now fully static and there is no relay-driven negotiation exception.

### 9.5 Tests

- [ ] 9.5.1 Rewrite every test that sets a tracing env var **and** passes the matching option (~89 call sites across 11 files) to use exactly one of them.
- [ ] 9.5.2 Add per-module kill-switch asymmetry tests: relay `true` + module env off ⇒ no spans and no evaluation; relay `false` + module env on ⇒ the running connection's next operation emits no span.
- [ ] 9.5.3 Add constructor-conflict tests for all seven option-accepting constructors, asserting `errors.Is` against the module sentinel.
- [ ] 9.5.4 Update the relay integration test to assert the revoke direction (start enabled, revoke, observe stop) instead of enabling from off.
- [ ] 9.5.5 Run `go build`, `go test -race`, `golangci-lint` in every touched module until clean.

### 9.6 Outstanding correctness items found during review

- [ ] 9.6.1 **R14 was marked done but is not implemented.** `collection.go`, `cursor.go` and `results.go` each hand-roll `impl()` in both Mongo modules. Either add the generics helper or reopen R14 as WONTFIX with a reason.
- [ ] 9.6.2 Write a test for read-modify-write duplicate `_oteltrace`: read a document into `bson.M`, modify, `ReplaceOne`. If the field is written twice, make `InjectTraceIntoDocument`/`InjectTraceIntoUpdate` remove any existing key before appending. Independent of the relay; affects v1 and v2.

### 9.7 Documentation

- [ ] 9.7.1 Correct `CLAUDE.md`'s transport table: `_oteltrace` is **not** stripped on read. Check `README`s and module docs for the same claim.
- [x] 9.7.2 Create `feature-flags.md` at the repo root — following the existing convention of flat, English-only design notes (`otel-ws.md`, `VERSIONING.md`) — as the single home for the flag reference. It holds: the `gate1` resolution table (env var × option, including the construction-error row), the effective-tracing table (`gate1` × module env × relay verdict), the `otel-mongo` `_oteltrace` propagation table, the `otel-gorilla-ws` capability / negotiation / span-gate table, the truthiness allow-list with its worked examples (empty string and `enabled` both disable), the flag key ↔ environment variable pairing, and the list of what "the relay has no opinion" covers.
- [ ] 9.7.3 In `feature-flags.md`, state plainly that the relay can only revoke: it cannot enable anything, sites wanting relay control must deploy with the module switches on, and there is no relay key that stops the whole process — all four flags must be revoked individually.
- [ ] 9.7.4 In `feature-flags.md`, document the supported provider evaluation mode (in-process, the provider's default) and that remote evaluation is unsupported because it puts an HTTP request on the operation path.
- [ ] 9.7.4b In `feature-flags.md` and in every wiring snippet, state that `openfeature.SetProviderAndWait` is **required**, not preferred, and give the reason: an unresolvable flag means "allow", so a not-ready provider cannot revoke, and a process restarting under an active revocation would run instrumented until the provider catches up.
- [ ] 9.7.4c Reduce the flag material in `README.md` and `README.zh-TW.md` to a short summary plus a link to `feature-flags.md`. The link and the repo-tree entry are already in place, along with a banner marking the README summary as the released behaviour and `feature-flags.md` as the incoming model; when the code lands, drop that banner, delete the duplicated tables from both READMEs, and leave only the summary and the link. One home only, so the two cannot drift.
- [ ] 9.7.5 Rewrite `CLAUDE.md`'s feature-flag, disabled-mode-invariant and `internal/flags` sections for the kill-switch model. The invariant's bullet list must say what it protects rather than list mechanisms: within **gated** code paths, no span creation, no SDK or exporter initialisation, no attribute-slice build, and no trace-context **injection** or extraction — with explicitly-invoked read-only helpers (`ContextFromDocument`, `ContextFromRawDocument`) named as outside its scope, since they emit no telemetry and the caller has already stated intent. replace the two-pattern table with the three-pattern one from `design.md` § Context. The current table is wrong twice: it calls `otel-nats` a cached gate and `oteljetstream` a per-call gate (both were strategy splits — `directConn`/`tracedConn`, `directJSImpl`/`tracedJSImpl`), and it groups `otel-mongo` Client/Database with `otel-gorilla-ws` as cached gates when neither creates a span and neither holds a gate — they are gate carriers.
- [ ] 9.7.6 Rewrite each module's `CHANGELOG.md` entry for the new BREAKING set: truthiness allow-list, mutual exclusion, no static connections, `NewConn` signature, and the ungating of `ContextFromDocument`/`ContextFromRawDocument` (a fully-disabled process now gets a span context from them where it previously got nothing — deployments that switched the env var off specifically to stop trace linking must stop calling them instead). Remove the withdrawn otel-ws negotiation exception.
- [ ] 9.7.7 File a follow-up for the `instrumentation-demo` parent project: its NATS demo enables tracing from the relay, which this model forbids; the deployment must set `OTEL_NATS_TRACING_ENABLED=true` and the demo must invert to revoke-then-restore.
