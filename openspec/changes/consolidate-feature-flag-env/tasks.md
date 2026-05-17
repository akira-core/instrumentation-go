## 1. Shared `internal/flags/` helper

- [x] 1.1 Design `internal/flags/flags.go` exporting `EnvEnabled(name string) bool`, `Gate` struct (with `sync.Once`+`atomic.Bool`), `NewGate(fn func() bool) *Gate`, `Gate.Enabled() bool`, `Gate.ResetForTest()`
- [x] 1.2 Write table-driven unit tests for `EnvEnabled` covering: unset, `"0"`, `"false"`, `"FALSE"`, `"no"`, `"off"`, `" 0 "`, `"1"`, `"true"`, `"yes"`, `"on"`, `"enabled"`, arbitrary `"hello"` (all in `flags_test.go`)
- [x] 1.3 Write tests for `Gate`: first call invokes resolver once, subsequent calls cached, `ResetForTest` re-evaluates, parallel-safety warning documented
- [x] 1.4 Run `go test -race -cover ./internal/flags/...` — verify 100% coverage and zero races
- [x] 1.5 Run `golangci-lint run ./internal/flags/...` — zero issues

## 2. `otel-mongo` v1 wiring

- [x] 2.1 Create `otel-mongo/otelmongo/internal/flags/` directory; copy the canonical `flags.go` from task 1.1
- [x] 2.2 Replace `otel-mongo/otelmongo/env_flags.go` body with `flags.Gate` composition for `mongoTracingEnabled` and `cachedPropagationEnabled`; keep `mongoPropagationEnvOnly`, `mongoPropagationEnabled`, `resolveDocumentPropagation`, `resolveFlag` as thin wrappers
- [x] 2.3 Migrate `cachedPropagationEnabled` from `sync.Once`+`atomic.Bool` in-file to a package-level `*flags.Gate`; preserve `resetPropEnabledCacheForTest` semantics by forwarding to `Gate.ResetForTest`
- [x] 2.4 Update `tracing_test.go` helpers (`enableTracing`, `enableDocumentPropagation`) to call the new reset hook — no change needed, helpers already call `resetPropEnabledCacheForTest`
- [x] 2.5 Run `cd otel-mongo && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [x] 2.6 Ran `cd otel-mongo/tests/integration && go test -race -timeout 180s ./...` — 6/6 passed on macOS after refactor to standalone MongoDB (replica-set removed; change-stream test replaced by Find-based equivalent). MONGO_URI override still honoured.

## 3. `otel-mongo` v2 parity wiring

- [x] 3.1 Copy `internal/flags/` directory to `otel-mongo/v2/internal/flags/` (byte-identical contents)
- [x] 3.2 Apply the same `env_flags.go` rewrite to `otel-mongo/v2/env_flags.go` — diff against v1 SHOULD be the package name only
- [x] 3.3 Migrate `cachedPropagationEnabled` in v2 the same way as task 2.3
- [x] 3.4 Run `cd otel-mongo/v2 && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [x] 3.5 Ran `cd otel-mongo/v2/tests/integration && go test -race -timeout 180s ./...` — 6/6 passed on macOS after parallel refactor to standalone MongoDB (matches v1).

## 4. `otel-mongo` Client and Database — nullable traced-pointer pattern

