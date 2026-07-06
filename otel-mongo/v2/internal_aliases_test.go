package otelmongo

import "github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"

// Test-only aliases for shared helpers used by parent-package _test.go files.
var (
	extractMetadataFromRaw  = shared.ExtractMetadataFromRaw
	injectTraceIntoDocument = shared.InjectTraceIntoDocument
	injectTraceIntoUpdate   = shared.InjectTraceIntoUpdate
)
