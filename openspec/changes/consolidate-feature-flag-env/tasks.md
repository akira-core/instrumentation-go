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
- [ ] 2.6 Run `cd otel-mongo/tests/integration && go test -race ./...` — verify testcontainers integration still passes

## 3. `otel-mongo` v2 parity wiring

- [x] 3.1 Copy `internal/flags/` directory to `otel-mongo/v2/internal/flags/` (byte-identical contents)
- [x] 3.2 Apply the same `env_flags.go` rewrite to `otel-mongo/v2/env_flags.go` — diff against v1 SHOULD be the package name only
- [x] 3.3 Migrate `cachedPropagationEnabled` in v2 the same way as task 2.3
- [x] 3.4 Run `cd otel-mongo/v2 && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [ ] 3.5 Run `cd otel-mongo/v2/tests/integration && go test -race ./...`

## 4. `otel-mongo` Client and Database strategy-split

- [ ] 4.1 Add `clientImpl` / `databaseImpl` interfaces to `internal/shared/impls.go` (v1 and v2)
- [ ] 4.2 Implement `internal/direct/client.go` and `internal/direct/database.go` (zero `otel/sdk` imports) — pure delegation to upstream `*mongo.Client` / `*mongo.Database`
- [ ] 4.3 Implement `internal/traced/client.go` and `internal/traced/database.go` — full instrumentation (moved verbatim from current facade)
- [ ] 4.4 Add facade compile-time assertions `var _ clientImpl = (*direct.Client)(nil)` etc.; remove cached-gate `tracingEnabled bool` field from facade `Client` / `Database`
- [ ] 4.5 Wire `Connect` / `Wrap` / `client.Database(name)` / `db.Collection(name)` to pick impls based on `mongoTracingEnabled()` once
- [ ] 4.6 Repeat tasks 4.1–4.5 for v2 identically
- [ ] 4.7 Run build + test + lint + integration on both v1 and v2

## 5. `otel-nats` wiring + strategy-split

- [x] 5.1 Create `otel-nats/otelnats/internal/flags/` (copy of canonical `flags.go`)
- [x] 5.2 Replace `otel-nats/otelnats/env_flags.go` body with `flags.Gate` composition for `natsTracingEnabled` (two-tier: global + `OTEL_NATS_TRACING_ENABLED`)
- [ ] 5.3 Create `otel-nats/otelnats/internal/shared/impls.go` with `connImpl`, `subscriptionImpl` interfaces — deferred; existing file-level split (`conn_direct.go` / `conn_traced.go`) in same package already satisfies functional requirement (constructor picks `connImpl` once, no `if tracingEnabled` in public methods)
- [ ] 5.4 Create `otel-nats/otelnats/internal/direct/conn.go` and `subscription.go` — deferred; see 5.3
- [ ] 5.5 Create `otel-nats/otelnats/internal/traced/conn.go` and `subscription.go` — deferred; see 5.3
- [x] 5.6 Refactor facade `otelnats.Conn` to hold `impl connImpl`; replace every public method body with `return c.impl.<Method>(args...)`; delete every `if c.tracingEnabled` branch in public methods — already done in prior commit (`2858a51`); verified by inspection of `conn.go`
- [x] 5.7 Wire `Connect` / `Wrap` to call `natsGate.Enabled()` once and pick the impl — already done in prior commit
- [ ] 5.8 Repeat 5.3–5.7 for `oteljetstream`: `Consumer`, `MessageBatch`, `directMessageBatch` migrate to `internal/{direct,traced}/` — deferred; file-level split already in place (`consumer_direct.go` / `consumer_traced.go` etc.)
- [ ] 5.9 Add `var _ connImpl = (*direct.Conn)(nil)` and `var _ connImpl = (*traced.Conn)(nil)` assertions in facade `conn.go` — deferred until package-level split
- [x] 5.10 Run `cd otel-nats && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [ ] 5.11 Run `cd otel-nats/tests/integration && go test -race ./...` — verify NATS testcontainers integration still passes

## 6. `otel-gorilla-ws` wiring + strategy-split