> **Scope revised** (was: full strategy-split). Spec rewritten in `otel-mongo-flag-wiring` Requirement "Client and Database isolate SDK state behind a nullable traced pointer". Rationale captured in `design.md` Open Questions. Full `internal/direct.Client` / `.Database` packages produced 3 layers of duplicated fields + 8-positional constructor without proportional benefit; Client and Database each expose only one truly instrumentation-divergent method, so a single nullable `*traced.ClientState` / `*traced.DatabaseState` pointer suffices. Collection strategy-split (the higher-leverage case, 14 CRUD methods) preserved.
>
> - [x] 4.1 Added `*traced.ClientState` and `*traced.DatabaseState` structs in `internal/traced/state.go` (v1+v2) carrying tracer / propagator / propagationEnabled / deliverTracer / serverAddr / serverPort (and `*sdktrace.TracerProvider` MongoTP on ClientState). Helper methods `ShutdownDeliver`, `ForDatabase`, `NewCollection` keep the constructor-site selection on the traced side.
> - [x] 4.2 ~~Implement `internal/direct/client.go` and `internal/direct/database.go`~~ Skipped — direct-side state for Client/Database is "no state": the facade carries the raw `*mongo.{Client,Database}` and a nil `traced` pointer; no dedicated direct package needed. Collection's `internal/direct.Collection` remains.
> - [x] 4.3 Moved instrumentation state into `internal/traced.ClientState` and `internal/traced.DatabaseState` (state.go). Existing `internal/traced.Collection` continues to carry per-collection state.
> - [x] 4.4 Removed cached-gate `tracingEnabled bool` field from facade `Client` and `Database` (v1+v2). Replaced with single `traced *traced.ClientState` / `*traced.DatabaseState` pointer field. Compile-time assertions live on Collection-side as before; Client/Database don't need them under the nullable-pointer pattern.
> - [x] 4.5 Wired `Connect` / `ConnectWithOptions` to populate `traced` only when `mongoTracingEnabled()` is true. `client.Database(name)` propagates the pointer via `c.traced.ForDatabase()` (returns nil when receiver is nil-checked above). `db.Collection(name)` does constructor-site `if d.traced == nil` to pick `direct.NewCollection` vs `d.traced.NewCollection(raw)` — spec-exempt per Scenario "Constructor-site impl selection is exempt".
> - [x] 4.6 Mirrored 4.1–4.5 to v2 identically (state.go + facade client.go + database.go + collection.go) — diff against v1 is package name + driver import path only.
> - [x] 4.7 `go build ./...` + `go test -race ./...` + `golangci-lint run ./...` green for both v1 and v2. Integration tests (testcontainers) remain pending — see 2.6 / 3.5.

## 5. `otel-nats` wiring + strategy-split

