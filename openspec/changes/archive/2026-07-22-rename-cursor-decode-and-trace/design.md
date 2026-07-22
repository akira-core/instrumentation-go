## Context

`Cursor` and `ChangeStream` (both `otel-mongo` v1 and `otel-mongo/v2`) expose `DecodeWithContext(ctx, val) (context.Context, error)` — a `Decode` variant that also emits a `mongo.cursor.decode` span and returns a context enriched from the document's `_oteltrace` field. The name reads as "decode, taking a context" (which every method does), obscuring the actual differentiator: it traces. This is a naming-only change; the strategy-split layout (facade → `shared.*Impl` interface → `internal/{direct,traced}`) and all runtime behavior are untouched.

## Goals / Non-Goals

**Goals:**
- Rename the public method to `DecodeAndTrace` on `Cursor` and `ChangeStream` in both modules, in parity.
- Keep signature, span shape, `_oteltrace` handling, and disabled-path behavior byte-for-byte identical.
- Version-bump both modules and record the break in each CHANGELOG.

**Non-Goals:**
- No behavior, wire-format, span-kind, or attribute changes.
- No new test scenarios required for the rename itself (existing tests are renamed and still pass). Broader coverage gaps are tracked separately, not in this change.
- No deprecation shim / alias — pre-1.0 (and pre-1.0-tracked v2) allow a hard rename.

## Decisions

- **Hard rename, no alias.** Both modules are pre-1.0 in their release policy (v2 tracks `2.MINOR.PATCH` against the siblings' `0.MINOR.PATCH`). Keeping a deprecated `DecodeWithContext` forwarding to `DecodeAndTrace` would add permanent surface for a cosmetic gain; a clean break with a minor bump is the established convention here. Alternative (deprecation alias) rejected: no external-stability guarantee at this version.
- **Rename spans the full strategy stack in lockstep.** Facade method → `shared.CursorImpl`/`shared.ChangeStreamImpl` interface method → `internal/direct` + `internal/traced` impls, per module. The compile-time `var _ shared.CursorImpl = (*traced.Cursor)(nil)` assertions guarantee the build fails if any impl is missed.
- **Spec driven through the delta, not a raw edit.** The canonical `openspec/specs/mongodb-tracing/spec.md` is restored to the pre-change name; the rename lands as a `MODIFIED Requirements` delta in this change and is merged into the canonical spec at archive time.

## Risks / Trade-offs

- [Compile break for external callers of `DecodeWithContext`] → Documented as **BREAKING** in both CHANGELOGs with the old→new mapping; minor version bump signals it per the repo's pre-1.0 policy.
- [v1/v2 drift if only one module is renamed] → The parity rule (CLAUDE.md) plus the shared-interface compile assertions and the `go build`/`go test` gate on both modules catch a one-sided change.

## Migration Plan

- Callers replace `cursor.DecodeWithContext(ctx, v)` → `cursor.DecodeAndTrace(ctx, v)` and `changeStream.DecodeWithContext(ctx, v)` → `changeStream.DecodeAndTrace(ctx, v)`. No argument or return changes; a global rename suffices.
- No runtime rollout concern (compile-time only). Rollback = revert the rename commit + version bump.
