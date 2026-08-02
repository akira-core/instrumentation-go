// Package flags provides shared feature-flag helpers used by the
// instrumentation modules. The file contents (excluding the package declaration)
// MUST be byte-identical across every module copy of this package — drift is
// caught by CI.
//
// Exported primitives:
//
//   - EnvEnabled reads a single env var with default-off semantics.
//   - EnvGlobalTracing / GlobalTracingPossible name and read the process-wide
//     kill switch (the only OTEL_* name allowed in this shared file).
//   - Resolver resolves a module's flags through the process-global OpenFeature
//     client, using each flag's env var as the OpenFeature default value, and
//     caches the results in an immutable per-module snapshot with a one-second
//     TTL.
//
// The Resolver refresh, fallback and TTL logic below is the highest-drift-risk
// code in this package: it decides, for every module, whether a relay value or
// an env var wins. Any edit here MUST be replayed verbatim into the other three
// copies.
//
// This package never installs an OpenFeature provider, never sets an evaluation
// context and never shuts the SDK down — exactly as the instrumentation packages
// never initialize a TracerProvider. Provider lifecycle belongs to the
// application; the library only reads.
//
// Each module composes one Resolver at package init, supplying its own flag keys
// and env var names (which cannot live in this shared file), and reads it at each
// decision point. The global switch is read via GlobalTracingPossible and ANDed
// ahead of the Resolver, never expressed as a Spec: it is an out-of-band kill
// switch with no relay counterpart, and while it is off no OpenFeature code path
// runs at all.
package flags

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
)

// refreshTTL bounds how stale a Resolver snapshot may be. It is deliberately not
// configurable: the relay proxy's own polling interval (minutes) dominates
// end-to-end latency, so tightening this cannot meaningfully improve
// responsiveness and loosening it saves nanoseconds.
const refreshTTL = time.Second

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

// EnvGlobalTracing is the process-wide kill-switch environment variable.
// It is the only OTEL_* name allowed in this shared file; module-scoped
// env vars and OpenFeature keys live in each module's env_flags.go.
const EnvGlobalTracing = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"

// GlobalTracingPossible reports whether this process may ever run instrumented
// paths or negotiate otel-ws. It reads EnvGlobalTracing only (never OpenFeature).
func GlobalTracingPossible() bool {
	return EnvEnabled(EnvGlobalTracing)
}

// Spec identifies one dynamic flag: the OpenFeature key to evaluate and the
// environment variable whose value is passed as the evaluation default. Because
// this file must stay byte-identical across modules, it names no key and no env
// var itself — each module supplies its own Specs from its own env_flags.go.
type Spec struct {
	// Key is the OpenFeature flag key looked up on the provider.
	Key string
	// EnvVar is the environment variable read with EnvEnabled and passed as the
	// OpenFeature default value, so it decides whenever the provider has no
	// usable opinion.
	EnvVar string
}

// snapshot is an immutable view of every Spec's resolved value at one instant.
// Resolving a module's flags together means one clock read covers all of them
// and no torn read can produce a combination that never existed on the relay.
type snapshot struct {
	at     time.Time
	values []bool
}

// Resolver resolves one module's Specs through the process-global OpenFeature
// client and caches them for refreshTTL.
//
// Reads are cheap by construction: Enabled loads an atomic pointer, compares one
// timestamp and indexes a slice. The OpenFeature evaluation pipeline is entered
// at most once per TTL window per module, never per operation.
type Resolver struct {
	domain string
	specs  []Spec
	ttl    time.Duration
	now    func() time.Time

	clientOnce sync.Once
	client     openfeature.IClient

	snap atomic.Pointer[snapshot]
}

// ResolverOption configures a Resolver at construction.
type ResolverOption func(*Resolver)

// WithSpecs sets the flags this Resolver resolves. The index of each Spec is the
// index callers pass to Enabled.
func WithSpecs(specs ...Spec) ResolverOption {
	return func(r *Resolver) {
		r.specs = specs
	}
}

// WithClock replaces the Resolver's time source. Test-only: it exists so tests
// can step deterministically across the TTL boundary instead of sleeping, which
// also makes the TTL behavior itself testable rather than merely bypassable.
func WithClock(now func() time.Time) ResolverOption {
	return func(r *Resolver) {
		r.now = now
	}
}

// NewResolver returns a Resolver for one module. domain is the OpenFeature client
// domain (the module name), which lets an application scope a provider to a
// single module with openfeature.SetNamedProvider.
//
// No OpenFeature client is created here — construction happens lazily on the
// first refresh, so a process whose global kill switch is off never touches the
// OpenFeature SDK at all.
func NewResolver(domain string, opts ...ResolverOption) *Resolver {
	r := &Resolver{
		domain: domain,
		ttl:    refreshTTL,
		now:    time.Now,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(r)
	}
	return r
}

// Enabled returns the cached value of the Spec at index i, refreshing the
// snapshot first if it is absent or older than the TTL. An out-of-range index
// returns false rather than panicking, so a mis-wired module degrades to the
// disabled path instead of taking the process down.
func (r *Resolver) Enabled(i int) bool {
	s := r.snap.Load()
	if s == nil || r.now().Sub(s.at) >= r.ttl {
		s = r.refresh()
	}
	if i < 0 || i >= len(s.values) {
		return false
	}
	return s.values[i]
}

// refresh evaluates every Spec and stores a fresh snapshot.
//
// Concurrent refreshes are deliberately not serialized: the last store wins,
// and a lock on this path would cost more than the duplicate work it prevents.
// The snapshot timestamp is taken at the start of evaluation (not after) so a
// slower refresh that observed older relay values cannot stamp a newer
// completion time and keep stale values marked fresh for a full TTL.
func (r *Resolver) refresh() *snapshot {
	at := r.now()
	ctx := context.Background()
	client := r.evaluator()

	values := make([]bool, len(r.specs))
	for i, spec := range r.specs {
		// Client.Boolean returns the supplied default on every failure path — no
		// provider installed, provider not ready, flag absent from the relay
		// configuration, evaluation error, type mismatch. One call therefore
		// expresses the whole fallback policy: the relay decides when it has an
		// opinion, otherwise the environment does.
		values[i] = client.Boolean(ctx, spec.Key, EnvEnabled(spec.EnvVar), openfeature.EvaluationContext{})
	}

	s := &snapshot{at: at, values: values}
	r.snap.Store(s)
	return s
}

// evaluator lazily creates the domain-scoped OpenFeature client. The client
// resolves the installed provider per evaluation, so creating it before the
// application installs one is safe.
func (r *Resolver) evaluator() openfeature.IClient {
	r.clientOnce.Do(func() {
		r.client = openfeature.NewClient(r.domain)
	})
	return r.client
}