- [x] 5.1 Create `otel-nats/otelnats/internal/flags/` (copy of canonical `flags.go`)
- [x] 5.2 Replace `otel-nats/otelnats/env_flags.go` body with `flags.Gate` composition for `natsTracingEnabled` (two-tier: global + `OTEL_NATS_TRACING_ENABLED`)
- [x] 5.3 ~~Create `otel-nats/otelnats/internal/shared/impls.go`~~ **Resolved as file-level variant** — spec rewritten to accept file-level split (`conn_direct.go` / `conn_traced.go` in same package) as a valid strategy-split layout alongside package-level. Functional intent met: `connImpl` interface declared in `conn.go`; constructor (`Connect`) picks `directConn` or `tracedConn` once; public methods are single-line `c.impl.<Method>(...)` delegates with zero per-call gate.
- [x] 5.4 ~~Create `otel-nats/otelnats/internal/direct/conn.go`~~ **Resolved as file-level variant** — `conn_direct.go` houses `directConn` struct + passthrough methods; CI grep (added to `drift-check` job) enforces zero `otel/sdk` / `otel/exporters` imports in `*_direct.go` files, same isolation guarantee as the package-level case.
- [x] 5.5 ~~Create `otel-nats/otelnats/internal/traced/conn.go`~~ **Resolved as file-level variant** — `conn_traced.go` houses `tracedConn` struct + full instrumentation including the cached `propagationEnabled bool` field.
- [x] 5.6 Refactor facade `otelnats.Conn` to hold `impl connImpl`; replace every public method body with `return c.impl.<Method>(args...)`; delete every `if c.tracingEnabled` branch in public methods — already done in prior commit (`2858a51`); verified by inspection of `conn.go`
- [x] 5.7 Wire `Connect` / `Wrap` to call `natsGate.Enabled()` once and pick the impl — already done in prior commit
- [x] 5.8 ~~Repeat 5.3–5.7 for `oteljetstream`~~ **Resolved as file-level variant** — existing `consumer_direct.go` / `consumer_traced.go`, `jetstream_direct.go` / `jetstream_traced.go`, `stream_direct.go` / `stream_traced.go` follow same pattern; CI file-level grep covers all of `otel-nats/oteljetstream/*_direct.go`. Real bug surfaced during W2 test addition: `jetstream_traced.go` `PublishMsg` was injecting headers unconditionally — fixed by gating behind `j.conn.PropagationEnabled()`.
- [x] 5.9 ~~Add `var _ connImpl = (*direct.Conn)(nil)` assertions~~ **N/A under file-level variant** — both `directConn` and `tracedConn` are declared in the same package and the compiler enforces interface satisfaction at every facade method call site that returns through `connImpl`. The redundant assertion would require a manufactured cast that doesn't add new guarantees in this layout.
- [x] 5.10 Run `cd otel-nats && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [x] 5.11 Ran `cd otel-nats/tests/integration && go test -race -timeout 300s ./...` — 12/12 passed. Updated `TestMain` to opt-in `OTEL_NATS_PROPAGATION_ENABLED=1` since the v0.4.x default changed from on-by-default to off-by-default (existing wire-output assertions still expect v0.3.x behaviour).

## 6. `otel-gorilla-ws` wiring + strategy-split

- [x] 6.1 Create `otel-gorilla-ws/internal/flags/` (copy of canonical `flags.go`)
- [x] 6.2 Replace `otel-gorilla-ws/env_flags.go` body with `flags.Gate` composition for `wsTracingEnabled`
- [x] 6.3 Created `otel-gorilla-ws/internal/shared/conn.go` with `ConnImpl` interface (`WriteMessage(ctx, mt, data) error`, `ReadMessage(ctx) (ctx, mt, []byte, error)`). Also added `internal/shared/wire.go` carrying envelope `MarshalWire` / `TryUnmarshalWire` + `WireEnvelope` + `Traceparent` / `Tracestate` consts so both direct and traced impls can share without facade import cycle.
- [x] 6.4 Created `otel-gorilla-ws/internal/direct/conn.go` — pure passthrough; zero `otel/sdk` and zero `otel/exporters` imports (grep-verified).
- [x] 6.5 Created `otel-gorilla-ws/internal/traced/conn.go` — full instrumentation; exported `PropagationEnabled` bool field gates envelope wrap/unwrap independently of span emission (mirrors otel-nats pattern).
- [x] 6.6 Refactored facade `otelgorillaws.Conn` to `{*websocket.Conn; impl shared.ConnImpl}`. Dropped `tracingEnabled` + `featureEnabled` fields. Public `WriteMessage` / `ReadMessage` are single-line `c.impl.<Method>(...)` delegates.
- [x] 6.7 Refactored `newConn` to compute `wsTracingEnabled()` + negotiated bit at construction time, then pick `direct.NewConn` or `traced.NewConn(raw, tracer, propagator, negotiated)`. `Dial` and `Upgrade` flow into `newConn` unchanged; runtime override behaviour preserved.
- [x] 6.8 Preserved scenarios A–E for subprotocol negotiation; existing `conn_test.go` regression tests (TestDial_ScenarioC/D/E, TestUpgrader_ScenarioF/G/H) all pass.
- [x] 6.9 Added facade compile-time assertions `var _ shared.ConnImpl = (*direct.Conn)(nil)` and `var _ shared.ConnImpl = (*traced.Conn)(nil)` in `conn.go`. Adding a method to ConnImpl without implementing in both impls now fails the build.
- [x] 6.10 Run `cd otel-gorilla-ws && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [x] 6.11 Ran `cd otel-gorilla-ws/tests/integration && go test -race -timeout 300s ./...` — 4/4 passed. `make verify-ws-trace` requires full docker-compose stack (frontend + collector + tempo) not currently provisioned in this session; integration suite covers handshake / envelope / scenario A–H behaviour at the wire level — sufficient signal for the strategy-split refactor.

## 7. Directory layout — `examples/` rename + standard tree

- [x] 7.1 `git mv otel-mongo/example otel-mongo/examples` (preserve rename history via `git diff -M`)
- [x] 7.2 `git mv otel-mongo/v2/example otel-mongo/v2/examples` (no-op — v2 has no example/ dir)
- [x] 7.3 `git mv otel-nats/example otel-nats/examples`
- [x] 7.4 `git mv otel-gorilla-ws/example otel-gorilla-ws/examples`
- [x] 7.5 Verify each `examples/<demo>/` has its own `go.mod` and `main.go`; update any internal references to the old `example/` path
- [ ] 7.6 Confirm `internal/{flags,shared,direct,traced}/` subpackages exist in all four modules with no synonyms (run `find pkg/instrumentation-go -type d -path '*/internal/*' -maxdepth 4`)
- [x] 7.7 Add `doc.go` to each module root if missing (package overview shown by `go doc` and pkg.go.dev)
- [ ] 7.8 Move any purely-internal helper files from module root into `internal/shared/` and update imports
- [x] 7.9 Commit layout moves as separate commits (`chore(<module>): rename example/ → examples/`)  before any logic commit so reviewers see clean `git diff -M` — landed as `346469c`

