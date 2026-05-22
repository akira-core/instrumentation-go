# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Sibling module of `otel-mongo`, `otel-mongo/v2`, `otel-gorilla-ws`. The repo-root `pkg/instrumentation-go/CLAUDE.md` covers cross-module conventions (wrapper pattern, env-flag matrix, semconv, deliver-span design, versioning policy, `internal/flags/` byte-identical rule). This file only documents what is specific to `otel-nats`.

## Module identity

- Module path: `github.com/Marz32onE/instrumentation-go/otel-nats`
- Two packages live under one `go.mod`:
  - `otelnats/` — core NATS (Connect, Conn, Publish/Subscribe/Request, HeaderCarrier, deliver spans)
  - `oteljetstream/` — JetStream (JetStream/Stream/Consumer/PushConsumer/MessageBatch). Imports `otelnats` for shared `Conn`, tracer/propagator, server attrs, and the `PropagationEnabled()` gate.
- Current `instrumentationVersion`: `otelnats/version.go` (`Version()` returns this string). `oteljetstream` reuses it via `otelnats.Version()` — there is **no** separate version constant in `oteljetstream/`.

## Common commands (run inside `otel-nats/`)

```bash
go build ./...
go test -v -race ./...
go test -v -race -run TestName ./otelnats/...      # single test
golangci-lint run ./...                            # v2 syntax required
```

Integration tests live in their own go-module (testcontainers spawns `nats:alpine`):

```bash
cd tests/integration && go test -v -race ./...     # Docker/Podman must be running
```

The `examples/basic/` directory is also its own module — `go run ./examples/basic` from module root will fail; `cd examples/basic && go run .` works.

All three of `go build`, `go test`, `golangci-lint run` MUST pass with 0 issues before any commit. `goimports` requires stdlib group → blank line → third-party group → blank line → local prefix `github.com/Marz32onE/instrumentation-go`.

## Architecture specifics

### File-level strategy split (not package split)

`otel-nats` uses a **file-level** strategy split, not the package-level split used by `otel-mongo`'s Collection/Cursor. Same package, sibling files:

```
otelnats/        conn.go + conn_direct.go + conn_traced.go
oteljetstream/   jetstream.go + jetstream_direct.go + jetstream_traced.go
                 stream.go    + stream_direct.go    + stream_traced.go
                 consumer.go  + consumer_direct.go  + consumer_traced.go
```

`conn.go` defines the `connImpl` interface. `newConn` (in `conn.go`) reads `natsGate.Enabled()` **once** and assigns either `&directConn{}` or `&tracedConn{}`. Public `Conn` methods are single-line `c.impl.<Method>(...)` delegates — **no per-call env reads, no runtime gate branches in the hot path**. `oteljetstream.New` does the same for `JetStream`.

**The disabled-mode invariant for this module is enforced by a `drift-check` CI job** that greps every `*_direct.go` file for forbidden imports (`go.opentelemetry.io/otel/sdk/*`, `go.opentelemetry.io/otel/exporters/*`). When adding code to a `*_direct.go` file, do **not** import the SDK or any exporter — if you need them, the code belongs in `*_traced.go`. This gives the same compile-time guarantee as the package-level split in `otel-mongo/internal/direct/` without the package overhead.

### Adding a public method to `Conn` / `JetStream` / `Stream` / `Consumer`

Touch THREE files in lockstep:
1. Add to the impl interface (`connImpl` in `conn.go`, `JetStream` in `oteljetstream/jetstream.go`, etc.)
2. Implement passthrough in `*_direct.go` — must compile without `otel/sdk` or `otel/exporters`
3. Implement instrumented version in `*_traced.go` — full SDK access

Then add the single-line facade method (`c.impl.X(...)`) to the user-facing file. Compile fails if any impl misses a method.

### Cached gates and the `ResetGatesForTest` export

`otelnats/env_flags.go` declares two `flags.Gate`s built via `flags.NewGate(func() bool { ... })`:

- `natsGate` — composes `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_NATS_TRACING_ENABLED`
- `natsPropagationGate` — composes `natsGate` AND `OTEL_NATS_PROPAGATION_ENABLED`

