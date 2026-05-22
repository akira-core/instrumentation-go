# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Sibling module of `otel-mongo`, `otel-mongo/v2`, `otel-nats`. The repo-root `pkg/instrumentation-go/CLAUDE.md` covers cross-module conventions (wrapper pattern, env-flag matrix, semconv, deliver-span design, versioning policy, `internal/flags/` byte-identical rule, strategy-split layout). This file only documents what is specific to `otel-gorilla-ws`.

## Module identity

- Module path: `github.com/Marz32onE/instrumentation-go/otel-gorilla-ws`
- Single package (`otelgorillaws`) — no sub-packages on the public surface.
- Current `instrumentationVersion`: `version.go` (`Version()` returns this string). Currently `0.5.1`.
- Sibling JS packages (`otel-ws`, `otel-rxjs-ws` in `pkg/instrumentation-js/`) MUST produce/consume the exact same wire envelope — any change to `internal/shared/wire.go` is a cross-language contract change.

## Common commands (run inside `otel-gorilla-ws/`)

```bash
go build ./...
go test -v -race ./...
go test -v -race -run TestName ./...               # single test
golangci-lint run ./...                            # v2 syntax required

# Integration tests live in a separate go-module (spawns a real httptest server)
cd tests/integration && go test -v -race ./...

# Example is also its own module; cd in before running
cd examples/basic && go run .
```

All three of `go build`, `go test -race`, `golangci-lint run` MUST pass with 0 issues before any commit. `goimports` requires: stdlib group → blank line → third-party group → blank line → local prefix `github.com/Marz32onE/instrumentation-go`.

## Architecture specifics

### Two-tier flag surface (no propagation flag)

`otel-gorilla-ws` reads only two env vars, in contrast to the 3-tier surface used by `otel-mongo` and `otel-nats`:

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` — global master
- `OTEL_GORILLA_WS_TRACING_ENABLED` — module tracing

There is **no** `OTEL_GORILLA_WS_PROPAGATION_ENABLED`. The JSON envelope is constructed inline in `internal/shared/wire.go`'s `MarshalWire` — there is no equivalent "wrapper spans on, wire propagation off" mode that fits the existing wire format. The **subprotocol negotiation runtime check** serves the same per-connection opt-out purpose a propagation flag would.

### Subprotocol negotiation as the per-connection gate

Tracing is enabled per-`Conn` only when **all of**: env gate ON, AND the WebSocket handshake negotiated the `otel-ws` subprotocol. The negotiation rules (see `conn.go` doc comment and `upgrader.go`):

- **Client `Dial`**: if `subprotocols` is non-empty, `otel-ws` is **prepended** to the proposed list. Tracing enabled iff server responds with an `otel-ws+<negotiated>` prefixed protocol. If `subprotocols` is nil/empty, no injection — passthrough.
- **Server `Upgrader.Upgrade`**: detects `otel-ws` in client's list; responds with `otel-ws+<negotiated>` (the second app protocol picked from the remaining list). Tracing enabled on acceptance.
- **`NewConn`**: assumes negotiated = true (back-compat for callers managing the handshake themselves). Use `Dial`/`Upgrader.Upgrade` for spec-compliant negotiation.

`appProtocolFromRaw` / `isOTelWireProtocol` (in `conn.go`) and `splitClientProtocols` (in `upgrader.go`) implement the wire-protocol prefix handling. Wire-protocol token (`otel-ws`) and prefix separator (`+`) are intentionally hardcoded — they are part of the cross-language wire contract with the JS packages.

### Strategy split (package boundary)

```
otelgorillaws/        conn.go upgrader.go message.go options.go constants.go
                      env_flags.go version.go doc.go helpers.go