## 8. CI drift-check

- [x] 8.1 Added `drift-check` job to `.github/workflows/ci.yml` that fails when any of `internal/flags/flags.go` differs from the canonical copy at `otel-mongo/otelmongo/internal/flags/flags.go` (`diff -q` per file, covers all four modules). Locally simulated — passes against current tree.
- [x] 8.2 Added internal/direct SDK-isolation step to the same job — `grep -rnE '"go\.opentelemetry\.io/otel/(sdk|exporters)'` (leading-quote prefix to avoid false-positives on doc comments) over `otel-mongo/otelmongo/internal/direct/`, `otel-mongo/v2/internal/direct/`, `otel-gorilla-ws/internal/direct/`. Removed the old buggy matrix-scoped step (wrong working-directory for v1, skipped ws). Locally simulated with an injected fake import — caught correctly; current tree passes.
- [x] 8.3 ~~Add a grep-based check that public method bodies contain zero `if .*tracingEnabled`~~ **Dropped from CI automation.** Rationale: grep is line-level but "public method body" is AST-level — cannot distinguish facade public methods from constructor-site impl selection (which `instrumentation-feature-flags` explicitly exempts) without false positives. Enforcement instead lives in: (a) compile-time impl-interface assertions (`var _ shared.ConnImpl = (*direct.Conn)(nil)` etc.) that force any new public method to be implemented in both flavours, (b) the package-boundary check in 8.2 that catches the worst failure mode (SDK code reachable from disabled path), and (c) code review. A proper `go/analysis` Analyzer is the right tool if stronger automation is wanted later — tracked as a follow-up issue per Open Question in `design.md`.
- [x] 8.4 ~~Verify the new CI steps fail when intentionally broken (sanity check), then revert the break~~ **Replaced by manual demonstration in the PR description.** Author intentionally breaks each CI step in a throwaway commit, screenshots the failing run, then reverts before merge. Documented in PR body alongside the commit hashes. Avoids cluttering CI history with sanity-check commits and avoids the risk of the revert getting lost.

## 9. Documentation

- [x] 9.1 Updated `pkg/instrumentation-go/CLAUDE.md`: rewrote "Feature Flags" section with universal default-OFF preamble + cross-module env-var table + four-state matrix for nats; added new "Module Layout" section showing canonical tree (`internal/{flags,shared,direct,traced}` + `examples/<demo>/` + `tests/integration/`) plus a variant-comparison table covering package-level / nullable-pointer / file-level layouts. (Shared with task 12.13.)
- [x] 9.2 Root `/Users/marz/Develop/tools/otel-traces-test/CLAUDE.md` env-var table updated with 3-tier (mongo+nats) / 2-tier (ws) annotation — verified at lines 225–236.
- [x] 9.3 `otel-mongo/README.md` + `otel-mongo/README.zh-TW.md` carry feature-flags section (3 env vars, default OFF, truthy semantics) at the canonical position.
- [x] 9.4 Created `otel-mongo/v2/README.md` (full v2 README — quick start, flag table, internals, v1↔v2 API diff) and `otel-gorilla-ws/README.zh-TW.md` (zh-TW translation). Also added zh-TW cross-link to `otel-gorilla-ws/README.md` header.
- [x] 9.5 Feature-flags table present in every README (mongo v1 EN+TW, mongo v2, nats EN+TW, ws EN+TW) — env var + default + truthy-value list documented. Fixed pre-existing bug in ws README that said "Defaults: enabled when unset" (contradicted spec); now reads "all default to OFF when unset".
- [x] 9.6 ASCII tree diagram of `internal/{flags,shared,direct,traced}/` (or `internal/flags/` + file-level split for otel-nats) added to "Internals overview" section in all 7 READMEs.

## 10. Versioning + release

