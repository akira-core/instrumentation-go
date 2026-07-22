## 1. Rename method — otel-mongo v1 (already implemented)

- [x] 1.1 Facade: rename `Cursor.DecodeWithContext` → `DecodeAndTrace` (`otelmongo/cursor.go`)
- [x] 1.2 Facade: rename `ChangeStream.DecodeWithContext` → `DecodeAndTrace` (`otelmongo/results.go`)
- [x] 1.3 Interfaces: rename method on `CursorImpl` + `ChangeStreamImpl` (`internal/shared/impls.go`)
- [x] 1.4 Impls: rename in `internal/direct/{cursor,changestream}.go` and `internal/traced/{cursor,changestream}.go`
- [x] 1.5 Rename test file + funcs (`cursor_decodewithcontext_test.go` → `cursor_decodeandtrace_test.go`)

## 2. Rename method — otel-mongo/v2 (already implemented)

- [x] 2.1 Facade: rename on `Cursor` (`v2/cursor.go`) and `ChangeStream` (`v2/results.go`)
- [x] 2.2 Interfaces + impls: `v2/internal/shared/impls.go`, `v2/internal/{direct,traced}/{cursor,changestream}.go`
- [x] 2.3 Update tests (`v2/cursor_test.go`, `v2/tests/integration/mongo_test.go`)

## 3. Docs (already implemented)

- [x] 3.1 Update `otel-mongo/README.md`, `README.zh-TW.md`, `examples/main.go`
- [x] 3.2 Update `CLAUDE.md` reference

## 4. Versioning

- [x] 4.1 Bump v1 constant `0.7.0` → `0.8.0` (`otel-mongo/otelmongo/version.go`)
- [x] 4.2 Bump v2 constant `2.7.0` → `2.8.0` (`otel-mongo/v2/version.go`)
- [x] 4.3 Add `## [0.8.0]` BREAKING entry to `otel-mongo/CHANGELOG.md` (rename old→new)
- [x] 4.4 Add `## [2.8.0]` BREAKING entry to `otel-mongo/v2/CHANGELOG.md` (parity)
- [x] 4.5 Update version pin tests (`version_test.go`, both modules)

## 5. Verify

- [x] 5.1 `go build ./...`, `go test -race ./...`, `golangci-lint run ./...` pass in `otel-mongo/otelmongo`
- [x] 5.2 Same three pass in `otel-mongo/v2`
- [x] 5.3 `grep -r DecodeWithContext` returns nothing in `.go` sources (only CHANGELOG/change-artifact/canonical-spec-until-archive references remain)

## 6. Backfill test coverage (opportunistic, this area)

- [x] 6.1 v1 `TestCursorDecodeAndTrace_NoTrace` — enabled tracing + doc without `_oteltrace` → decode span with no link (parity with v2)
- [x] 6.2 v1 + v2 `TestChangeStreamDecodeAndTrace_LinksOriginFromFullDocument` — `_oteltrace` nested in `fullDocument` → decode span linked to origin (tolerates driver `ErrNilCursor`, which occurs after extraction+link)
- [x] 6.3 v1 + v2 `TestChangeStreamDecodeAndTrace_NoLinkWhenNoTraceMetadata` — `fullDocument` without `_oteltrace` → decode span, no link
- [x] 6.4 Re-verify build/test/lint both modules after test additions