internal/
├── flags/            # byte-identical across all four modules
├── shared/           # ConnImpl interface, MarshalWire / TryUnmarshalWire envelope codec
├── direct/           # disabled-mode impl — ZERO go.opentelemetry.io/otel/sdk / otel/exporters imports
└── traced/           # enabled-mode impl — full instrumentation
```

`newConn(conn, negotiated, opts...)` in `conn.go` checks `wsTracingEnabled() && negotiated` **once** and picks either `direct.NewConn` (passthrough delegate) or `traced.NewConn(... negotiated)`. Public `Conn.WriteMessage` / `Conn.ReadMessage` are single-line `c.impl.<Method>(...)` delegates — **no per-call env reads, no runtime gate branches in the hot path**.

Compile-time assertions `var _ shared.ConnImpl = (*direct.Conn)(nil)` and `(*traced.Conn)(nil)` (in `conn.go`) fail the build if any impl misses a method. The disabled-mode invariant for this module is enforced by the same `drift-check` CI job that protects the other modules — `internal/direct/` MUST NOT import `go.opentelemetry.io/otel/sdk/*` or `go.opentelemetry.io/otel/exporters/*`.

### Adding a public method to `Conn`

Touch THREE files in lockstep:
1. Add to `shared.ConnImpl` (`internal/shared/conn.go`).
2. Implement passthrough in `internal/direct/conn.go` — must compile without `otel/sdk` or `otel/exporters`.
3. Implement instrumented version in `internal/traced/conn.go` — full SDK access.

Then add a single-line facade method (`c.impl.X(...)`) in `conn.go`. Compile-time assertions in `conn.go` will fail if either impl misses the new method.

### Wire envelope (cross-language contract)

`internal/shared/wire.go` `MarshalWire` produces:

```json
{"header":{"traceparent":"...","tracestate":"..."},"data":<payload>}
```

`data` is the original payload **as-is** if it is valid JSON, otherwise a JSON-encoded string. `TryUnmarshalWire` accepts the same envelope OR the **legacy flat format** (`{"traceparent":"...","tracestate":"...","...fields"}`) for backward compatibility with older Go-only deployments. Non-envelope, non-flat payloads pass through unchanged with no extracted context.

`MarshalWire` is hand-written (no reflection) and uses a `sync.Pool` of byte buffers capped at 64 KiB to avoid unbounded retention. When editing wire code, do NOT introduce `json.Marshal(envelope)` — the hand-written serializer is a measured hot-path optimisation and the test suite asserts exact byte output for the cross-language contract.

### `Conn.ReadMessage` returns `context.Context` (signature change vs. native)

`Conn.ReadMessage(ctx context.Context) (context.Context, int, []byte, error)` differs from native `gorilla/websocket` `(*websocket.Conn).ReadMessage()`:
- Adds leading `ctx` argument for span parenting.
- Adds leading `context.Context` return value carrying the extracted remote trace.

Callers must thread the returned ctx into downstream calls to continue the trace chain. The signature change is intentional — it is the only way to make trace extraction visible to callers without hiding it inside a global.

`Conn.WriteMessage(ctx, messageType, data)` also takes a leading `ctx`. Use the embedded `*websocket.Conn` for any other native methods (Close, WriteJSON, ReadJSON, Ping/Pong handlers, etc.) — those are unwrapped passthrough.

### `Subprotocol()` strips the otel-ws prefix

`Conn.Subprotocol()` returns the application protocol with the `otel-ws+` prefix removed (e.g. raw `otel-ws+json` → `json`). Callers expecting the negotiated app protocol see what they would have seen without otel-ws involvement. To check whether otel-ws negotiated successfully, call `Conn.TracingNegotiated()` (or similar accessor) rather than parsing `Subprotocol()`.

### Cached gate + `ResetForTest`

`env_flags.go` declares `wsGate = flags.NewGate(func() bool { ... })` composing the two env vars into a single boolean. `sync.Once`-cached for the process lifetime via `atomic.Bool`. **Env changes after the first call are ignored for the rest of the process.**

Tests that flip env vars via `t.Setenv` MUST call `wsGate.ResetForTest()` after the Setenv. Existing tests in `env_flags_test.go` and `options_test.go` use the pattern:

```go
t.Setenv(envGlobalTracingEnabled, "true")
t.Setenv(envWSTracingEnabled, "true")
wsGate.ResetForTest()
t.Cleanup(wsGate.ResetForTest)
```

Do **NOT** add `t.Parallel()` to tests that touch these env vars — the reset is process-global.

### Re-exported constants

`constants.go` re-exports the gorilla/websocket message-type and close-code constants (`TextMessage`, `BinaryMessage`, `CloseNormalClosure`, ...) plus `CloseError` as a type alias. Callers can import only `otel-gorilla-ws` instead of also importing `gorilla/websocket`. When adding new re-exports, mirror the upstream name exactly — these are intentionally a 1:1 alias surface.

## Version bump checklist

Any code change to this module requires bumping `version.go` `instrumentationVersion` before pushing the release tag `otel-gorilla-ws/v<x.y.z>`. Pre-1.0 (`0.x.y`), a minor bump is allowed for breaking changes. Any change to `internal/shared/wire.go` is **also** a cross-language contract change — coordinate with the JS sibling packages (`pkg/instrumentation-js/packages/otel-ws`, `otel-rxjs-ws`) and bump those as well.

## Local development against sibling consumers

The repo-root `otel-traces-test` services (`worker/`, `frontend/`) consume this module via `replace` directives in each service's `go.mod` pointing to `../pkg/instrumentation-go/otel-gorilla-ws`. After editing this module, no rebuild of this module is needed — just `go build` / `go test` in the consuming service.

The frontend integration test path crosses language boundaries: `worker` writes envelopes here, the browser reads them via `@marz32one/otel-rxjs-ws`. Any wire-format change should be exercised end-to-end via the repo-root `make verify-trace` after rebuilding both sides.
