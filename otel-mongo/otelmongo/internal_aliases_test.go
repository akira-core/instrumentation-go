package otelmongo

import "github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/shared"

// Test-only aliases for shared helpers used by parent-package _test.go files.
// Keeps existing call sites compiling after helpers moved to internal/shared/.
var (
	extractMetadataFromRaw  = shared.ExtractMetadataFromRaw
	injectTraceIntoDocument = shared.InjectTraceIntoDocument
	injectTraceIntoUpdate   = shared.InjectTraceIntoUpdate
)
