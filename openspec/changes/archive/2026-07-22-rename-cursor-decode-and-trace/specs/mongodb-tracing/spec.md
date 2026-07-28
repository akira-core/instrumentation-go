## MODIFIED Requirements

### Requirement: Cursor decode with trace linking
`Cursor.DecodeAndTrace(ctx, v)` SHALL emit a `mongo.cursor.decode` INTERNAL span on a new, detached trace, and SHALL add a span link to the origin span when the decoded document's `_oteltrace` metadata is present and propagation is enabled. Plain `Cursor.Decode` SHALL behave exactly like the underlying driver's `Decode` and SHALL ignore `_oteltrace`.

#### Scenario: Change-stream document with trace metadata
- **WHEN** `DecodeAndTrace` decodes a document containing `_oteltrace` and propagation is enabled
- **THEN** a `mongo.cursor.decode` span is created with a link to the document's origin span

#### Scenario: Plain Decode ignores trace metadata
- **WHEN** `Cursor.Decode` is called on the same document
- **THEN** the call behaves identically to the underlying driver and does not create a span or read `_oteltrace`
