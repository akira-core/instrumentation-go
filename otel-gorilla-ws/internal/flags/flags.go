// Package flags provides shared feature-flag gate helpers used by the
// instrumentation modules. The file contents (excluding the package declaration)
// MUST be byte-identical across every module copy of this package — drift is
// caught by CI.
//
// Two primitives are exported:
//
//   - EnvEnabled reads a single env var with default-off semantics.
//   - Gate caches the result of a resolver function across the process
//     lifetime using sync.Once + atomic.Bool, with a test-only reset hook.
//
// Each module composes its own gate(s) at package init by calling NewGate with
// a resolver that ANDs the relevant EnvEnabled calls. Hot paths call
// Gate.Enabled once in the constructor; under the strategy-split layout the
// impl is then picked once and the gate is never read again.
package flags

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// EnvEnabled reports whether the named environment variable is set to a
// truthy value. Default-off: an unset variable returns false. Falsy values
// (case-insensitive, whitespace-trimmed) are "0", "false", "no", "off".
// Any other set value is treated as truthy.
func EnvEnabled(name string) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Gate caches the result of a resolver function across the process lifetime.
// The resolver is invoked at most once per Gate (until ResetForTest is called).
//
// Production callers should construct one Gate per logical flag at package
// init, then call Enabled once per wrapper construction. ResetForTest exists
// for unit tests that toggle env vars with t.Setenv; it is NOT parallel-safe
// and MUST NOT be called from production code paths.
type Gate struct {
	once sync.Once
	flag atomic.Bool
	fn   func() bool
}

// NewGate returns a Gate that will lazily evaluate fn on the first call to
// Enabled. fn is typically `func() bool { return EnvEnabled(global) && EnvEnabled(module) }`.
func NewGate(fn func() bool) *Gate {
	return &Gate{fn: fn}
}

// Enabled returns the cached value of the resolver. The resolver is invoked
// exactly once per Gate; all subsequent calls return the cached boolean.
//
// WARNING: env changes after the first call are ignored. OTel instrumentation
// env vars are expected to be set at process startup.
func (g *Gate) Enabled() bool {
	g.once.Do(func() {
		g.flag.Store(g.fn())
	})
	return g.flag.Load()
}

// ResetForTest clears the cached value and the sync.Once so the next call to
// Enabled re-invokes the resolver. Tests that toggle env vars with t.Setenv
// MUST call this after the Setenv and SHOULD register t.Cleanup to reset
// again at test teardown so cached state does not leak into sibling tests.
//
// Not parallel-safe. Tests that use this MUST NOT call t.Parallel.
func (g *Gate) ResetForTest() {
	g.once = sync.Once{}
	g.flag.Store(false)
}
