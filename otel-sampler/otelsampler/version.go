package otelsampler

// instrumentationVersion is this module's released version. It is validated
// against the pushed release tag by .github/workflows/release-guard.yml — see
// VERSIONING.md. Bump it in the same commit as the release's code changes.
const instrumentationVersion = "0.2.0"

// Version returns the otel-sampler module version.
//
// Unlike the instrumentation wrappers, this module emits no spans of its own,
// so the value is not reported as an instrumentation-scope version. It exists
// so applications can record which sampler build they are running and so the
// release guard has a constant to check the tag against.
func Version() string { return instrumentationVersion }
