## Why

The exported method name `DecodeWithContext` was ambiguous — it collides conceptually with the Go driver v2's own `Decode`/context conventions and does not convey that its distinguishing behavior is emitting a trace span and extracting `_oteltrace`. Renaming to `DecodeAndTrace` makes the trace side effect explicit at the call site.

## What Changes

- **BREAKING**: Rename the exported method `DecodeWithContext(ctx, val) (context.Context, error)` to `DecodeAndTrace(ctx, val) (context.Context, error)` on both `Cursor` and `ChangeStream`, in `otel-mongo` (v1) and `otel-mongo/v2`. Signature and behavior are unchanged — pure rename.
- Rename the corresponding method on the internal strategy interfaces (`shared.CursorImpl`, `shared.ChangeStreamImpl`) and both impls (`internal/direct`, `internal/traced`) in both modules.
- Version bump (pre-1.0 breaking → minor): v1 `0.7.0` → `0.8.0`, v2 `2.7.0` → `2.8.0`, with CHANGELOG entries in both modules.
- Update docs/examples referencing the old name (README.md, README.zh-TW.md, examples/main.go, CLAUDE.md).

## Capabilities

### New Capabilities
<!-- none -->

### Modified Capabilities
- `mongodb-tracing`: the "Cursor decode with trace linking" requirement is renamed from `Cursor.DecodeWithContext` to `Cursor.DecodeAndTrace` (behavior identical).

## Impact

- Affected API: `otelmongo.Cursor`, `otelmongo.ChangeStream` (v1) and `v2.Cursor`, `v2.ChangeStream` — public method rename is a compile-break for any caller of `DecodeWithContext`.
- Affected code: facade `cursor.go`/`results.go`, `internal/shared/impls.go`, `internal/{direct,traced}/{cursor,changestream}.go`, tests (test file renamed), examples, READMEs, CLAUDE.md, version constants + CHANGELOGs — both modules, in parity.
- No behavior, wire-format, or span-shape change; no new dependencies.