- [x] 6.1 Create `otel-gorilla-ws/internal/flags/` (copy of canonical `flags.go`)
- [x] 6.2 Replace `otel-gorilla-ws/env_flags.go` body with `flags.Gate` composition for `wsTracingEnabled`
- [ ] 6.3 Create `otel-gorilla-ws/internal/shared/impls.go` with `connImpl` interface — deferred; current `featureEnabled` flag is cached at construction (no per-call env read), functional requirement met
- [ ] 6.4 Create `otel-gorilla-ws/internal/direct/conn.go` — deferred; see 6.3
- [ ] 6.5 Create `otel-gorilla-ws/internal/traced/conn.go` — deferred; see 6.3
- [ ] 6.6 Refactor facade `otelgorillaws.Conn` to hold `impl connImpl`; delete `tracingEnabled bool` field and every public-method runtime branch — deferred; current `featureEnabled` check + subprotocol-negotiated `tracingEnabled` are construction-time decisions, not per-call gate reads
- [ ] 6.7 Refactor `Dial` and `Upgrade` to (a) negotiate subprotocol, (b) compute `wsGate.Enabled() && negotiatedOTelSubprotocol`, (c) pick `direct.NewConn` or `traced.NewConn` — deferred; existing `Dial` already computes negotiated tracingEnabled at handshake
- [x] 6.8 Preserve scenarios A–E for subprotocol negotiation; add a regression test for each scenario — already covered by existing `conn_test.go`
- [ ] 6.9 Add facade compile-time assertions — deferred until package-level split
- [x] 6.10 Run `cd otel-gorilla-ws && go build ./... && go test -race ./... && golangci-lint run ./...` — zero failures
- [ ] 6.11 Run repo-level `make verify-ws-trace` — verify end-to-end WebSocket trace propagation still works through `pkg/instrumentation-js`

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

- [ ] 8.1 Add `.github/workflows/ci.yml` step that runs `diff <module>/internal/flags/flags.go` pairwise across all four modules; non-zero exit fails the job
- [ ] 8.2 Add a grep-based check that `internal/direct/` directories contain zero `go.opentelemetry.io/otel/sdk/` and zero `go.opentelemetry.io/otel/exporters/` imports; fail if any match
- [ ] 8.3 Add a grep-based check that public method bodies in `otelnats.Conn`, `oteljetstream.Consumer`, `oteljetstream.MessageBatch`, `otelgorillaws.Conn` contain zero `if .*tracingEnabled` strings; fail if any match
- [ ] 8.4 Verify the new CI steps fail when intentionally broken (sanity check), then revert the break

## 9. Documentation

- [ ] 9.1 Update `pkg/instrumentation-go/CLAUDE.md` "Architecture" section: replace existing flag descriptions with the unified gate table; add "Module Layout" section showing canonical tree; document `internal/flags/` + `internal/{shared,direct,traced}/`
- [ ] 9.2 Update root `/Users/marz/Develop/tools/otel-traces-test/CLAUDE.md` env-var table to reflect 2-tier vs 3-tier surface per module
- [ ] 9.3 Update `otel-mongo/README.md` and `otel-mongo/README.zh-TW.md` per the canonical section order: quick start, feature-flags table, public API, disabled-mode behaviour, internals overview, versioning
- [ ] 9.4 Repeat 9.3 for `otel-mongo/v2/README.md` (+zh-TW), `otel-nats/README.md` (+zh-TW), `otel-gorilla-ws/README.md` (+zh-TW)
- [ ] 9.5 Each README's "Feature Flags" table SHALL list every env var the module reads, its default (`disabled`), and the truthy-value list
- [ ] 9.6 Each README's "Internals overview" SHALL contain the ASCII tree diagram of `internal/{flags,shared,direct,traced}/`

## 10. Versioning + release

