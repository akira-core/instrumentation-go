## Why

This repository ships four independently versioned OpenTelemetry instrumentation modules (otel-mongo v1/v2, otel-nats, otel-gorilla-ws) whose behavior is currently documented only in READMEs and `CLAUDE.md`, with no OpenSpec capability specs (`openspec/specs/` is empty). There is no spec-level contract to validate future changes against, or to detect drift between the four modules' parity requirements (e.g. otel-mongo v1/v2, or the four byte-identical `internal/flags` copies). The repo just stabilized at version 0.5.0 after a repository migration (PR #17), making this a good checkpoint to baseline specs before further feature work.

## What Changes

- Baseline four new capability specs describing **existing, shipped behavior** (documentation-only change; no source code is modified):
  - `mongodb-tracing`: otel-mongo v1 + v2 wrapper behavior — feature-flag gating, `_oteltrace` document propagation, deliver spans, strategy-split disabled-mode invariant.
  - `nats-jetstream-tracing`: otelnats + oteljetstream — header propagation, deliver spans, `MessageBatch.Stop()`, NATS 2.11+ trace events, unsupported API surface.
  - `websocket-tracing`: otel-gorilla-ws — envelope format, `NewConn` vs `Dial`/`Upgrader` negotiation, legacy flat-format fallback.
  - `shared-feature-flags`: the `internal/flags` package (`EnvEnabled`, `Gate`) vendored byte-identically across all four modules, and the two enforcement patterns (strategy split, cached gate) built on top of it.
- Add `design.md` capturing the cross-module architectural decisions (wrapper pattern, TracerProvider/propagator fallback, span-link-vs-parent-child, disabled-mode invariant) that apply to all four capabilities.
- Add `tasks.md` as a documentation-verification checklist (no code tasks — this change produces specs only).

## Capabilities

### New Capabilities
- `mongodb-tracing`: MongoDB driver v1/v2 wrapper tracing — client spans, `_oteltrace` document propagation, deliver spans, feature-flag gating.
- `nats-jetstream-tracing`: Core NATS + JetStream wrapper tracing — header propagation, deliver spans, trace events, consumer/message-batch lifecycle.
- `websocket-tracing`: gorilla/websocket wrapper tracing — envelope propagation, subprotocol negotiation, legacy fallback.
- `shared-feature-flags`: Shared env-var-driven feature-flag primitives (`EnvEnabled`, `Gate`) vendored across all four modules.

### Modified Capabilities
(none — `openspec/specs/` currently has no existing capabilities)

## Impact

- **Affected paths**: `openspec/changes/document-otel-instrumentation/**` only. No `.go` source, `go.mod`, or CI files change.
- **Source of truth**: `otel-mongo/`, `otel-mongo/v2/`, `otel-nats/`, `otel-gorilla-ws/` (code) and their READMEs, plus root `CLAUDE.md` and `otel-ws.md`, are the reference material these specs are derived from.
- **Future changes**: once archived, subsequent feature work (e.g. new deliver-span kinds, new feature flags) should add delta specs against these baseline capabilities instead of only updating READMEs.