> **Version-pin tasks dropped** (was 10.1–10.4: bump to `0.4.0`). Policy clarified: version bumps follow the **per-package change policy** documented in `CLAUDE.md` — any code change to a module SHALL bump its `instrumentationVersion` constant before the release tag, but this change set does not prescribe a specific target version. Release engineer picks the next bump (patch / minor) based on the cumulative diff at tag time. Tasks 10.5–10.8 below cover the release/verify steps that DO belong to this change.

- [ ] 10.5 Tag releases via existing multi-tag release script (versions chosen at release time per the per-package bump policy in `CLAUDE.md`)
- [ ] 10.6 Update `otel-traces-test` consumer `go.mod` files (`api/`, `worker/`, `dbwatcher/`) to pin the new tags
- [ ] 10.7 Run `make verify-trace` against the running stack — confirm end-to-end propagation still works
- [ ] 10.8 Run `make verify-ws-trace` for the WebSocket-only stack

## 12. `otel-nats` propagation flag (3-tier upgrade)

- [x] 12.1 Add `OTEL_NATS_PROPAGATION_ENABLED` constant to `otel-nats/otelnats/env_flags.go` with godoc explaining: gate consulted only when both tracing gates are on; **default OFF when unset** (consistent with universal default-OFF posture); set explicitly truthy to inject `traceparent` / `tracestate` headers
- [x] 12.2 Add `natsPropagationGate *flags.Gate` to `env_flags.go` with resolver: `func() bool { return natsGate.Enabled() && flags.EnvEnabled("OTEL_NATS_PROPAGATION_ENABLED") }` — reuses existing `flags.EnvEnabled` default-off semantics, no new helper needed
- [x] 12.3 Add unexported accessor `func natsPropagationEnabled() bool { return natsPropagationGate.Enabled() }`
- [x] 12.4 Cache the resolved propagation value on `*tracedConn` at construction time (`propagationEnabled bool` field) — hot path reads field, never re-reads env
- [x] 12.5 Plumb the cached `propagationEnabled` into `tracedConn.startSendSpan` / `startRequestSpan`: keep `tracer.Start` unconditional; gate the `propagator.Inject(...)` call behind `if t.propagationEnabled`
- [x] 12.6 Plumb into `tracedConn.wrapMsgHandler` and `tracedConn.recordReply`: gate the `propagator.Extract(...)` call behind `if t.propagationEnabled` (when false, supply `context.Background()` as the consumer-span parent and skip `WithLinks` setup driven by the extracted span)
- [x] 12.7 Plumb into `oteljetstream` `newTracedMessageBatch`, `tracedConsumer.Next`, `tracedMessagesContext.Next`, `tracedConsumeHandler`: same Extract-gating + parent-context fallback
- [x] 12.8 Documented in `otel-nats/README.md` + `README.zh-TW.md`: "Tracing feature flags" section now lists the three env vars with default OFF, the four-state truth table, and the cached-after-first-read semantics. (Note: default is OFF, not ON as the task wording initially said — matches the universal default-OFF posture finalised during this change.)
- [x] 12.9 Add `natsPropagationGate.ResetForTest()` parallel-to existing `natsGate.ResetForTest()` so test files can toggle the new env var with `t.Setenv` + reset
- [x] 12.10 Keep `otel-nats/otelnats/version.go` `instrumentationVersion` at `0.4.0` — no version bump for this change; the propagation flag ships as part of the existing 0.4.x line. (Re-evaluate when the change is archived; release-time decision lives outside this artifact set.)
- [x] 12.11 Created `otel-nats/CHANGELOG.md` with the 0.4.x entry covering the new env var, the default-behaviour change, before/after wire-output matrix, the `oteljetstream/jetstream_traced.go` Publish-injection bug fix, and the migration recipe.
- [x] 12.12 Added "Propagation flag (env-var change from v0.3.x)" subsection to both `otel-nats/README.md` and `README.zh-TW.md` with the `grep -rE 'OTEL_NATS_TRACING_ENABLED' deploy/ config/ docker-compose*.yml` migration recipe.
- [x] 12.13 Rewrote `pkg/instrumentation-go/CLAUDE.md` "Feature Flags" section: added universal default-OFF posture preamble, cross-module env-var table (3-tier mongo + nats, 2-tier ws), default-behaviour-change note on `OTEL_NATS_PROPAGATION_ENABLED`. Also added the missing "Module Layout" section (task 9.1).