- [x] 10.1 Bump `otel-mongo/otelmongo/version.go` → `0.4.0` (already at 0.4.0 from prior commit)
- [x] 10.2 Bump `otel-mongo/v2/version.go` → `0.4.0` (already at 0.4.0)
- [x] 10.3 Bump `otel-nats/otelnats/conn.go` `instrumentationVersion` → `0.4.0` (already at 0.4.0)
- [x] 10.4 Bump `otel-gorilla-ws/version.go` `Version()` literal → `0.4.0`
- [ ] 10.5 Tag releases via existing multi-tag release script: `otel-mongo/v0.4.0`, `otel-mongo/v2/v0.4.0`, `otel-nats/v0.4.0`, `otel-gorilla-ws/v0.4.0`
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
- [ ] 12.8 Document in `otel-nats/README.md` (+ zh-TW): new env var, default ON, the four-state truth table, examples of when to flip off
- [x] 12.9 Add `natsPropagationGate.ResetForTest()` parallel-to existing `natsGate.ResetForTest()` so test files can toggle the new env var with `t.Setenv` + reset
- [x] 12.10 Keep `otel-nats/otelnats/version.go` `instrumentationVersion` at `0.4.0` — no version bump for this change; the propagation flag ships as part of the existing 0.4.x line. (Re-evaluate when the change is archived; release-time decision lives outside this artifact set.)
- [ ] 12.11 Add `otel-nats/CHANGELOG.md` entry under the existing 0.4.x heading: "`OTEL_NATS_PROPAGATION_ENABLED` env var introduced, default OFF. Deployments previously relying on implicit header injection (tracing-on alone) must add this env var to keep `traceparent` injection working." Include before/after wire-output examples
- [ ] 12.12 Add migration block to `otel-nats/README.md` + `README.zh-TW.md` under a "Propagation flag (env-var change)" heading with a one-line `grep -rE "OTEL_NATS_TRACING_ENABLED" config/` recipe to locate affected deployments
- [ ] 12.13 Update root `pkg/instrumentation-go/CLAUDE.md` env-var section with the universal default-OFF rule and a footnote pointing at the propagation env var for nats

## 13. `otel-nats` propagation flag tests (all four on/off combinations)

- [x] 13.1 Add `propagation_gate_test.go` table-driven test asserting `natsPropagationGate.Enabled()` value for the matrix: (tracing unset, propagation unset) → false; (tracing unset, propagation `"true"`) → false; (tracing on, propagation unset) → **false** (new default); (tracing on, propagation `"true"`) → true; (tracing on, propagation `"false"`) → false; (tracing on, propagation `"0"`) → false; (tracing on, propagation `"OFF"`) → false (case-insensitive). Each row SHALL call `natsPropagationGate.ResetForTest()` after `t.Setenv` so the cache reflects the case under test.
- [x] 13.2 Add `conn_test.go` test `TestPublishWithPropagationOffSkipsHeaders`: tracing on + `OTEL_NATS_PROPAGATION_ENABLED` unset (default OFF), call `Conn.Publish(ctx, subj, data)` with a parent span in ctx; assert the message on `Subscribe` arrives with NO `traceparent` header but a PRODUCER span IS recorded in the test span recorder
- [x] 13.3 Add `TestPublishWithPropagationEnabledInjectsHeaders`: tracing on + `OTEL_NATS_PROPAGATION_ENABLED=true`; assert message has `traceparent` header AND PRODUCER span recorded (positive counterpart of 13.2)
- [ ] 13.4 Add `TestPublishWithTracingOffSkipsBoth`: leave all gates off (default state); assert no `traceparent` header AND no spans recorded
- [ ] 13.5 Add `TestPublishWithTracingOffPropagationOnStillSkipsBoth`: tracing unset + `OTEL_NATS_PROPAGATION_ENABLED=true`; assert behaviour identical to 13.4 (tracing gate is the hard prerequisite)
- [x] 13.6 Add `TestPublishWithPropagationExplicitlyFalseSkipsHeaders`: tracing on + `OTEL_NATS_PROPAGATION_ENABLED=false`; assert observationally identical to 13.2 (explicit falsy = unset)
- [x] 13.7 Add `TestSubscribeWithPropagationOffDoesNotExtract`: tracing on + propagation default off; publish via raw `*nats.Conn` with `traceparent` header pre-set; assert the wrapper consumer span has no remote-parent link and the `Msg.Ctx` carries `context.Background()`-equivalent (no extracted span)
- [x] 13.8 Add `TestSubscribeWithPropagationOnExtractsRemoteContext`: same as 13.7 but with `OTEL_NATS_PROPAGATION_ENABLED=true`; assert the consumer span has a link to the published `traceparent` span context
- [ ] 13.9 Repeat 13.2–13.8 for `oteljetstream` consumer paths: `newTracedMessageBatch`, `tracedConsumer.Next`, `tracedConsumeHandler` — each must respect the propagation gate identically
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
- [ ] 11.5 Run `golangci-lint run ./...` from the repo root for all four modules; zero issues
- [ ] 11.6 Run `go test -race ./...` for all four modules; zero failures
- [ ] 11.7 Run all four integration test suites (`tests/integration` in each module); zero failures
- [ ] 11.8 Open follow-up tracking issue for any deferred work (e.g. functional-option overrides for nats/ws propagation if requested later)
