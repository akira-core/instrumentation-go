# wrapper-api-parity Specification

## Purpose
TBD - created by archiving change upgrade-wrapper-dependencies. Update Purpose after archive.
## Requirements
### Requirement: Wrapped library public API remains reachable through the wrapper
Each wrapper package SHALL keep the public API of the library it wraps reachable to callers, so that upgrading the wrapped dependency never strands a user on functionality they cannot access through the wrapper. A wrapper satisfies this via one of three documented mechanisms: (a) **embedding** the upstream type so its exported methods are promoted, (b) **re-exporting** upstream types with `type X = upstream.X` aliases, or (c) exposing a **curated subset** of instrumented methods together with an **escape-hatch accessor** that returns the underlying upstream value (e.g. `otelnats.Conn.NatsConn() *nats.Conn`). When the wrapped library adds public API in an upgrade, any addition not already covered by mechanism (a) or (b) SHALL remain reachable via mechanism (c); a **trace-relevant** addition — one that must inject or extract trace context — SHALL additionally receive an instrumented wrapper method that honors the package's disabled-mode gate.

#### Scenario: Upstream adds a method to an embedded or aliased type
- **WHEN** the wrapped library adds an exported method to a type the wrapper embeds (`*mongo.Collection`, `*websocket.Conn`, `jetstream.Msg`) or re-exports as a `type X = upstream.X` alias (`oteljetstream.StreamConfig`, `oteljetstream.PubAck`, …)
- **THEN** the method or field is available to callers through the wrapper with no wrapper code change, because the embed promotes it and the alias *is* the upstream type

#### Scenario: Upstream adds a method to a curated wrapper
- **WHEN** the wrapped library adds an exported method to a type the wrapper re-exposes as a curated subset (`otelnats.Conn`) or a curated interface (`oteljetstream.JetStream`, `Consumer`, `Stream`, `ConsumeContext`, `MessagesContext`, `MessageBatch`)
- **THEN** a trace-relevant addition gets a new instrumented wrapper method whose first statement is the package's cached-gate delegation (`if !c.tracingEnabled { return c.nc.X(...) }`), and any non-trace addition either receives a pure passthrough method (when the interface is a full mirror) or stays reachable through the escape-hatch accessor — the caller is never left unable to reach the new upstream API, and the decision (wrap vs passthrough vs delegate) is recorded in the change

#### Scenario: A fully mirrored interface drops its escape hatch
- **WHEN** a curated interface re-exposes every method of its upstream counterpart (`oteljetstream.Consumer`, `Stream`, `ConsumeContext`, `MessagesContext`, `MessageBatch`), with non-trace additions handled as pure passthroughs
- **THEN** no `Unwrap()` escape hatch is required on that interface, and removing a previously-present one is permitted (a breaking change covered by the pre-1.0 `0.6.0` minor bump); the escape hatch is retained only where the wrapper deliberately re-exposes a subset — `otelnats.Conn.NatsConn()`, and `oteljetstream.JetStream.Unwrap()` for the `KeyValueManager`/`ObjectStoreManager` feature families it does not wrap

#### Scenario: Upstream removes or renames a wrapped method
- **WHEN** the wrapped library removes or renames an exported method that the wrapper embeds-and-references, aliases, or hand-declares
- **THEN** the mandatory `go build` fails at the reference, alias, or override site; for a method that an embedded type merely promoted (never referenced by wrapper code), the parity audit's grep of the wrapper's source and tests catches the silent drop — so the divergence is caught before release rather than shipped

