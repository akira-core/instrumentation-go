## Why

`otel-mongo` (v1 `otelmongo/` + v2 `v2/`) derives the `server.address`/`server.port` attributes on CLIENT spans by statically parsing the connection URI once at `Connect`/`ConnectWithOptions` time (`parseServerFromURI` in `client.go`). This only looks at the first host of a comma-separated replica-set list, never resolves `mongodb+srv://` SRV records, and never updates after failover or topology changes — every span for the lifetime of the `Client` carries the same value regardless of which server actually handled the operation. This misrepresents `server.address` for replica sets, sharded clusters, and SRV connection strings, and diverges from OTel's db semconv intent (`server.address` should identify the server that served the request).

## What Changes

- Add a `event.CommandMonitor` (registered on `*options.ClientOptions` at `ConnectWithOptions` time, only when tracing is enabled) that captures the real per-command server address from `CommandStartedEvent.ConnectionID` (driver-internal format `<host>:<port>[-<n>]`) instead of relying solely on the static URI parse.
- Thread the captured address through each Collection CRUD operation in `internal/traced/collection.go` via a per-call `context.Context`-scoped holder. Static `db.*` attributes stay attached at span start as today, but `server.address`/`server.port` are **not** emitted at start — `shared.DBAttributes` **drops its `serverAddr`/`serverPort` params** and emits `db.*` only. `server.*` is emitted **once**, after the raw driver call, via `span.SetAttributes(shared.ServerAttributes(addr, port)...)` from the captured per-command value (static fallback) — an additive one-block change per site, not a restructure. Emitting `server.*` only post-call avoids a stale-`server.port` mismatch that a start-then-overwrite would cause (`SetAttributes` upserts by key and never removes the default-port-omitted key); dropping the params makes "no static `server.*` at start" compiler-enforced.
- If the caller supplies their own `options.ClientOptions.SetMonitor(...)`, chain it — our `Started`/`Succeeded`/`Failed` callbacks run first (to capture the address), then the caller's original callbacks run unmodified. No caller-supplied monitor is silently dropped.
- Keep the existing static `parseServerFromURI`/`Client.serverAddr`/`serverPort` path as a fallback for the (expected to be rare/never) case where no `CommandStartedEvent` was captured for a call.
- No change to `Cursor`/`ChangeStream`/`SingleResult` spans — they don't call `shared.DBAttributes` directly (they link to the parent Collection span's already-baked attributes), so they're out of scope. (`Collection.Watch`'s own change-stream-open `aggregate` span *is* in scope — it is a `collection.go` `DBAttributes` site — but the `ChangeStream` reader's later `getMore` spans are not.)
- Applied identically to `otel-mongo/otelmongo` (v1) and `otel-mongo/v2`, per this repo's v1/v2 parity rule.
- Bump `instrumentationVersion` in both `otelmongo/version.go` and `v2/version.go`.

## Capabilities

### New Capabilities
- `otel-mongo-server-address`: how `otel-mongo` (v1 + v2) determines the `server.address`/`server.port` attribute values on CLIENT spans — command-monitor-based per-operation capture, the URI-parse fallback, and caller-monitor chaining behavior.

### Modified Capabilities
<!-- none: no baseline specs exist in openspec/specs/ (span-cleanup, which defined the current `otel-mongo-spans` capability, has not been archived yet) -->

## Impact

- **Modules**: `otel-mongo/otelmongo`, `otel-mongo/v2` only. `otel-nats` and `otel-gorilla-ws` are unaffected.
- **Source added**: a `CommandMonitor` constructor + `ConnectionID` parser + context-scoped address holder in `internal/shared/` (new file), a `ServerAttributes` helper **moved out** of `DBAttributes` in `internal/shared/semconv.go` (`DBAttributes` loses its `serverAddr`/`serverPort` params and no longer emits `server.*` — `ServerAttributes` is the sole `server.*` emitter, so the two paths can't drift because there is only one), `MergeClientOptions`-based monitor registration in `client.go` (`ConnectWithOptions`, v1+v2, with `//nolint:staticcheck SA1019` on v1 where the driver deprecates the merge helper), and a one-block post-call `span.SetAttributes(shared.ServerAttributes(...)...)` addition (start-time `DBAttributes` server args removed) at every `shared.DBAttributes(...)` call site in `internal/traced/collection.go` (14 sites in v1, 16 in v2).
- **Source unchanged in shape**: `parseServerFromURI`, `lastNonEmptyURI`, `Client.serverAddr`/`serverPort` remain as the fallback path. `shared.DBAttributes`' signature **does** change (drops `serverAddr string, serverPort int`) — an internal-only helper, no public API impact; its `server.*` tail moves verbatim into the new `shared.ServerAttributes(addr, port)`.
- **Public API**: no breaking changes. `ConnectWithOptions` behavior is additive — a `CommandMonitor` is now registered on the underlying `*options.ClientOptions` when tracing is enabled; any monitor the caller already set via `options.Client().SetMonitor(...)` continues to fire, chained after ours.
- **Disabled-mode invariant**: unaffected — the monitor is only registered on the tracing-enabled construction path; `internal/direct` and the disabled `ConnectWithOptions` branch never touch it.
- **Dependency surface**: uses `go.mongodb.org/mongo-driver/event` (v1) / `go.mongodb.org/mongo-driver/v2/event` (v2), both already transitive dependencies of the existing driver import — no new module dependency.