Each gate reads env **once** via `sync.Once` then stores the result in an `atomic.Bool`. **Env changes after the first call are ignored for the rest of the process.**

For tests that flip env vars via `t.Setenv`, call `otelnats.ResetGatesForTest()` (from `test_helpers.go`) after the Setenv to reset both caches. This symbol is intentionally exported (not `_test.go`-only) so sibling test packages like `oteljetstream_test` can reset gates too — but production callers MUST NOT invoke it. Do **not** add `t.Parallel()` to tests that touch these env vars; the reset is process-global.

The propagation gate is a hard-prerequisite chain: setting `OTEL_NATS_PROPAGATION_ENABLED=true` while either tracing gate is OFF keeps propagation OFF.

### Deliver spans and `OTEL_EXPORTER_OTLP_ENDPOINT`

`initNATSProvider` in `conn.go` creates an independent `sdktrace.TracerProvider` with `service.name = nc.ConnectedUrlRedacted()` (falls back to `nats://<addr>`). It runs **only** when `OTEL_EXPORTER_OTLP_ENDPOINT` is set AND `natsTracingEnabled()` is true. Endpoint with `http://` / `https://` prefix → OTLP HTTP exporter; bare `host:port` → OTLP gRPC. Bare hostnames without scheme or port are unsupported.

This independent TP is shut down in `Conn.Close()` / `Conn.Drain()` with a 3 s timeout. Tests that build a `Conn` directly (bypassing `Connect`) and skip `Close` will leak the deliver-TP goroutine — always defer `conn.Close()`.

### `oteljetstream` propagation parity

The v0.4.x changelog documents a **fixed regression** where `tracedJSImpl.PublishMsg` called `propagator.Inject(...)` unconditionally regardless of the propagation gate, leaking `traceparent` on the wire even when `OTEL_NATS_PROPAGATION_ENABLED` was OFF. Fix: wrap inject (and the deliver-span construction immediately preceding it) in `if j.conn.PropagationEnabled() { ... }`. **When adding new JetStream paths that inject headers, always check `PropagationEnabled()` first** — mirror the otelnats core-NATS path exactly. Regression test: `TestJetStreamSubscribeWithPropagationOffDoesNotExtract`.

### Subscribe handler signature

`Conn.Subscribe(subject, MsgHandler)` — `MsgHandler` is `func(Msg)` (not native `func(*nats.Msg)`). `Msg` carries the original `*nats.Msg` plus the context with extracted trace. Callers thread `m.Context()` into downstream calls.

### Request/RequestMsg parent context

`Conn.Request(subject, data, timeout)` and `Conn.RequestMsg(msg, timeout)` mirror `nats.Conn.Request` exactly (no `ctx` arg) — the producer span uses `context.Background()` as parent. Callers that need to chain into an existing trace MUST use `RequestWithContext` or `RequestMsgWithContext`.

### `MessageBatch.Stop()` is required for early exit

`oteljetstream.MessageBatch` interface (`oteljetstream/consumer.go`) added `Stop()` in 0.3.0 — **breaking for custom impls.** Callers that drain `Messages()` to channel close need not call it; callers that `break` / `return` early MUST `defer batch.Stop()` to release the internal goroutine and end the in-flight span. The disabled-tracing path uses `directMessageBatch` which still spawns 1 goroutine for `jetstream.Msg → Msg` type adaptation.

## Version bump checklist

Any code change to either package requires bumping `otelnats/version.go` `instrumentationVersion` before pushing the release tag `otel-nats/v<x.y.z>`. Pre-1.0 (`0.x.y`), a minor bump is allowed for breaking changes (e.g. the 0.4.0 propagation-flag default-behaviour change). Update `CHANGELOG.md` with the same versioning, including a before/after wire-output table for any default-behaviour change.

## Local development against sibling consumers

The repo-root `otel-traces-test` services consume this module via `replace` directives in each service's `go.mod` pointing to `../pkg/instrumentation-go/otel-nats`. After editing this module, no rebuild of this module is needed — just `go build` / `go test` in the consuming service.
