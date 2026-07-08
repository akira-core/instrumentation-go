## Context

Four independently versioned Go modules (`otel-mongo` v1, `otel-mongo/v2`, `otel-nats`, `otel-gorilla-ws`) each wrap an upstream client library (MongoDB driver, `nats.go`, `gorilla/websocket`) and add OpenTelemetry tracing. Behavior today is documented only in per-module READMEs and the root `CLAUDE.md`; there are no `openspec/specs/` capability files. This design captures the cross-module architectural decisions those four modules already share, so the accompanying capability specs (`mongodb-tracing`, `nats-jetstream-tracing`, `websocket-tracing`, `shared-feature-flags`) can reference a single rationale instead of repeating it per module. Constraint: this change is documentation-only — no `.go` file changes — so every decision below describes **already-shipped** behavior (version 0.5.0), not a proposed future state.

## Goals / Non-Goals

**Goals:**
- Record why the two feature-flag enforcement patterns (strategy-split, cached-gate) coexist instead of one being used everywhere.
- Record why trace-context carriers differ per transport (document field vs. headers vs. JSON envelope).
- Record why deliver spans exist for Mongo/NATS but not WebSocket.
- Record why span **links** (not parent-child) are used for async consumers.
- Record why `internal/flags` is vendored per-module instead of factored into a shared Go module.
- Surface known drift risks so future changes don't silently reintroduce them.

