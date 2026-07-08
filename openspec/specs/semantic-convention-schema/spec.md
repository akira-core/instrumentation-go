# semantic-convention-schema Specification

## Purpose
TBD - created by archiving change upgrade-wrapper-dependencies. Update Purpose after archive.
## Requirements
### Requirement: Tracer schema URL matches the imported semconv version
Every wrapper package that creates a `Tracer` with `trace.WithSchemaURL(semconv.SchemaURL)` SHALL import `go.opentelemetry.io/otel/semconv/v1.39.0` — the semconv version pinned to stay aligned with the downstream consumer's policy (`otel-nats` is consumed by `flywindy/o11y`, whose ADR 0006 pins the SDK to semconv v1.39.0), and itself a subpackage version bundled in the pinned `go.opentelemetry.io/otel` v1.44.0 release — so the reported schema URL (`semconv.SchemaURL`) stays aligned with the generated attribute helpers used in the same file. The pin is version-aligned with the downstream policy rather than "the highest bundled semconv version". Packages that do not call `WithSchemaURL` are unaffected; packages that import semconv only for non-schema helpers (e.g. `semconv.ServiceName`) SHALL still use the same import version as the rest of the repo for consistency.

#### Scenario: Schema URL updates with semconv import bump
- **WHEN** the semconv import path is bumped from `v1.37.0` to the policy-aligned `v1.39.0`
- **THEN** every `Tracer` created with `trace.WithSchemaURL(semconv.SchemaURL)` reports `https://opentelemetry.io/schemas/1.39.0` as its schema URL

#### Scenario: Messaging attribute helpers still compile
- **WHEN** `otelnats` or `oteljetstream` references generated semconv keys (`MessagingSystemKey`, `MessagingDestinationNameKey`, `MessagingOperationNameKey`, `MessagingMessageBodySize`, etc.) after the import bump
- **THEN** `go build` succeeds without attribute-key renames, and emitted span attribute key strings remain backward-compatible for downstream consumers

#### Scenario: Mongo hand-written semconv keys unchanged
- **WHEN** `otel-mongo` `internal/shared/semconv.go` continues to use hand-written stable attribute keys
- **THEN** the semconv import bump does not require changes to that file unless a deliberate migration to generated semconv is undertaken separately

