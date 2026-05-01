package otelmongo

// ScopeName is the instrumentation scope name for Tracer creation (OTel contrib guideline).
const ScopeName = "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"

const instrumentationVersion = "0.2.11"

// Version returns the instrumentation module version (OTel contrib guideline).
func Version() string {
	return instrumentationVersion
}