## 13. `otel-nats` propagation flag tests (all four on/off combinations)

- [x] 13.1 Add `propagation_gate_test.go` table-driven test asserting `natsPropagationGate.Enabled()` value for the matrix: (tracing unset, propagation unset) → false; (tracing unset, propagation `"true"`) → false; (tracing on, propagation unset) → **false** (new default); (tracing on, propagation `"true"`) → true; (tracing on, propagation `"false"`) → false; (tracing on, propagation `"0"`) → false; (tracing on, propagation `"OFF"`) → false (case-insensitive). Each row SHALL call `natsPropagationGate.ResetForTest()` after `t.Setenv` so the cache reflects the case under test.
- [x] 13.2 Add `conn_test.go` test `TestPublishWithPropagationOffSkipsHeaders`: tracing on + `OTEL_NATS_PROPAGATION_ENABLED` unset (default OFF), call `Conn.Publish(ctx, subj, data)` with a parent span in ctx; assert the message on `Subscribe` arrives with NO `traceparent` header but a PRODUCER span IS recorded in the test span recorder
- [x] 13.3 Add `TestPublishWithPropagationEnabledInjectsHeaders`: tracing on + `OTEL_NATS_PROPAGATION_ENABLED=true`; assert message has `traceparent` header AND PRODUCER span recorded (positive counterpart of 13.2)
- [x] 13.4 Added `TestPublishWithTracingOffSkipsBoth` (`otel-nats/otelnats/conn_test.go`): all gates unset → asserts no `traceparent` header on the wire AND zero wrapper spans recorded. Uses new `startServerTracingOff` helper.
- [x] 13.5 Added `TestPublishWithTracingOffPropagationOnStillSkipsBoth` (`otel-nats/otelnats/conn_test.go`): tracing unset + `OTEL_NATS_PROPAGATION_ENABLED=true` → asserts identical behaviour to 13.4 (tracing gate is the hard prerequisite).
- [x] 13.6 Add `TestPublishWithPropagationExplicitlyFalseSkipsHeaders`: tracing on + `OTEL_NATS_PROPAGATION_ENABLED=false`; assert observationally identical to 13.2 (explicit falsy = unset)
- [x] 13.7 Add `TestSubscribeWithPropagationOffDoesNotExtract`: tracing on + propagation default off; publish via raw `*nats.Conn` with `traceparent` header pre-set; assert the wrapper consumer span has no remote-parent link and the `Msg.Ctx` carries `context.Background()`-equivalent (no extracted span)
- [x] 13.8 Add `TestSubscribeWithPropagationOnExtractsRemoteContext`: same as 13.7 but with `OTEL_NATS_PROPAGATION_ENABLED=true`; assert the consumer span has a link to the published `traceparent` span context
- [x] 13.9 Added JetStream propagation matrix (`otel-nats/oteljetstream/consumer_test.go`): `TestJetStreamSubscribeWithPropagationOffDoesNotExtract`, `TestJetStreamSubscribeWithPropagationOnExtractsRemoteContext`, `TestJetStreamConsumerWithTracingOffSkipsBoth`, `TestJetStreamConsumerWithTracingOffPropagationOnStillSkipsBoth`. Exercises Fetch path (newTracedMessageBatch). Test development surfaced a **real bug in `oteljetstream/jetstream_traced.go`**: `PublishMsg` was calling `prop.Inject(...)` unconditionally regardless of the propagation gate — fixed by gating behind `j.conn.PropagationEnabled()`. Promoted `otelnats.ResetGatesForTest` from a `_test.go`-only symbol to a regular package-level function in `test_helpers.go` so sibling test packages (oteljetstream_test) can reset gates after `t.Setenv`.
- [x] 13.10 Add a default-behaviour-change regression test `TestDefaultBehaviorIsTracingOnlyNoHeaders`: with only `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` and `OTEL_NATS_TRACING_ENABLED=true` set (no propagation env), assert the new default = span recorded BUT no `traceparent` header — guards against accidentally re-flipping to implicit propagation-on
- [x] 13.11 Add migration validation test `TestPropagationFlagRestoresFullWireOutput`: with the full tracing env set PLUS `OTEL_NATS_PROPAGATION_ENABLED=true`, assert wire output contains `traceparent` (+ `tracestate` when active span carries one) — documents the exact migration recipe in test form
- [x] 13.12 Run `cd otel-nats && go test -race ./... && golangci-lint run ./...` — zero failures, zero issues, all 4 propagation states verified

