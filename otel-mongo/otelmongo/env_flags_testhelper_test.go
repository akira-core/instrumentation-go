package otelmongo

// resetPropEnabledCacheForTest re-arms the module's flag resolver so the next
// mongoTracingEnabled() / cachedPropagationEnabled() call re-evaluates instead
// of returning a snapshot cached by an earlier test. Test-only; not exported.
//
// Callers must invoke this AFTER t.Setenv changes any of the three tracing env
// vars, and after installing or removing an OpenFeature provider — otherwise the
// value cached by a prior test leaks. Rebuilding the resolver (rather than
// resetting one in place) also drops the lazily-created OpenFeature client, so
// the next evaluation binds to whatever provider is installed now.
//
// Not parallel-safe: the resolver is a package-level variable, so tests that use
// this MUST NOT call t.Parallel.
func resetPropEnabledCacheForTest() {
	mongoResolver = newMongoResolver()
}
