## 1. otel-mongo v1 — shared address-capture helpers

- [ ] 1.1 In `otelmongo/internal/shared/`, add a `splitHostPort(s string) (addr string, port int)` helper implementing the existing IPv6-aware host/port extraction logic (factor out of `client.go`'s `parseServerFromURI`, which now calls this helper too).
- [ ] 1.2 Add `internal/shared/monitor.go`: an unexported context key type + `addrCapture` holder struct (`addr string`, `port int`), a `withAddrCapture(ctx) (context.Context, *addrCapture)` that stashes a fresh holder, and `parseConnectionID(connID string) (addr string, port int)` that splits on the first `[` and calls `splitHostPort`.
- [ ] 1.3 In `internal/shared/monitor.go`, add `NewCommandMonitor(existing *event.CommandMonitor) *event.CommandMonitor` that returns a monitor whose `Started` reads the `addrCapture` from `ctx` (via the context key) and writes `parseConnectionID(ev.ConnectionID)` into it, then calls `existing.Started` if non-nil; `Succeeded`/`Failed` are pure pass-throughs to `existing.Succeeded`/`existing.Failed` when non-nil, no-ops otherwise.
- [ ] 1.4 Unit tests for `parseConnectionID` (formats: `host:27017[-1]`, `host:27017[-42]`, IPv6 `[::1]:27017[-1]`, malformed input → empty/zero fallback) and for `NewCommandMonitor` chaining (asserts a caller-supplied `Started`/`Succeeded`/`Failed` still fires, asserts nil-callback caller monitor doesn't panic).
- [ ] 1.5 In `internal/shared/semconv.go`, split the `server.address`/`server.port` tail of `DBAttributes` (the `if serverAddr != "" { ... }` block) into `ServerAttributes(serverAddr string, serverPort int) []attribute.KeyValue` (returns `nil` on empty address; omits `server.port` when default 27017 — identical rules). Have `DBAttributes` call `ServerAttributes` internally so the start-time and post-call paths can't drift. `DBAttributes`' signature stays unchanged.

## 2. otel-mongo v1 — client.go wiring

- [ ] 2.1 In `client.go` `ConnectWithOptions`, tracing-enabled branch only: merge inputs with `merged := options.MergeClientOptions(opts...)`, then `merged.SetMonitor(shared.NewCommandMonitor(merged.Monitor))`, and pass the single `merged` to `mongo.Connect` (v1: `mongo.Connect(ctx, merged)`). Read the effective caller monitor from `merged.Monitor` (exported field) — no `opts`-walking / append hack. Unify the fallback address with v2 by deriving it from `parseServerFromURI(merged.GetURI())` and retiring v1's `lastNonEmptyURI(opts)`. (v2 already merges via `MergeClientOptions` — see 5.2.)
- [ ] 2.2 Confirm the disabled-tracing branch of `ConnectWithOptions` is untouched (no monitor registration) — add/keep a test asserting `mongo.Connect`'s effective options carry no injected monitor when tracing is disabled.
- [ ] 2.3 `go build ./...` in `otel-mongo/` — passes.

## 3. otel-mongo v1 — collection.go call sites

- [ ] 3.1 For each of the 14 `shared.DBAttributes(...)` call sites in `internal/traced/collection.go`, keep the existing `tracer.Start(..., trace.WithAttributes(shared.DBAttributes(...t.ServerAddr, t.ServerPort)...))` line **unchanged**. Add, additively: `ctx, capture := shared.WithAddrCapture(ctx)` immediately after `tracer.Start` (its returned `ctx` must be the one passed to the raw driver call), and after the raw call returns, `if addr, port := capture.Resolve(t.ServerAddr, t.ServerPort); addr != "" { span.SetAttributes(shared.ServerAttributes(addr, port)...) }` before `span.End()` runs. This overwrites `server.address`/`server.port` with the per-command value while leaving every other `db.*` attribute attached at start (per design Decision 2). Cover the two `DBAttributes` lines inside `Watch` and the shared `runUpdate`/`runDelete` helpers — count sites through those helpers so none is missed or double-counted.
- [ ] 3.2 Verify error paths (`RecordSpanError`) and propagation/inject still run unchanged — the start-time `trace.WithAttributes(...)` line is untouched, so `db.response.status_code`/`error.type` and `db.system.name`/`db.operation.name`/`db.namespace` attribution are unaffected; only `server.*` is overwritten post-call.
- [ ] 3.3 `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-mongo/` — all pass; CI "direct/ has no OTel SDK imports" grep still clean (no `internal/direct` file touched).

## 4. otel-mongo v1 — integration-style test of the capture path

- [ ] 4.1 Unit-test the capture path directly (no live server, deterministic): construct the monitor via `shared.NewCommandMonitor(nil)`, obtain `ctx, capture := shared.WithAddrCapture(ctx)`, call `monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "realhost:27018[-1]"})`, and assert `capture.Resolve(...)` yields `realhost`, `27018`. Then, with a fake `Coll` whose call fires the registered monitor with a `ConnectionID` differing from `t.ServerAddr`, assert the traced method's span carries `server.address` = the captured value, not `t.ServerAddr`. Do **not** attempt to make the URI-parsed and actual addresses differ via `mongodb+srv://` aliasing or multi-host strings here — that is not reliably reproducible in a single-node unit/testcontainers setup; reserve it for the manual replica-set spot-check (7.3).
- [ ] 4.2 Add a test asserting the caller-supplied `SetMonitor` chaining requirement from the spec (`Requirement: Caller-supplied CommandMonitor is chained, not replaced`) — both callbacks fire, nil-subset caller monitor doesn't panic.
- [ ] 4.3 Add a test asserting the fallback requirement (`Requirement: Fallback to static URI-derived address`) — when the capture holder never gets written, `t.ServerAddr`/`t.ServerPort` are used, matching pre-change behavior exactly.
- [ ] 4.4 Add a test for the default-port omission under capture: a captured `ConnectionID` of `host:27017[-1]` emits `server.address` only (no `server.port`), while `host:27018[-1]` emits both — matching `ServerAttributes`' 27017 rule.

## 5. otel-mongo v2 — mirror v1 (parity)

- [ ] 5.1 Apply tasks 1.1–1.5 identically to `v2/internal/shared/` (using `go.mongodb.org/mongo-driver/v2/event`).
- [ ] 5.2 Apply tasks 2.1–2.3 identically to `v2/client.go` — v2's `ConnectWithOptions` **already** does `merged := options.MergeClientOptions(opts...)` before `mongo.Connect(merged)`, so 2.1 reduces to inserting `merged.SetMonitor(shared.NewCommandMonitor(merged.Monitor))` after that merge (no signature/flow change; v2 `mongo.Connect` takes no `ctx`).
- [ ] 5.3 Apply tasks 3.1–3.3 identically to `v2/internal/traced/collection.go` (16 call sites).
- [ ] 5.4 Apply tasks 4.1–4.4 identically to `v2/`.
- [ ] 5.5 `go build ./... && go test -v -race ./... && golangci-lint run ./...` in `otel-mongo/v2/` — all pass; direct/ grep clean.

## 6. Docs + version bump

- [ ] 6.1 Bump `instrumentationVersion` in `otel-mongo/otelmongo/version.go` and `otel-mongo/v2/version.go`.
- [ ] 6.2 Update `otel-mongo/README.md` + `README.zh-TW.md`: document that `server.address`/`server.port` now reflect the actual per-command connection (command-monitor-based), the URI-parse fallback, and the `SetMonitor` chaining behavior/guarantee for callers who pass their own command monitor.
- [ ] 6.3 Update root `CLAUDE.md` "Architecture" section if the strategy-split / cached-gate description needs a note about the new shared address-capture helper living in `internal/shared/`.

## 7. Final verification

- [ ] 7.1 Full sweep: `go build`, `go test -race`, `golangci-lint run` green in both `otel-mongo/otelmongo` and `otel-mongo/v2`.
- [ ] 7.2 Run `otel-mongo/tests/integration` and `otel-mongo/v2/tests/integration` (Docker/testcontainers) — confirm `server.address` on emitted spans matches the actual container-mapped host:port used by the test's `mongo.Client`.
- [ ] 7.3 Manual spot-check: connect against a real (or testcontainers-based) MongoDB replica set with a multi-host connection string where the first host is not the primary; confirm span `server.address` matches the primary, not the first-listed host.
