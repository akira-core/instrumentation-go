package otelmongo

import "testing"

// TestVersion pins the reported instrumentation-scope version (see the
// instrumentation-scope-metadata capability): Version() feeds
// trace.WithInstrumentationVersion on every tracer, so a silent drift here
// changes otel.scope.version on every emitted span.
func TestVersion(t *testing.T) {
	if got := Version(); got != "0.7.0" {
		t.Errorf("Version() = %q, want %q", got, "0.7.0")
	}
}
