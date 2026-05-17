package otelnats

// Helpers exported for tests in **sibling packages** that need to manipulate
// otelnats's cached gates after running `t.Setenv`. Lives in a non-`_test.go`
// file so `oteljetstream_test` (a different package's test binary) can call it;
// `_test.go` symbols are scoped to a single test binary in Go.
//
// Not part of the production public API. Production callers MUST NOT invoke
// these helpers — env changes after the first gate read are intentionally
// ignored at runtime (the gate is sync.Once-cached for process lifetime).

// ResetGatesForTest resets natsGate and natsPropagationGate so the next
// Enabled() call re-evaluates the env vars. Not parallel-safe; tests using
// this MUST NOT call t.Parallel.
func ResetGatesForTest() {
	natsGate.ResetForTest()
	natsPropagationGate.ResetForTest()
}
