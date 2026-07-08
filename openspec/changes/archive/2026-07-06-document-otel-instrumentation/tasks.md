## 1. Spec Validation

- [x] 1.1 Run `openspec validate --change document-otel-instrumentation --strict` and fix any schema errors (missing scenarios, malformed headers)
- [x] 1.2 Cross-check every `mongodb-tracing` requirement against `otel-mongo/README.md`, `otel-mongo/v2` source, and `CLAUDE.md`'s otel-mongo section for accuracy (5 issues found and fixed: deliver-span scope, slog default-level accuracy, `ContextFromDocument` override blind spot, missing `BulkWrite` coverage, direct-package import-check precision)
- [x] 1.3 Cross-check every `nats-jetstream-tracing` requirement against `otel-nats/README.md` and `CLAUDE.md`'s otel-nats section (6 issues found and fixed, including a real `ConnectTLS`/`ConnectWithCredentials` nil-panic bug now documented as a known limitation)
- [x] 1.4 Cross-check every `websocket-tracing` requirement against `otel-gorilla-ws/README.md`, `otel-ws.md`, and `CLAUDE.md`'s otel-gorilla-ws section (2 issues found and fixed: noop-tracer-when-disabled scope, conditional otel-ws injection)
- [x] 1.5 Cross-check every `shared-feature-flags` requirement against the four `internal/flags/flags.go` copies (confirm they are still byte-identical) (3 issues found and fixed: otel-mongo's tracing gate is not actually Gate-wrapped, propagation gate is read per-call not per-construction, empty-string truthy edge case added)

## 2. Consistency Review

- [x] 2.1 Confirm no requirement in any spec file contradicts documented behavior in `CLAUDE.md` or a module README (checked `ConnectTLS`/`ConnectWithCredentials`, `Gate`, and deliver-span mentions in `CLAUDE.md` — no contradictions; the otel-nats README does not mention the `ConnectTLS`/`ConnectWithCredentials` panic bug either, a pre-existing README gap outside this change's scope)
- [x] 2.2 Confirm every requirement has at least one `#### Scenario:` block using exactly four hashtags and WHEN/THEN bullets (scenario counts exceed requirement counts in all 4 files: mongodb-tracing 10 req/20 scen, nats-jetstream-tracing 11 req/18 scen, websocket-tracing 8 req/13 scen, shared-feature-flags 5 req/12 scen; zero malformed 3-hash scenario headers)
- [x] 2.3 Confirm `design.md`'s decisions section matches the rationale already documented in `CLAUDE.md` (strategy-split vs cached-gate, deliver spans, span links) — consistent; added two new Risks entries surfaced by source verification (§1.2-1.5)

## 3. Archive

- [x] 3.1 Run `openspec archive document-otel-instrumentation` to move the four capability specs into `openspec/specs/`
- [x] 3.2 Verify `openspec/specs/mongodb-tracing/spec.md`, `openspec/specs/nats-jetstream-tracing/spec.md`, `openspec/specs/websocket-tracing/spec.md`, and `openspec/specs/shared-feature-flags/spec.md` all exist post-archive — confirmed, all 4 present
