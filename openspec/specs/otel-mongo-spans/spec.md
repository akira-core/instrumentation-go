# otel-mongo-spans Specification

## Purpose
Span taxonomy for otel-mongo (v1 and v2): no deliver spans, spec-correct span kinds (CRUD/watch CLIENT, cursor decode INTERNAL), the MongoDB db.*/server.* attribute set, and the compiler-enforced disabled-mode invariant.

## Requirements

### Requirement: No deliver spans or deliver TracerProvider

`otel-mongo` (v1 `otelmongo/` and `v2/`) SHALL NOT emit synthetic "deliver" spans and SHALL NOT construct an independent deliver `TracerProvider`. No exported or internal identifier (`StartDeliverSpan`, `DeliverTracer`, `DeliverAttributes`, `initMongoProvider`, and any `WithDeliver*` option) SHALL remain. The packages SHALL NOT read `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission.

#### Scenario: No deliver span on a write

- **WHEN** `OTEL_EXPORTER_OTLP_ENDPOINT` is set and tracing is enabled and a caller invokes `InsertOne`, `InsertMany`, `UpdateOne`, `DeleteOne`, `Aggregate`, `BulkWrite`, or `Watch`
- **THEN** exactly one span (the operation span) SHALL be emitted for that call
- **AND** no span named `"* deliver"` SHALL be emitted
- **AND** no separate `BatchSpanProcessor` or OTLP exporter SHALL be created by the module

#### Scenario: Deliver identifiers removed from the API

- **WHEN** the module source is compiled
- **THEN** no reference to `StartDeliverSpan`, `DeliverTracer`, `DeliverAttributes`, or `initMongoProvider` SHALL exist in `otelmongo/` or `v2/`

### Requirement: Span kind per MongoDB operation

The wrapper SHALL set span kind per the OTel database semantic conventions (Mongo is a database, not a messaging system): synchronous DB calls use `CLIENT`, and local-only work uses `INTERNAL`.

#### Scenario: CRUD operations use CLIENT

- **WHEN** a caller invokes any CRUD/command wrapper (`InsertOne`, `Find`, `UpdateOne`, `DeleteOne`, `ReplaceOne`, `Aggregate`, `CountDocuments`, `Distinct`, `BulkWrite`, etc.)
- **THEN** the emitted span SHALL have `SpanKind == CLIENT`

#### Scenario: Change-stream read uses CLIENT

- **WHEN** a caller reads from a change stream (`Watch` and subsequent cursor advance/decode)
- **THEN** the emitted operation span SHALL have `SpanKind == CLIENT`
- **AND** it SHALL NOT use `CONSUMER` or `PRODUCER`

#### Scenario: Cursor decode uses INTERNAL

- **WHEN** the cursor performs a local decode with no round trip
- **THEN** the emitted span SHALL have `SpanKind == INTERNAL`

### Requirement: MongoDB span attribute set

Operation spans SHALL carry only OTel database-semconv attributes: `db.system.name = "mongodb"`, `db.collection.name`, `db.operation.name`, `db.namespace` (when known), `db.operation.batch.size` (only when batch ≥ 2), and `server.address` / `server.port` (port omitted when the default 27017). On error, `db.response.status_code` and `error.type` SHALL be set. No deliver-only attribute set SHALL be produced.

#### Scenario: Successful insert attributes

- **WHEN** `InsertMany` succeeds with 5 documents against `mydb.things` on `mongodb://host:27017`
- **THEN** the span SHALL carry `db.system.name=mongodb`, `db.collection.name=things`, `db.operation.name=insert`, `db.namespace=mydb`, `db.operation.batch.size=5`, `server.address=host`
- **AND** `server.port` SHALL be omitted (default 27017)

#### Scenario: Error attributes on write failure

- **WHEN** a write returns a `mongo.WriteException` with code 11000
- **THEN** the span status SHALL be `Error` and it SHALL carry `db.response.status_code=11000` and `error.type=11000`

### Requirement: Disabled tracing emits no spans or SDK objects

When the tracing gate is off (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and `OTEL_MONGO_TRACING_ENABLED` are not both truthy), the wrapper SHALL delegate to the native driver and run no OTel SDK code path — no real-tracer `Start`, no `TracerProvider`, no exporter, no `_oteltrace` inject/extract — consistent with the module-wide disabled-mode invariant. This applies to both the strategy-split path (`internal/direct/*`, which imports no `otel/sdk` or `otel/exporters`) and the cached-gate Client/Database. Removing the deliver `TracerProvider` shrinks this disabled surface (its init is gone, not merely gated off).

#### Scenario: Tracing disabled delegates to native driver

- **WHEN** the tracing gate is off and a caller invokes `InsertOne`, `Find`, `Aggregate`, or `Watch` (v1 or v2)
- **THEN** the wrapper SHALL delegate to the native `*mongo.Collection` via the `internal/direct` impl
- **AND** no span SHALL be emitted
- **AND** no `TracerProvider`, `BatchSpanProcessor`, or OTLP exporter SHALL be constructed
- **AND** no `_oteltrace` field SHALL be injected or stripped
