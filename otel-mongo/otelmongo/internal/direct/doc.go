// Package direct holds the disabled-path passthrough implementations of
// Cursor, SingleResult, and ChangeStream. Constructed only when the tracing
// gate is off.
//
// MUST NOT import go.opentelemetry.io/otel/* — see STRATEGY_REFACTOR_PLAN §8.
// The lack of OTel SDK imports is what makes the disabled-mode invariant
// structurally provable.
package direct
