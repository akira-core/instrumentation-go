## Context

`otel-mongo`'s CLIENT spans carry `server.address`/`server.port` (`internal/shared/semconv.go` `DBAttributes`). Today those values come from `Client.serverAddr`/`serverPort`, set once in `client.go` `ConnectWithOptions` by statically parsing the connection URI (`parseServerFromURI`) and threaded down through `Database` → `Collection` → `internal/traced.Collection.ServerAddr/ServerPort`. This has three known gaps: only the first host of a multi-host replica-set URI is used, `mongodb+srv://` is never resolved past the SRV name itself, and the value never updates after failover — every span for the `Client`'s lifetime repeats the Connect-time guess.

The MongoDB Go driver (both v1 `go.mongodb.org/mongo-driver` and v2 `go.mongodb.org/mongo-driver/v2`) exposes `event.CommandMonitor` (set via `options.ClientOptions.SetMonitor`). Its `Started` callback fires synchronously, in the calling goroutine, before the wire command is sent, with a `CommandStartedEvent.ConnectionID` in the driver-internal format `fmt.Sprintf("%s[-%d]", addr, nextConnectionID())` (confirmed identical in both driver versions' `x/mongo/driver/topology/connection.go`). This is the address of the actual connection used for that specific command — the thing `server.address` is supposed to represent. The upstream `go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/mongo/otelmongo` package is built entirely on this same `CommandMonitor` + context-keyed correlation, so the ctx-holder approach here is proven, not novel — its `ConnectionID` handling is worth cross-referencing when implementing the parser.

Constraints from the existing codebase (CLAUDE.md):
- v1/v2 parity: identical logic in `otelmongo/` and `v2/`, including their separate `internal/{direct,traced,shared}/` trees.
- Disabled-mode invariant: `internal/direct` and the disabled-tracing branch of `ConnectWithOptions` must not gain any new OTel-adjacent machinery.
- Cached-gate pattern for `Client`/`Database`: tracing-on/off is decided once at construction, not per call.

## Goals / Non-Goals

**Goals:**
- CLIENT spans for Collection CRUD operations (`InsertOne`, `Find`, `UpdateOne`, `Aggregate`, `BulkWrite`, etc. — every existing `shared.DBAttributes(...)` call site in `internal/traced/collection.go`) carry the `server.address`/`server.port` of the connection that actually carried that command.
- Correctness for multi-host replica sets, `mongodb+srv://`, and post-failover topology changes.
- No silent loss of a caller-supplied `options.ClientOptions.SetMonitor(...)`.
- Zero behavior change when tracing is disabled.

**Non-Goals:**
- `Cursor`/`ChangeStream`/`SingleResult` spans — they don't call `DBAttributes` directly today (they link to the parent Collection span), so their attribute sourcing is untouched. **Boundary at `Collection.Watch`:** Watch's own change-stream-open `aggregate` CLIENT span *is* a `collection.go` `DBAttributes` site and receives the per-command address like any CRUD op (in scope); only the resulting `ChangeStream` reader's subsequent `getMore` spans are out of scope.
- Exposing the per-command address as a new public API (e.g. no new exported getter) — it only feeds the existing `server.address`/`server.port` span attributes.
- `otel-nats` / `otel-gorilla-ws` — unaffected, no comparable static-parse problem exists there (`otel-nats` already uses `nc.ConnectedAddr()` post-connection).
- Resolving DNS SRV records ourselves — we rely on the driver's own resolution surfacing through `ConnectionID`.

## Decisions

### 1. Capture mechanism: `event.CommandMonitor.Started`, not `event.ServerMonitor`
`ServerMonitor`'s `ServerDescriptionChanged` only fires on topology changes and reports the driver's view of "current primary/mongos", not which server served a specific command — still inaccurate for sharded clusters with multiple mongos or reads routed to secondaries. `CommandMonitor.Started` is fired per command with the exact connection used, matching OTel semconv's intent for `server.address` (the server that handled *this* request). Trade-off accepted: one extra callback invocation per command versus one per topology change; negligible relative to a network round trip.

### 2. Correlation: context-scoped mutable holder; emit server.* once post-call, keep static db.* at span start
Each traced Collection method already creates its span before calling the raw driver method and passes `ctx` through. Immediately after `tracer.Start(...)` we stash a pointer to a small holder (`*addrCapture{addr string, port int}`) into that `ctx` via `context.WithValue` (`shared.WithAddrCapture(ctx)`), then pass the holder-`ctx` to the raw `t.Coll.XxxOne(ctx, ...)` call. `CommandMonitor.Started` runs synchronously in the same goroutine with a ctx derived from ours, reads the holder key, and writes the parsed `ConnectionID` into it. When the raw call returns, the traced method reads the holder (`capture.Resolve(t.ServerAddr, t.ServerPort)`, falling back to the static value when nothing was captured) and calls `span.SetAttributes(shared.ServerAttributes(addr, port)...)`.

  This is **additive to each call site, not a restructure**. The `db.*` attributes stay attached at `tracer.Start(..., trace.WithAttributes(shared.DBAttributes(...)...))` exactly as today, but `DBAttributes` **drops its `serverAddr`/`serverPort` params** — it emits `db.*` only, never `server.*`. So no static `server.*` can be attached at start **by construction**: there is no argument through which to pass one. `server.*` is emitted **once**, post-call, from the per-command value (with static fallback) via `shared.ServerAttributes(...)`. Each site gains one post-call block:
  ```go
  ctx, span := t.Tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient),
      trace.WithAttributes(shared.DBAttributes(dbName, collName, op, batchSize)...))  // db.* only; no server args
  defer span.End()
  ctx, capture := shared.WithAddrCapture(ctx)          // NEW: stash holder in ctx
  // ... existing propagation/inject, unchanged ...
  res, err := t.Coll.XxxOne(ctx, ...)                  // raw call receives holder-ctx
  if addr, port := capture.Resolve(t.ServerAddr, t.ServerPort); addr != "" {
      span.SetAttributes(shared.ServerAttributes(addr, port)...)   // NEW: emit server.* (captured, else static fallback)
  }
  shared.RecordSpanError(span, err)                    // unchanged
  ```

  **Why emit `server.*` only post-call, not start-then-overwrite.** The OTel Go SDK builds the exported snapshot at `span.End()`, *not* at `tracer.Start()`, and `SetAttributes` **upserts by key** — it replaces an existing key but **never removes** a key absent from the new set. That last property is the trap in a start-then-overwrite design: `ServerAttributes` omits `server.port` for the default 27017 (semconv rule). If a static non-default `server.port` (e.g. from URI `mongodb://host1:27018,host2`) were set at start and the command were then served by a host on 27017, the post-call `SetAttributes` would overwrite `server.address` but leave the stale `server.port=27018` attached — a mismatched pair. Emitting `server.*` **once**, post-call, as a single write structurally eliminates this: the exported `server.address`/`server.port` are always a same-source pair.

  `db.*` is still attached at start, so the two advantages of start-time attributes are preserved for everything *except* `server.*`:
  - **Robustness**: if the raw call panics or returns early and `span.End()` fires via `defer`, the span still carries `db.system.name`/`db.operation.name`/`db.namespace`. Only `server.*` is absent on that path — acceptable, and better than emitting a wrong Connect-time-guess address.
  - **Sampling**: samplers inspecting start-time attributes see the full `db.*` set. They do **not** see `server.*` at start — acceptable, because the pre-change static value was a Connect-time guess (the very inaccuracy this change fixes), so sampling on it was never meaningful.

  Cost: `internal/direct` is untouched; only `internal/traced/collection.go` gains the post-call block.

  **Helper split.** **Move** the `server.address`/`server.port` tail of `DBAttributes` (`semconv.go`, the `if serverAddr != "" { ... }` block) out into `shared.ServerAttributes(addr string, port int) []attribute.KeyValue`, and **drop** `DBAttributes`' `serverAddr`/`serverPort` params — `DBAttributes` no longer references server at all. `ServerAttributes` becomes the **sole** emitter of `server.*`, called only on the post-call path. This makes "no static `server.*` at start" **compiler-enforced** rather than a convention: `DBAttributes` has no server argument through which to leak one, and there is exactly one server path, so the two paths cannot drift because there is no second path. `ServerAttributes` returns `nil` for an empty address and omits `server.port` for the default 27017 — identical rules to today.

### 3. Multiple `Started` events per call (retries): last write wins
Retryable reads/writes can retry once. We do not special-case this — the holder is overwritten on every `Started` event for that ctx, so the last (i.e., the attempt that actually produced the result returned to the caller) wins. This matches "the server that ultimately served this operation."

### 4. Registration point and disabled-mode invariant
The `CommandMonitor` is built and attached to `*options.ClientOptions` only inside the `mongoTracingEnabled()` branch of `ConnectWithOptions` (the branch that already builds a real `tracer`). The disabled branch (noop tracer, `internal/direct` impls) is untouched — no monitor is registered, no holder machinery is exercised, consistent with the existing disabled-mode invariant (this isn't itself an OTel SDK call, but keeping it out of the disabled path avoids adding any behavior at all when tracing is off).

### 5. Registration via `MergeClientOptions`, and chaining a caller-supplied `SetMonitor`
**Registration is unified across v1 and v2 via `options.MergeClientOptions(opts...)`.** v2's `ConnectWithOptions` already merges `opts` into a single `merged *options.ClientOptions` before `mongo.Connect(merged)`; v1 adopts the same instead of walking `opts` for a last-non-nil `Monitor` and appending an extra `*options.ClientOptions`. Both then: read the effective caller monitor from `merged.Monitor`, build `shared.NewCommandMonitor(merged.Monitor)`, call `merged.SetMonitor(...)`, and pass the single `merged` to `mongo.Connect`. Both drivers expose `.Monitor` as an exported field and provide `MergeClientOptions`, so this needs no "how does Connect merge multiple options" guesswork. As a bonus it lets v1's fallback address derive from `parseServerFromURI(merged.GetURI())` (matching v2), retiring `lastNonEmptyURI(opts)`.

**Chaining.** If `merged.Monitor` is non-nil, our replacement `CommandMonitor.Started`/`Succeeded`/`Failed` callbacks call the address-capture logic first, then invoke the caller's original callback with the same event. Only `Started` needs interception; `Succeeded`/`Failed` are pure pass-throughs to the caller's when non-nil, no-ops otherwise (no nil-function-call panic). This preserves 100% of caller-visible monitor behavior.

### 6. Fallback retained
`parseServerFromURI`, `lastNonEmptyURI`, and `Client.serverAddr`/`serverPort` are unchanged and remain in place. `internal/traced.Collection` keeps its `ServerAddr`/`ServerPort` fields (fed from `Client`/`Database` as today) and uses them when the per-call holder has a zero value (`addr == ""`) after the raw call returns — e.g. defensive coverage for any future driver operation path that doesn't go through `CommandMonitor`, or a same-process mock/test double that doesn't invoke it.

### 7. Address parsing
`ConnectionID` format is `<addr>[-<n>]`; split on the first `[` and reuse the existing `net.SplitHostPort`-based host/port extraction logic that `parseServerFromURI` already implements (factor into a small shared helper e.g. `splitHostPort(s string) (string, int)` in `internal/shared/` used by both the URI parser and the new `ConnectionID` parser, to avoid duplicating the IPv6-bracket-aware logic). Default-port omission rule (27017) stays identical.

## Risks / Trade-offs

- **[Risk] Touching every span call site (14 in v1, 16 in v2)** → but the change is *additive*, not a restructure: one `shared.WithAddrCapture(ctx)` line after `tracer.Start` plus one post-call `if ...; addr != "" { span.SetAttributes(shared.ServerAttributes(addr, port)...) }` block; the `tracer.Start(..., trace.WithAttributes(shared.DBAttributes(...)...))` line changes only in that the two trailing `DBAttributes` server args are dropped (the `DBAttributes` signature no longer has them — no static `server.*` at start, per Decision 2). **Mitigation**: mechanical, identical transform per site; cover with a table-driven test asserting `server.address` matches an injected fake `ConnectionID` per operation, run in both v1 and v2. Watch through the shared `runUpdate`/`runDelete` helpers and the two `DBAttributes` lines inside `Watch` so no site is missed or double-counted.
- **[Correctness] Holder mutation + last-write-wins must be race-free.** `shared.WithAddrCapture` mints a **fresh** holder per call, so concurrent operations sharing a common parent `ctx` never share a holder. `CommandMonitor.Started` runs *synchronously in the operation's own goroutine* (driver `operation.go`: `publishStartedEvent` → `op.CommandMonitor.Started(ctx, ...)`, no separate goroutine), and the traced method reads the holder only *after* the raw call returns — a happens-before via the function-call return. Therefore write and read are same-goroutine, never concurrent → `go test -race` clean by construction. This is the load-bearing safety argument behind Decision 3's last-write-wins.
- **[Risk] `context.WithValue` holder pattern adds a small per-call allocation + lookup on every traced operation** → **Mitigation**: negligible versus a network round trip; only active on the tracing-enabled path (matches existing cost model where tracing-enabled already allocates spans/attributes). The holder-`ctx` value survives any driver child-context wrapping (e.g. `WithTimeout` for socket deadlines) because `Value` lookups walk the parent chain.
- **[Risk] Chaining an unknown caller-supplied `CommandMonitor` could double-invoke side effects if the caller's callback is not idempotent, or could reorder relative to their expectations** → **Mitigation**: our callback always runs first and does not mutate the event before delegating; document the ordering explicitly in the package doc comment and README.
- **[Risk] Driver's `ConnectionID` format (`fmt.Sprintf("%s[-%d]", addr, nextConnectionID())`) is an internal implementation detail, not a public contract — could change in a future driver release** → **Mitigation**: parsing is defensive (falls back to the static value on any parse failure); add a test pinned to the current driver version's actual format so a driver upgrade that changes it fails CI loudly instead of silently degrading.
- **[Trade-off] BulkWrite and other multi-command operations may internally issue several driver commands (batches); we only capture the last `Started` event's address** → acceptable per Decision 3; documented as "represents the connection that handled the final batch," not "every batch."
- **[Note] Default-port omission under capture.** `ServerAttributes` omits `server.port` when the port is the default 27017 (unchanged rule). Because `server.*` is emitted **once**, post-call (Decision 2 — no static `server.*` at start), there is no stale-`server.port` hazard: a command served by `host:27017` emits `server.address` alone, cleanly. Test assertions for failover/replica-set scenarios must still account for the omission (a captured `host:27017[-1]` yields `server.address` only; `host:27018[-1]` yields both).

## Migration Plan

No data migration. This is an internal, non-breaking behavior change:
1. Implement in `otel-mongo/otelmongo` (v1) first, full build/test/lint green.
2. Mirror identically into `otel-mongo/v2`.
3. Bump `instrumentationVersion` in both `version.go` files (patch or minor per existing 0.x convention — this is additive/non-breaking, so a minor bump per repo convention for 0.x is appropriate, decided at tasks time).
4. No rollback concerns beyond a normal revert — no persisted state changes, no wire-format changes.

## Open Questions

- Should the version bump be `0.5.x` → next patch, or grouped into whatever the next planned `0.x` minor is? (Non-blocking — decide at release time, not implementation time.)
- Should README document the `SetMonitor` chaining behavior under a new subsection, or fold into the existing "NewCollection vs Connect" doc area? (Cosmetic — resolve during tasks/docs step.)
