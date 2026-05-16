package otelnats

// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
const ScopeName = "instrumentation-go/otel-nats/otelnats"

const instrumentationVersion = "0.4.1"

// Version returns the instrumentation module version (OTel contrib guideline).
func Version() string {
	return instrumentationVersion
}
