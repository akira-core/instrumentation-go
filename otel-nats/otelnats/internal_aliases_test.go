package otelnats

// Test-only exports for external _test packages (e.g. oteljetstream_test) so
// they can reset cached gates after manipulating env vars with t.Setenv.
// Hidden behind `_test.go` so they do not leak into the public surface.

// ResetGatesForTest resets natsGate and natsPropagationGate so the next read
// re-evaluates the env vars. Not parallel-safe; tests using this MUST NOT
// call t.Parallel.
func ResetGatesForTest() {
	natsGate.ResetForTest()
	natsPropagationGate.ResetForTest()
}
