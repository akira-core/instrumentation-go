// Package traced holds the enabled-path implementations of Cursor,
// SingleResult, and ChangeStream. Constructed only when the
// OTEL_INSTRUMENTATION_GO_TRACING_ENABLED + OTEL_MONGO_TRACING_ENABLED gates
// are both on. Imports the OTel SDK and shared helpers.
package traced