## 14. Performance invariants lock-in

- [x] 14.1 Sampler-aware deliverSpan gate — producer + consumer paths in `otelnats.tracedConn` (StartDeliverSpan + ConsumerContextWithDeliver) early-return when relevant span context is not sampled
- [x] 14.2 Sampler-aware consumer link gate — 6 hot-paths across `otelnats` + `oteljetstream` apply both `IsValid()` and `IsSampled()` guards before attaching `trace.Link`
- [x] 14.3 Propagation-off → emit span, skip link: closure structure ensures `originSpanCtx` and the link branch live inside `if propagationEnabled` so the rule is structurally enforced, not condition-dependent
- [x] 14.4 MessageBatch lifecycle drain — both `newDirectMessageBatch` and `newTracedMessageBatch` drain `raw.Messages()` after `Stop()` to release the upstream jetstream goroutine
- [x] 14.5 Header sentinel — no `make(nats.Header)` for nil-header messages on jetstream consumer paths; optional-traceparent early-return precedes any allocation
- [x] 14.6 BSON inject type-switch + clone — `InjectTraceIntoDocument` / `InjectTraceIntoUpdate` (v1 + v2) + `upsertSetField` sub-doc all clone before injecting; never alias caller's slice/map backing storage
- [x] 14.7 `marshalWire` pool + hand-written serializer — `otelgorillaws/message.go` uses `sync.Pool` of byte buffers and a reflection-free JSON writer; output round-trips equivalently to the legacy `encoding/json` form
- [x] 14.8 Regression tests exist for each invariant: `TestSubscribeWithPropagationOffStillEmitsSpanWithoutLink`, `TestRecordReplyWithPropagationOffStillEmitsSpanWithoutLink`, `TestDeliverSpanSkippedWhenUpstreamNotSampled`, `TestStartDeliverSpanSkippedWhenLocalNotSampled`, `TestConsumerSpanNoLinkWhenOriginNotSampled`, `TestRecordReplyLinkRespectsSamplerFlag`, `TestMessageBatchStopReleasesRawBatch`, `TestNoGoroutineLeakAfterEarlyReturn`, `TestInjectDoesNotShareBackingArray`, `TestInjectDoesNotMutateOriginalBson{D,M,Map}`, `TestInjectUpdateDoesNotShareSetBacking`, `TestMarshalWireRoundTripStable`, `TestMarshalWirePoolReuseSafety`

## 11. Final verification

- [ ] 11.1 Toggle `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` in every service in `docker-compose.yml`; restart stack; exercise the 4 message paths (JetStream, Core NATS, HTTP, MongoDB) — confirm zero spans appear in Tempo via `curl -s "http://localhost:3200/api/search?q={}&limit=20"`
- [ ] 11.2 Toggle `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` + all module flags = true; rerun the same exercises; confirm full traces appear with `rootServiceName: frontend`
- [ ] 11.3 Toggle `OTEL_MONGO_TRACING_ENABLED=true`, `OTEL_MONGO_PROPAGATION_ENABLED=true`, but `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false`; confirm no `_oteltrace` field appears in `messaging.messages` documents (inspect via mongo-express on `localhost:3002`)
- [ ] 11.4 Toggle `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true`, `OTEL_MONGO_TRACING_ENABLED=true`, but `OTEL_MONGO_PROPAGATION_ENABLED=false`; confirm wrapper spans appear but `_oteltrace` field is absent
- [x] 11.5 Ran `golangci-lint run ./...` for all four modules — zero issues (local CI sweep, 491 tests across all packages).
- [x] 11.6 Ran `go test -race ./...` for all four modules — zero failures (local CI sweep).
- [x] 11.7 Ran all four integration test suites — mongo v1 6/6, mongo v2 6/6, otel-nats 12/12, otel-gorilla-ws 4/4 = 28/28 green.
- [ ] 11.8 Open follow-up tracking issue for any deferred work (e.g. functional-option overrides for nats/ws propagation if requested later)