**Non-Goals:**
- Not a full per-method API reference — that belongs in the capability specs.
- Not proposing any new capability, flag, or code change.
- Not a migration guide for adopting these packages (see each module's README `Usage` section).

## Decisions

### 1. Wrapper pattern with no owned TracerProvider
Packages accept `TracerProvider`/`Propagators` via functional options and otherwise fall back to `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()`. **Alternative considered**: an `InitTracer()` helper per package. **Rejected** because OTel Go Contrib instrumentation guidelines reserve provider/exporter lifecycle to the application — a library-owned provider risks double-initialization or conflicting shutdown when an app uses more than one instrumented package (all four modules are commonly used together).

### 2. Transport-native trace carriers
Each module injects W3C trace context into whatever extensibility point its transport already exposes: MongoDB gets a reserved `_oteltrace` document subfield (writes are inspectable/queryable documents), NATS/JetStream gets message headers (the protocol has first-class headers), WebSocket gets a JSON envelope wrapping the payload (the protocol has no header concept, only frames). **Alternative considered**: a uniform sidecar/out-of-band channel for all three. **Rejected** as it would require a new transport-specific side-channel per protocol anyway, with none of the benefit of using what each protocol already offers natively.

### 3. Two feature-flag enforcement patterns
Both patterns implement the same disabled-mode invariant (no OTel SDK code path runs when a flag is off), but at different points:
- **Strategy split** (otel-mongo `Collection`/`Cursor`/`SingleResult`/`ChangeStream`): construction picks one of two interface implementations (`internal/direct` vs `internal/traced`) once; the disabled path is **compiler-enforced** because `internal/direct` imports no `go.opentelemetry.io/otel/sdk/*` package, checked by a CI grep step.
- **Cached gate** (`otelnats`, `otel-gorilla-ws`, otel-mongo `Client`/`Database`): a `sync.Once`-cached bool is checked as the first statement of every public method; the disabled path is **reviewer-enforced** only.

**Rationale for keeping both**: strategy-split's interface-plus-two-impl-packages overhead pays off for high-method-count, hot-path types (Collection has a dozen+ CRUD/aggregate methods); cached-gate is cheaper to extend for lower-arity wrapper types (a `Conn` with `Publish`/`Subscribe`/`Request`). **Alternative considered**: migrate everything to strategy-split for uniform compiler enforcement. **Not done** — tracked as an open question below, not yet a written plan.

### 4. Deliver spans for broker-mediated transports only
otel-mongo and otel-nats synthesize a "deliver" span with `service.name` set to the broker's address (`mongodb://host:port`, `nats://host:port`), making the broker a visible node in a service graph. otel-gorilla-ws does not — it only sets `SpanKindConsumer` on the read span. **Rationale**: Mongo and NATS have a discrete, addressable server sitting between producer and consumer; a raw WebSocket connection is a direct peer-to-peer stream with no separate broker identity to represent.

### 5. Span links, not parent-child, for async consumers
NATS subscribers, MongoDB change-stream readers, and WebSocket readers link to the producer span rather than becoming its child. **Rationale**: these consumers run asynchronously and potentially long after the producer span ended; a parent-child edge would misrepresent them as synchronously nested work, while a link preserves the causal relationship without that implication.

### 6. `internal/flags` vendored per module, not a shared Go module
The four copies of `internal/flags` (`EnvEnabled`, `Gate`) are required to stay byte-identical (documented in the package comment) rather than factored into a fifth shared module. **Rationale**: each of the four modules is independently versioned and tagged (`<module>/v<x.y.z>`); introducing a shared internal module would force lockstep versioning or a dependency edge between otherwise-independent modules. **Alternative considered**: a shared `internal/` Go module imported by all four. **Rejected** for the versioning-coupling reason above; revisit only if a fifth consumer emerges.

## Risks / Trade-offs

- **[Risk]** The four `internal/flags` copies (and, within otel-mongo, the two `internal/shared/*` trees) drift out of byte-identical sync over time. → **Mitigation**: doc comments mandate identical content; a CI drift-check step is planned per `CLAUDE.md` but **not yet implemented** — flag this as a live gap, not a solved problem.
- **[Risk]** Cached-gate correctness depends on every new method remembering the fast-path gate check as its first statement; nothing compiles-in the guarantee. → **Mitigation**: code review checklist in `CLAUDE.md` ("Adding a new public method to a cached-gate wrapper"); no automated enforcement exists.
- **[Risk]** otel-mongo v1/v2 parity requires manually double-applying every logic change across two parallel `internal/{direct,traced,shared}/` trees. → **Mitigation**: `CLAUDE.md` mandates running lint/tests for both when either is touched; no automated parity check exists.
- **[Bug, found during this baseline's source verification]** `otelnats.ConnectTLS` and `otelnats.ConnectWithCredentials` panic with a nil-pointer dereference whenever both tracing flags are enabled. Both forward a bare `nil` literal as the sole positional argument into their `...WithOptions` sibling's variadic `traceOpts ...Option` parameter, which Go compiles as a one-element `[]Option{nil}` slice (not an empty slice); `newConnConfig` then calls `.apply(c)` on that nil `Option` interface value. Untested by `conn_test.go`. → **Mitigation**: none shipped — this is a real, live bug, out of scope for this documentation-only change to fix. Tracked as a known limitation in `nats-jetstream-tracing/spec.md`; a follow-up code change should fix `connect.go` to forward an empty `traceOpts` slice (or no trailing argument) instead of a bare `nil`.
- **[Risk, found during this baseline's source verification]** The "composed per-module gates" pattern (decision would suggest a uniform `Gate`-wrapped AND of global+module flags) does not actually hold for otel-mongo: `mongoTracingEnabled()` is a plain, uncached function, not `Gate`-wrapped, unlike `otelnats`'s and `otel-gorilla-ws`'s tracing gates. → **Mitigation**: none — `shared-feature-flags/spec.md` documents the actual per-module composition differences rather than asserting false uniformity; revisit if otel-mongo is ever migrated to a `Gate`-based tracing check for consistency.

## Migration Plan

None — this is a documentation-only change. Archiving requires merging the spec files into `openspec/specs/`; no code deploys, no rollback path needed.

## Open Questions

- Should `otelnats`, `otel-gorilla-ws`, and otel-mongo `Client`/`Database` migrate from cached-gate to strategy-split for compiler-enforced parity with `Collection`/`Cursor`? (Noted in `CLAUDE.md` as "planned but not yet tracked in a written design doc.")
- Should a CI drift-check be added for the duplicated `internal/shared/*` files between otel-mongo v1 and v2, and the four `internal/flags` copies?
- Should `internal/flags` be extracted into a real shared module if a fifth instrumentation module is ever added?
