package otelmongo

// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
const ScopeName = "instrumentation-go/otel-mongo/v2"

// The v2 module's version line is v2.x.y (its module path ends in the /v2
// major-version suffix, so Go requires major 2); v2.MINOR.PATCH tracks the
// sibling modules' 0.MINOR.PATCH — see VERSIONING.md.
const instrumentationVersion = "2.7.0"

// Version returns the instrumentation module version (OTel contrib guideline).
func Version() string {
	return instrumentationVersion
}
