# Context Map

Multi-module repo; each instrumentation module is its own bounded context.
Contexts are created lazily — a module gains a `CONTEXT.md` when its first
term is pinned down.

## Contexts

- [otel-nats Tracing](./otel-nats/CONTEXT.md) — span naming and attribute
  vocabulary for NATS/JetStream instrumentation

## Relationships

- All contexts share the OTel messaging semconv (v1.39.0) as their upstream
  vocabulary; module contexts only define terms semconv leaves open or that
  this codebase sharpens beyond it.
