# Changelog — otel-nats

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow per-package bump policy in repo root `CLAUDE.md`.

## [0.4.x] — feature-flag consolidation

### Added

- **`OTEL_NATS_PROPAGATION_ENABLED` env var.** Default OFF. Gates W3C `traceparent` / `tracestate` header inject (publish) + extract (subscribe). Only consulted when both `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_NATS_TRACING_ENABLED` are truthy — the tracing gate is the hard prerequisite. Enables a fourth state — "wrapper span emitted but wire format unchanged" — useful for local fan-out metrics or interop with non-OTel-aware downstreams.
- Per-conn `propagationEnabled` cached on the traced impl at construction time; hot paths read the field, never re-read env.
- Test helpers: `otelnats.ResetGatesForTest()` promoted from a `_test.go`-only symbol to a regular package-level export so sibling test packages (`oteljetstream_test`) can reset cached gates after `t.Setenv`. Not part of the production public API; production callers MUST NOT invoke.

### Changed — default-behaviour for header injection (BREAKING for v0.3.x deployments that relied on implicit injection)

In v0.3.x, enabling tracing alone (`OTEL_NATS_TRACING_ENABLED=true`) implicitly injected `traceparent` on every publish. Starting in 0.4.x, header injection requires `OTEL_NATS_PROPAGATION_ENABLED=true` to be explicit. Deployments that previously relied on implicit injection MUST add the propagation env var alongside their tracing env var to keep the same wire output.

#### Before / after — publish wire output

Setup: `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true`, `OTEL_NATS_TRACING_ENABLED=true`, **no other env vars**.

| | v0.3.x | v0.4.x (no migration) | v0.4.x (with `OTEL_NATS_PROPAGATION_ENABLED=true`) |
|---|---|---|---|
| `traceparent` header on the wire | present | **absent** | present |
| Wrapper PRODUCER span recorded | yes | yes | yes |
| Consumer-side `Extract` performed | yes | no | yes |
| Consumer span carries link to producer | yes | no | yes |

Same matrix applies to the JetStream Publish / Fetch / Consume paths after the `oteljetstream/jetstream_traced.go` fix (see Fixed below).

#### Migration recipe

```bash
# Find every config that enables nats tracing
grep -rE 'OTEL_NATS_TRACING_ENABLED' deploy/ config/ docker-compose*.yml

# Add the propagation env var alongside each match that you want injection for
# (env file, helm values, k8s manifest, systemd unit, etc.):
OTEL_NATS_PROPAGATION_ENABLED=true
```

Operators who deliberately want spans-only-no-wire propagation (e.g. wire-size sensitive fan-out, non-OTel-aware downstream consumer, partial rollout phase 1) leave the propagation env var unset.

### Fixed

- **`oteljetstream/jetstream_traced.go` Publish header injection regression.** `tracedJSImpl.PublishMsg` was calling `propagator.Inject(...)` unconditionally regardless of the propagation gate, contradicting the otelnats core-NATS path which gates on `propagationEnabled`. Fixed by wrapping the inject (and the deliver-span construction immediately preceding it) in `if j.conn.PropagationEnabled() { ... }`. Surface: pre-fix builds always emitted `traceparent` on JetStream publishes even when `OTEL_NATS_PROPAGATION_ENABLED` was off, leaking propagation state through the JetStream path while the core-NATS path correctly skipped injection. Caught by the new `TestJetStreamSubscribeWithPropagationOffDoesNotExtract` regression test.

### Migration checklist

1. Identify env files / helm values / k8s manifests that set `OTEL_NATS_TRACING_ENABLED` truthy
2. For every match where you want `traceparent` on the wire, add `OTEL_NATS_PROPAGATION_ENABLED=true`
3. Re-deploy, confirm `traceparent` reaches downstream services (e.g. inspect Tempo / collector for cross-service link continuity)
4. If observability fragments at a NATS hop, the propagation env var was missed — add it to that service's config
