package otelmongo

// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
const ScopeName = "instrumentation-go/otel-mongo/v2"

const instrumentationVersion = "0.4.2"

// Version returns the instrumentation module version (OTel contrib guideline).
func Version() string {
	return instrumentationVersion
}
