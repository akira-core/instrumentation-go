package otelflags

// instrumentationVersion is this module's released version.
//
// This module emits no spans and is not an instrumentation scope; the constant
// exists so the release-tag CI guard can validate a pushed `otel-flags/vX.Y.Z`
// tag against the tree, on the same terms as every other released module.
const instrumentationVersion = "0.2.0"

// Version reports the module version. It exists so the constant has a reader
// outside the release guard, and so a caller can record which build of the flag
// layer a process is running.
func Version() string { return instrumentationVersion }
