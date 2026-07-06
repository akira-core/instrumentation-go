// Package otelmongo provides OpenTelemetry instrumentation for the MongoDB
// Go driver (v1). It mirrors the API of go.mongodb.org/mongo-driver/mongo and
// wraps Client / Database / Collection / Cursor / SingleResult / ChangeStream
// with span creation and W3C Trace Context propagation via the `_oteltrace`
// document field.
//
// Feature flags are default-disabled. With OTEL_INSTRUMENTATION_GO_TRACING_ENABLED
// off, every wrapper is constructed with its disabled-mode impl (zero OTel SDK
// code paths). With both global and OTEL_MONGO_TRACING_ENABLED on, wrapper
// spans are emitted; with OTEL_MONGO_PROPAGATION_ENABLED additionally on, the
// `_oteltrace` field is injected on writes and extracted on reads.
//
// The package does NOT initialise a TracerProvider. Set the global provider
// and propagator at process startup via go.opentelemetry.io/otel.
package otelmongo
