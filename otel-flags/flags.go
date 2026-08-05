// Package otelflags resolves the feature switches that govern this repository's
// OpenTelemetry instrumentation modules.
//
// # The precedence ladder
//
// Every switch is resolved down four rungs, first source with an opinion
// winning:
//
//	relay  >  env  >  option (With*Enabled)  >  hardcoded default
//
// The ordering is by how late in the pipeline each source is decided — compiled
// in, written when the wrapper is constructed, set when the process is deployed,
// changed while it runs — so each later stage overrides the earlier ones. That
// is why the per-connection option sits BELOW its environment variable: a
// deployment must be able to disable one module without silencing the process
// and without a relay, even when the application's Go code asked for it. The
// case that forces the order is otel-mongo's document propagation, which appends
// a permanent field to the operator's own documents.
//
// The whole ladder is one call. Client.Boolean returns the value passed to it on
// every path where the relay has no usable answer — no provider installed, not
// ready, key absent, evaluation error, type mismatch — so this package hands it
// the already-resolved local value and lets the SDK perform the fallback. Relay
// silence and relay failure are deliberately indistinguishable: both mean "the
// next rung down decides".
//
// # What lives here and what does not
//
// Every name this file defines is PROCESS-scoped: the master switch, the three
// provider variables, the service-name attribute, the OpenFeature domain. Module
// flag keys, module environment variable names and module defaults belong to the
// module that owns them and reach this package only through WithFlagKeys and the
// local parameter of Value. Adding an instrumentation module must not require a
// change here.
//
// # The single-provider guarantee
//
// This package exists as one published module rather than four vendored copies
// because four packages sharing no state cannot guarantee a single provider:
// two of them can observe "nothing installed" concurrently and both register
// one. Go resolves one module path to one version per build, so there is one
// instance of the installMu/installDone latch below, which every path that binds
// FlagDomain goes through — the auto-install, its retries, and an application's
// own InstallProvider — and therefore exactly one install.
//
// # What this package will not touch
//
// It never calls SetProvider, SetEvaluationContext, AddHooks or Shutdown — the
// same rule the instrumentation packages follow for TracerProvider. The one
// piece of OpenFeature state it may write is a NAMED provider bound to
// FlagDomain, and only when the environment asks for one and the application
// installed none. Nothing it does can change how the application's own feature
// flags resolve.
//
// Nothing here is cached. Value evaluates on every call, so a relay change is
// observed on the next operation; the end-to-end delay is the provider's poll
// interval, which this package lengthens by at most a tenth — see
// jitterInterval — and nothing else.
package otelflags

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
	"github.com/open-feature/go-sdk/openfeature"
)

const (
	// EnvGlobalTracing is the process-wide master switch.
	//
	// It defaults to enabled, which makes it a veto rather than an enabler: the
	// only value with an effect is a falsy one, and setting it truthy changes
	// nothing. What it buys is a single variable that stops every module in the
	// process, including connections whose Go code passed an option.
	EnvGlobalTracing = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"

	// EnvFlagsEndpoint is the GO Feature Flag relay proxy URL. Setting it is an
	// operator's request for relay control; leaving it unset means no provider is
	// ever constructed, no OpenFeature state is written, and RelayPossible
	// reports false.
	EnvFlagsEndpoint = "OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT"

	// EnvFlagsAPIKey authenticates against a relay proxy that requires it. Its
	// value is never logged.
	EnvFlagsAPIKey = "OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY"

	// EnvFlagsPollInterval overrides how often the provider polls the relay.
	// Go duration strings only: a bare integer is rejected rather than read as
	// milliseconds, because misreading a polling interval that way is
	// catastrophic rather than merely wrong. The value configured here is
	// jittered by at most plus or minus a tenth for the life of the process —
	// see jitterInterval — so it sets the centre of the polling period, not an
	// exact one.
	EnvFlagsPollInterval = "OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL"

	// EnvServiceName is the OpenTelemetry-specified service name. It is the only
	// source of targeting attributes this package uses, and only on the
	// auto-install path.
	EnvServiceName = "OTEL_SERVICE_NAME"
)

// FlagKeyGlobalTracing is the master switch's relay key.
//
// Its evaluation default is the master switch's local value, which defaults to
// true, so setting this key to true on a relay has no effect at all. The only
// useful value is false, which stops every module in every process the relay
// serves. Documentation must describe it that way; presented as an enable it
// will read as a broken flag.
const FlagKeyGlobalTracing = "otel-instrumentation-go-tracing"

// FlagDomain is the single OpenFeature domain every module resolves through.
//
// One domain rather than one per module is forced by the provider: the
// in-process evaluator's Init is not idempotent, so registering one instance
// under N domains starts N polling goroutines of which N−1 can never be stopped,
// and N separate instances would poll the relay N times over identical
// configuration.
//
// Exported because module-package tests install their in-memory provider on it.
const FlagDomain = "otel-instrumentation-go"

// defaultPollInterval is how often the auto-installed provider polls the relay,
// and therefore — once jitterInterval has widened it by at most a tenth — the
// upper bound on how long a flag change takes to reach a running process. It is
// set explicitly because the provider's own default is
// two minutes — too slow for a control plane whose job includes stopping an
// incident — and because that default applies whenever the configured interval
// is non-positive.
const defaultPollInterval = 60 * time.Second

// noopProviderName is what the OpenFeature SDK's built-in no-op provider
// reports. It is the reliable test for "the application has installed no
// provider", used by both RelayPossible and the auto-install condition.
const noopProviderName = "NoopProvider"

// defaultMasterTracing is the master switch's value when nothing sets it. See
// EnvGlobalTracing for why it is true.
const defaultMasterTracing = true

// ErrInvalidFlagValue reports an environment variable set to something this
// package cannot interpret as a boolean.
//
// One sentinel serves every module. That is possible only because this package
// is published rather than internal/, and it is why the per-module
// configuration-conflict sentinels an earlier design needed are gone.
var ErrInvalidFlagValue = errors.New("otel-flags: invalid boolean value")

// truthy and falsy are the complete accepted vocabularies, mirrored one for one
// so the documented rule stays symmetric and short.
var (
	truthy = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
	falsy  = map[string]bool{"0": true, "false": true, "no": true, "off": true}
)

// Lookup reads one environment variable as a tri-state.
//
// Three outcomes, and only three:
//
//   - unset          → (false, false, nil): this source has no opinion, and
//     resolution falls through to the next rung down.
//   - recognised     → (value, true, nil): this source decides.
//   - anything else  → a non-nil error wrapping ErrInvalidFlagValue.
//
// The third case is an error rather than a warning-and-a-guess because under a
// precedence ladder there is no safe direction to guess in. The master tier
// defaults to true and every other tier defaults to false, so a value silently
// read as false would stop a whole fleet on one tier and change nothing on the
// others — the same input meaning two different things, with a log line as the
// only evidence.
//
// The empty string is invalid for the same reason. `export VAR=` reads as "set,
// to nothing", and both available readings are wrong somewhere: as false it lets
// an unexpanded ${SOMETHING} template variable express an opinion the deployment
// never had, and as unset it silently reverses meaning for anyone who used it as
// an off switch. Failing makes the ambiguity visible at the only moment anyone
// can act on it. The rule has no exceptions: set it to a recognised value, or do
// not set it.
//
// The error names the variable and the observed value, so the fix needs no
// documentation lookup.
func Lookup(name string) (value bool, set bool, err error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false, false, nil
	}
	switch normalized := strings.ToLower(strings.TrimSpace(v)); {
	case truthy[normalized]:
		return true, true, nil
	case falsy[normalized]:
		return false, true, nil
	default:
		return false, true, fmt.Errorf(
			"%w: %s=%q (accepted: 1,true,yes,on / 0,false,no,off)",
			ErrInvalidFlagValue, name, v)
	}
}

// ResolveLocal resolves the three rungs below the relay — env > option >
// default — into the single value Value takes as its evaluation default.
//
// The environment variable is applied LAST because it outranks the option. That
// ordering is the operator's per-module control: a deployment can disable one
// module without silencing the process and without a relay, even when the
// application's Go code asked for it. The case that forces it is otel-mongo's
// document propagation, which appends a permanent field to the operator's own
// documents.
//
// A Lookup error is returned even when an option was supplied. The option does
// not excuse an unreadable variable that outranks it, and a caller cannot know
// from the option alone what the deployment meant.
//
// envName is a parameter rather than a constant here so this package still names
// no module. Modules own their variable names, their flag keys and their
// defaults; this package owns the ladder.
func ResolveLocal(option *bool, envName string, def bool) (bool, error) {
	local := def
	if option != nil {
		local = *option
	}
	v, set, err := Lookup(envName)
	if err != nil {
		return false, err
	}
	if set {
		local = v
	}
	return local, nil
}

// MasterLocal resolves the master switch from everything the relay cannot
// change: the environment variable, else the default of true.
//
// There is no option parameter, and there must not be one. The master switch is
// process-scoped and an option is per-connection, so an option supplying it
// would give each connection its own "process-wide" switch — and would leave no
// single setting able to stop a process whose Go code hardcodes an opinion.
func MasterLocal() (bool, error) {
	return ResolveLocal(nil, EnvGlobalTracing, defaultMasterTracing)
}

// MasterEnabled resolves the master switch for one operation, given the local
// value MasterLocal returned at construction.
//
// It is resolved per operation like every other relay-backed switch. Resolving
// it once at construction would mean a relay veto reached only connections
// created afterwards, which is the opposite of what a veto is for.
func MasterEnabled(local bool) bool { return masterResolver.Value(0, local) }

// masterResolver resolves the master switch's relay key. It shares the
// process-wide provider install with every module resolver.
var masterResolver = NewResolver(WithFlagKeys(FlagKeyGlobalTracing))

// InstallProvider binds provider to FlagDomain and records that this process
// deliberately gave the instrumentation switches a relay.
//
// It is the recommended way for an application to install its own provider. The
// raw openfeature.SetNamedProviderAndWait(FlagDomain, p) still works and is
// still detected, but only through the heuristic in providerBound, which cannot
// tell an explicit binding from a fallback when the same provider is bound to
// both the default slot and this domain. Going through here removes that one
// blind spot: the record is exact.
//
// It installs with SetNamedProviderAndWait rather than the asynchronous form, so
// when it returns the provider has finished initialising and no startup window
// remains. Call it BEFORE constructing any wrapper — see RelayPossible.
//
// It writes nothing but the named binding on FlagDomain. The default provider,
// the global evaluation context, hooks and shutdown all remain the
// application's.
// It takes installMu, which is what makes it safe to call concurrently with the
// first instrumented operation: without it, this and the environment
// auto-install were two unsynchronised read-then-bind sequences on the same
// domain — the very race installMu exists to prevent, reachable through the one
// entry point that skipped it. It also latches installDone, so an application
// that installs its own provider is never followed by an auto-install, whichever
// of the two the process reaches first.
//
// Holding installMu across the wait does block a wrapper being constructed
// concurrently on another goroutine, for as long as the provider takes to
// initialise. That is the correct outcome and not merely an acceptable one: the
// wrapper would otherwise resolve its first operations against a provider this
// call is in the middle of replacing.
func InstallProvider(provider openfeature.FeatureProvider) error {
	if provider == nil {
		return errors.New("otel-flags: InstallProvider: provider is nil")
	}

	installMu.Lock()
	defer installMu.Unlock()

	if err := openfeature.SetNamedProviderAndWait(FlagDomain, provider); err != nil {
		return fmt.Errorf("otel-flags: binding a provider to %q: %w", FlagDomain, err)
	}
	explicitBind.Store(true)
	autoInstalled.Store(false)
	installDone = true

	return nil
}

// explicitBind records an InstallProvider call. It is never cleared outside
// tests: an application that installs a provider and later rebinds still has one
// bound, and unbinding a domain is not something the OpenFeature SDK offers.
var explicitBind atomic.Bool

// providerBound reports whether a provider is bound to FlagDomain specifically.
//
// The subtlety is that NamedProviderMetadata does NOT answer that question.
// Reading the SDK (go-sdk v1.17.2, openfeature_api.go):
//
//	provider, ok := api.namedProviders[name]
//	if !ok {
//		return ProviderMetadata() // ← the DEFAULT provider's metadata
//	}
//
// so an application that installed a business provider with SetProvider — for
// its own flags, with no relation to this library — reads back as if it had
// bound one here. Believing that had two consequences, both wrong and both
// silent: every wrapper built the instrumented implementation and evaluated
// instrumentation keys against the application's business provider on every
// operation, and installProviderFromEnv stood down, so an operator who had set
// EnvFlagsEndpoint got no relay at all.
//
// So the fallback is stripped: an answer that merely echoes the default provider
// is not a binding. What survives is one false negative — an application that
// binds the SAME provider to both slots reads equal and is treated as unbound —
// which InstallProvider closes for anyone who wants it closed.
// The two SDK reads take the same lock separately, so they can straddle a
// SetProvider call: the named read would return the OLD default through the
// fallback and the default read the NEW one, two different names, neither of
// them NoopProvider — a binding that does not exist. So the default is read
// twice around the named read and the answer is only trusted when it did not
// move, which is the same trick a seqlock plays for the same reason.
func providerBound() bool {
	if explicitBind.Load() {
		return true
	}
	for range providerReadAttempts {
		before := openfeature.ProviderMetadata()
		named := openfeature.NamedProviderMetadata(FlagDomain)
		after := openfeature.ProviderMetadata()
		if before.Name == after.Name {
			return boundToDomain(named, after)
		}
	}
	// The application is replacing its default provider faster than three reads.
	// Answer "bound", which stands the auto-install down: leaving an operator's
	// endpoint inert is recoverable by a restart, and replacing a provider the
	// application may have just bound to this domain is not.
	return true
}

// providerReadAttempts is how many times providerBound re-reads before giving
// up. Three is one more than the number needed to survive a single concurrent
// swap; a process that fails all three is swapping its default provider in a
// loop, which is not a state this package can usefully wait out.
const providerReadAttempts = 3

// boundToDomain holds the comparison itself, separately from the two SDK calls
// that feed it, because the case that matters most cannot be built through the
// SDK from a test: once anything binds FlagDomain there is no way to unbind it,
// and the fallback this exists to defeat only fires while the domain is unbound.
func boundToDomain(named, def openfeature.Metadata) bool {
	if named.Name == noopProviderName {
		return false
	}
	return named.Name != def.Name
}

// RelayPossible reports whether a relay could ever have an opinion in this
// process: an endpoint is configured, or a provider is bound to FlagDomain.
//
// When it is false, Client.Boolean can only ever return the value passed to it,
// so the relay is not merely silent — it is structurally incapable of speaking.
// Callers use that to keep the pre-dynamic zero-cost path: resolve from
// env > option > default alone, allocate the instrumented implementation only if
// that answer is on, and never touch the OpenFeature SDK.
//
// A provider the application installed for its OWN flags does not count; see
// providerBound.
//
// Callers MUST resolve it once per construction and MUST NOT memoize it
// process-wide. A package-level sync.Once would be cheaper and would guarantee
// that every wrapper agrees, but it would freeze the answer at whichever wrapper
// was built first — which in a test binary is whichever test ran first, making
// every subsequent relay test unreachable without a reset hook this design does
// not have.
//
// The consequence for applications is an ordering rule: install your own
// provider BEFORE constructing any wrapper. One built earlier resolves
// statically for the rest of its life.
func RelayPossible() bool {
	if strings.TrimSpace(os.Getenv(EnvFlagsEndpoint)) != "" {
		return true
	}
	return providerBound()
}

// Resolver resolves one module's flag keys through the OpenFeature client.
//
// It caches nothing. Value evaluates on every call, so there is no TTL, no
// snapshot timestamp, no clock to inject, and no cross-flag consistency window
// beyond the microseconds between two consecutive calls.
//
// The measured cost is roughly 2 µs and 7 allocations per call against 82 ns for
// an atomic snapshot read. That is the SDK's evaluation pipeline — hook chains,
// evaluation-context merging, the provider registry lock — not the flag lookup,
// and an in-memory provider does not make it cheaper. An instrumented operation
// makes two of these calls (three on a Mongo write), so the cost is real and is
// recorded rather than assumed. Caching remains a permitted optimisation: it
// fits entirely inside this type without changing Value's signature or any call
// site.
type Resolver struct {
	// keys are OpenFeature flag keys in Value-index order.
	keys []string

	clientOnce sync.Once
	client     openfeature.IClient

	// evalCtx is populated only when this process auto-installed the provider.
	// An application that installs its own owns its evaluation context outright,
	// and this stays zero so nothing it set can be overridden.
	evalCtx openfeature.EvaluationContext
}

// ResolverOption configures a Resolver at construction.
type ResolverOption func(*Resolver)

// WithFlagKeys sets the OpenFeature flag keys this Resolver resolves. Each key's
// index is the index callers pass to Value.
func WithFlagKeys(keys ...string) ResolverOption {
	return func(r *Resolver) { r.keys = keys }
}

// NewResolver returns a Resolver for one module.
//
// There is no domain parameter: the domain is process-scoped, so making it a
// parameter would only create a string that has to agree across every module
// with nothing checking it.
//
// No OpenFeature client is created here and no provider is installed. Both
// happen lazily on the first Value call, which a wrapper only reaches when a
// relay could exist — so a process without one never touches the OpenFeature SDK
// at all.
func NewResolver(opts ...ResolverOption) *Resolver {
	r := &Resolver{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(r)
	}
	return r
}

// Value returns the effective value of the key at index i, given the local value
// resolved from the option, the environment variable and the hardcoded default.
//
// This single call is the whole precedence ladder. local is passed as the
// evaluation default, so the relay's value wins when it has one and local stands
// on every other path.
//
// An out-of-range index returns false rather than panicking, so a mis-wired
// module degrades to the disabled path instead of taking the process down.
//
// Nothing is evaluated unless a provider is bound to FlagDomain. That guard
// belongs here rather than in each wrapper, and it is not an optimisation: the
// SDK's ForEvaluation falls back to the DEFAULT provider when a domain is
// unbound, so evaluating regardless would resolve instrumentation keys against
// whatever the application installed for its own feature flags — a network call
// per instrumented operation if that provider evaluates remotely, and a wrong
// answer outright if it happens to define a key by the same name. Every wrapper
// in this repository hand-rolls an equivalent short-circuit before calling here;
// this makes the module that owns the ladder enforce it too, for the wrapper
// that forgets and for the callers of the exported MasterEnabled.
func (r *Resolver) Value(i int, local bool) bool {
	if i < 0 || i >= len(r.keys) {
		return false
	}
	// The client is bound to a local before the call rather than inlined as its
	// receiver: evaluator() is what initialises r.evalCtx, and Go orders function
	// calls left to right only against each other, leaving a plain field load in
	// the argument list unordered against them. Inlined, the zero evaluation
	// context could reach the first evaluation of a Resolver.
	client := r.evaluator()
	if !providerBound() {
		return local
	}

	ctx, cancel := evaluationContext()
	defer cancel()
	return client.Boolean(ctx, r.keys[i], local, r.evalCtx)
}

// evaluationContext bounds one evaluation.
//
// The auto-installed provider evaluates in process, so it cannot block and the
// deadline is pure overhead — hence the fast path that skips building one. A
// provider the application installed through InstallProvider can be anything,
// including one that evaluates over HTTP, and that one sits on the hot path of
// every instrumented Mongo command and NATS publish: two evaluations per
// operation, three on a Mongo write. A stalled flag backend must cost a bounded
// amount and then fall through to the local value, which is what a timeout here
// produces — Boolean returns the passed-in default on a context error like any
// other failure.
//
// The caller's context is deliberately NOT threaded through. Cancelling a Mongo
// operation should not change what the instrumentation switch resolves to, and a
// caller's deadline is about their work, not about the control plane.
func evaluationContext() (context.Context, context.CancelFunc) {
	if autoInstalled.Load() {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), evaluationTimeout)
}

// evaluationTimeout is how long an evaluation may take before the local value
// decides. It is generous for a same-datacentre round trip and small next to any
// database or messaging operation this library instruments, which is the budget
// it is really spending.
const evaluationTimeout = 250 * time.Millisecond

// autoInstalled records that the provider bound to FlagDomain is the one this
// package built: in-process evaluation, no network on the evaluation path.
var autoInstalled atomic.Bool

// evaluator lazily installs the environment-configured provider, if any, and
// creates the domain-scoped OpenFeature client.
func (r *Resolver) evaluator() openfeature.IClient {
	r.clientOnce.Do(func() {
		r.evalCtx = withTargetingKey(installProvider())
		r.client = openfeature.NewClient(FlagDomain)
	})
	return r.client
}

// withTargetingKey adds this process's targeting key to an evaluation context.
//
// Without one, every relay rule that buckets — percentage and progressiveRollout,
// which is how a kill switch is canaried or ramped — fails with
// TARGETING_KEY_MISSING, and Client.Boolean turns that into the local value like
// any other failure. The rollout then appears to do nothing at all, on every
// process, with no diagnostic anywhere. So the key is supplied on every path,
// including the one where the application installed the provider itself: it
// applies to this package's own keys on its own domain, and an application that
// binds FlagDomain has opted into these switches.
//
// Unlike the service.name attribute, which stays confined to the auto-install
// path so it can never override one the application set.
func withTargetingKey(ctx openfeature.EvaluationContext) openfeature.EvaluationContext {
	return openfeature.NewEvaluationContext(processTargetingKey, ctx.Attributes())
}

// processTargetingKey identifies this process to the relay.
//
// Host plus PID rather than a random value, for two reasons. It buckets per
// process, which is what an operator canarying "enable tracing on 10% of the
// fleet" means — a key derived from the service name alone would make every
// percentage rollout all-or-nothing per service. And it is stable across a
// restart of the same container, so a process that lands in the canary stays
// there instead of re-drawing its verdict every time it restarts.
var processTargetingKey = func() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}()

// Provider-install state, guarded by installMu.
//
// This is package-level rather than per-Resolver, and that is the whole point of
// the module. A process holds one Resolver for the master switch and one per
// instrumentation module, each with its own sync.Once, which orders nothing
// between them. The sequence installProviderFromEnv performs — read whether a
// provider is bound, then bind one — is two separate critical sections inside the
// OpenFeature SDK: a read lock behind NamedProviderMetadata, a write lock in the
// named-provider setter, with no conditional setter offered that would fuse them.
// So two Resolvers can both observe NoopProvider and both register. One mutex
// here serialises the whole sequence across all of them, so "one shared module"
// becomes "exactly one install".
//
// What that race costs is not what it first appears. The SDK does shut the loser
// down — setNamedProviderWithContext hands the replaced provider to shutdownOld
// (go-sdk v1.17.2, openfeature_api.go). What it does not do is order that against
// the winner's initialisation: Init and Shutdown are dispatched to separate
// goroutines, and the in-process evaluator is not idempotent across that pair.
// Its Shutdown returns immediately when Init has not run yet, pollingDone being
// pre-closed for exactly that case, after which Init installs a fresh
// stopPolling channel and resets the shutdownOnce that just fired. The polling
// goroutine it then starts has no reachable handle and outlives every attempt to
// stop it, fetching from the relay for the life of the process.
//
// installEvalCtx is remembered rather than recomputed because a Resolver that
// initialises after the install would otherwise see a provider already bound,
// take the stand-down path, and evaluate with an empty evaluation context — so
// the service.name targeting attribute would reach one module and not the
// others.
var (
	installMu      sync.Mutex
	installDone    bool
	installEvalCtx openfeature.EvaluationContext
)

// installGen retires the retry goroutine below. It exists for the tests, which
// reset the install latch and rebind providers many times inside one process: a
// goroutine left over from an earlier test would otherwise wake up and rebind
// FlagDomain underneath a later one. Production bumps it never, so the goroutine
// runs until it succeeds or stands down.
var installGen atomic.Uint64

// providerRetryInitialDelay is how soon after a failed first fetch the retry
// below looks again, doubling to the poll interval from there. A relay that is
// briefly unavailable during a rollout is the common case and deserves a fast
// recovery; a relay that is down for an hour deserves one attempt per poll
// interval and no more.
const providerRetryInitialDelay = time.Second

func installProvider() openfeature.EvaluationContext {
	installMu.Lock()
	defer installMu.Unlock()
	if !installDone {
		installEvalCtx = installProviderFromEnv()
		installDone = true
	}
	return installEvalCtx
}

// installProviderFromEnv registers a GO Feature Flag provider on FlagDomain when
// the environment asks for one and the application installed none, and returns
// the evaluation context to use with it.
//
// Two conditions, both necessary. EnvFlagsEndpoint is the operator's expression
// of intent. The NoopProvider check is what makes this an allowance rather than
// a takeover: an application that installs its own provider before constructing
// any wrapper keeps it, and this returns without writing anything.
//
// Registration is non-blocking on purpose. Blocking would put a relay round trip
// in front of the first instrumented operation, and a control plane must not
// become a latency source. The window that leaves — every switch reads its local
// value until the provider's first fetch — is fail-safe: it can delay a
// relay-driven enable but can never introduce one, and for otel-mongo it can
// never write an _oteltrace field the deployment did not configure. An
// application that wants the relay's answer before its first operation installs
// its own provider with InstallProvider (or the raw
// SetNamedProviderAndWait(FlagDomain, p)), which also makes this stand down.
// SetProviderAndWait does NOT: it binds the default slot, which says nothing
// about the instrumentation switches — see providerBound.
//
// Because registration does not wait, it also cannot see whether initialisation
// succeeded, and a failure there is permanent rather than transient: the SDK
// discards the init result on the asynchronous path, and the provider's
// in-process evaluator returns from Init before it starts its ticker when the
// first fetch fails, so it never polls again and nothing retries it. A relay
// that was merely slow to come up during a rollout would leave the process with
// a bound provider that can never answer anything — the kill switch dead, with
// silence as the only signal. watchProviderInit is what closes that.
func installProviderFromEnv() openfeature.EvaluationContext {
	var empty openfeature.EvaluationContext

	endpoint := strings.TrimSpace(os.Getenv(EnvFlagsEndpoint))
	if endpoint == "" {
		return empty
	}
	// Stand down for a provider the application bound to THIS domain, and only
	// for that. A provider it installed as the default is its own business and
	// says nothing about the instrumentation switches — deferring to one used to
	// mean an operator who had set EnvFlagsEndpoint silently got no relay, while
	// every instrumentation key was evaluated against the application's business
	// provider. See providerBound.
	if providerBound() {
		return empty
	}

	// Read once, here, on the constructing goroutine. Passing the interval down
	// rather than re-reading it keeps the malformed-value warning to a single
	// line, and keeps every log this path emits on a goroutine the caller can
	// reason about — a retry goroutine that warned on its own would be writing to
	// whatever logger happened to be installed minutes later.
	interval := jitterInterval(pollIntervalFromEnv())

	provider, err := newRelayProvider(endpoint, interval)
	if err != nil {
		slog.Warn("feature flag provider unavailable; instrumentation switches cannot be changed remotely",
			"var", EnvFlagsEndpoint, "error", err)
		return empty
	}

	if err := openfeature.SetNamedProvider(FlagDomain, provider); err != nil {
		slog.Warn("feature flag provider registration failed; instrumentation switches cannot be changed remotely",
			"domain", FlagDomain, "error", err)
		return empty
	}

	autoInstalled.Store(true)
	go watchProviderInit(provider.Metadata().Name, endpoint, interval, installGen.Load())

	// Targeting attributes, supplied only here. Passed at the invocation site
	// rather than through SetEvaluationContext, so it composes with an
	// application's global context instead of replacing it — and confined to this
	// path, so it can never override a service.name the application set.
	//
	// Both spellings, and the dot-free one is the one a rule can actually use.
	// The SDK flattens an attribute key literally, so the semconv name arrives at
	// the relay as "service.name", but both supported query languages read a dot
	// as a nested-path separator: nikunjy's parser splits it into attribute
	// "service" with sub-attribute "name" and finds nothing, and JSONLogic's
	// {"var": "service.name"} resolves it as a path too. A rule written the
	// obvious way therefore matched no process at all. service.name stays because
	// it is the name a reader expects to see in an evaluation context, and
	// serviceName is what targeting rules key on.
	if svc := strings.TrimSpace(os.Getenv(EnvServiceName)); svc != "" {
		return openfeature.NewTargetlessEvaluationContext(map[string]any{
			"service.name": svc,
			"serviceName":  svc,
		})
	}
	return empty
}

// newRelayProvider builds the GO Feature Flag provider the auto-install binds.
//
// It is a function rather than an inline literal because a failed initialisation
// cannot be repaired in place: the in-process evaluator that returned an error
// from Init has no ticker and no way to be told to try again, so recovery means
// building a fresh provider. See watchProviderInit.
func newRelayProvider(endpoint string, interval time.Duration) (openfeature.FeatureProvider, error) {
	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:                  endpoint,
		APIKey:                    os.Getenv(EnvFlagsAPIKey),
		FlagChangePollingInterval: interval,

		// Both hardcoded, and deliberately not exposed as environment variables,
		// so the zero-code path cannot be misconfigured into either failure. The
		// data collector appends one event per evaluation to a bounded buffer
		// that a failed flush never drains; once full, every append flushes
		// synchronously on the evaluating goroutine, turning a relay outage into
		// stalled Mongo queries and NATS publishes. Remote evaluation would put
		// an HTTP request on that same path.
		DataCollectorDisabled: true,
		EvaluationType:        gofeatureflag.EvaluationTypeInProcess,
	})
	if err != nil {
		// Returned explicitly rather than as the pair, so a nil *Provider never
		// reaches a caller wrapped in a non-nil interface.
		return nil, err
	}
	return provider, nil
}

// watchProviderInit repairs an auto-installed provider whose first fetch failed.
//
// The failure it exists for is not transient. The asynchronous bind discards the
// SDK's init result, and the provider's in-process evaluator returns from Init
// before creating its ticker when the configuration fetch fails, so it polls
// nothing, recovers from nothing, and reports ERROR for the life of the process.
// A relay that is briefly unreachable while a fleet rolls out therefore used to
// produce processes in which the relay could never take effect again — every
// switch pinned to its local value, the kill switch dead, and nothing logged.
//
// So this watches the domain's state and rebinds a fresh provider whenever it
// reads ERROR, backing off from providerRetryInitialDelay to the poll interval.
// It ends on the first success, or when something else takes the domain over: an
// InstallProvider call, or a provider bound directly by the application. It logs
// the first failure at warn with the endpoint, keeps subsequent ones at debug so
// a long outage cannot flood, and reports the eventual recovery at info.
func watchProviderInit(providerName, endpoint string, interval time.Duration, gen uint64) {
	client := openfeature.NewClient(FlagDomain)
	ceiling := interval
	delay := min(providerRetryInitialDelay, ceiling)
	var warned bool

	for {
		time.Sleep(delay)
		if delay < ceiling {
			delay = min(2*delay, ceiling)
		}
		if installGen.Load() != gen || explicitBind.Load() {
			return
		}

		switch client.State() {
		case openfeature.ReadyState:
			if warned {
				slog.Info("feature flag provider reached the relay; instrumentation switches can be changed remotely again",
					"var", EnvFlagsEndpoint, "endpoint", endpoint)
			}
			return
		case openfeature.NotReadyState:
			// Still initialising. Nothing to repair yet.
			continue
		}

		if !warned {
			slog.Warn("feature flag provider could not reach the relay; retrying, and instrumentation switches cannot be changed remotely until it does",
				"var", EnvFlagsEndpoint, "endpoint", endpoint)
			warned = true
		} else {
			slog.Debug("feature flag provider still cannot reach the relay",
				"var", EnvFlagsEndpoint, "endpoint", endpoint, "retry", delay)
		}
		if !rebindRelayProvider(providerName, endpoint, interval, gen) {
			return
		}
	}
}

// rebindRelayProvider replaces a failed auto-installed provider with a fresh
// one. It reports whether the caller should keep watching.
//
// It runs under installMu, so it cannot interleave with an InstallProvider call
// or with the auto-install itself. The provider-name check is what keeps it from
// stealing a domain somebody else now owns: an application that bound its own
// provider directly, without going through InstallProvider, sets no explicitBind
// record, and its provider must not be replaced by this one.
func rebindRelayProvider(providerName, endpoint string, interval time.Duration, gen uint64) bool {
	installMu.Lock()
	defer installMu.Unlock()

	if installGen.Load() != gen || explicitBind.Load() {
		return false
	}
	if openfeature.NamedProviderMetadata(FlagDomain).Name != providerName {
		return false
	}

	provider, err := newRelayProvider(endpoint, interval)
	if err != nil {
		slog.Debug("feature flag provider could not be rebuilt", "var", EnvFlagsEndpoint, "error", err)
		return true
	}
	if err := openfeature.SetNamedProvider(FlagDomain, provider); err != nil {
		slog.Debug("feature flag provider could not be rebound", "domain", FlagDomain, "error", err)
	}
	return true
}

// pollIntervalFromEnv reads EnvFlagsPollInterval, falling back to
// defaultPollInterval.
//
// A malformed value warns and falls back rather than aborting the install. Note
// the deliberate asymmetry with Lookup, which fails construction on a value it
// cannot read: the interval has a safe fallback and a switch does not. Refusing
// to install over a typo in an optional tuning knob would delete the entire
// control plane — the highest-severity outcome reachable from the
// lowest-severity mistake — whereas guessing at an unreadable switch picks a
// behaviour nobody asked for.
func pollIntervalFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(EnvFlagsPollInterval))
	if v == "" {
		return defaultPollInterval
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		slog.Warn("invalid poll interval; falling back to the default",
			"var", EnvFlagsPollInterval, "value", v,
			"default", defaultPollInterval, "error", err)
		return defaultPollInterval
	case d <= 0:
		slog.Warn("non-positive poll interval; falling back to the default",
			"var", EnvFlagsPollInterval, "value", v,
			"default", defaultPollInterval)
		return defaultPollInterval
	default:
		return d
	}
}

// pollJitterFraction is the maximum deviation jitterInterval applies, as a
// fraction of the interval. It is upstream's value: the relay proxy's
// EnablePollingJitter uses the same 0.1 for the same reason on the hop above
// this one.
const pollJitterFraction = 0.1

// jitterInterval perturbs the polling interval by at most plus or minus a
// tenth, so that a fleet started from one deployment does not keep polling the
// relay on a shared period.
//
// Be precise about what this does and does not buy, because it is easy to
// oversell. The provider fetches the whole configuration once, unconditionally,
// inside Init, and only then starts a plain time.NewTicker; every later poll is
// ETag-conditional and answered with a 304 when nothing changed. So the
// expensive request is the one at process start, its timing is the process's
// own start time, and nothing here moves it. What this smooths is the cheap
// steady-state poll, and the reason to bother is only that a large fleet on an
// identical period turns even those into a periodic spike.
//
// Jittering the START instead — delaying the install so the first fetch
// scatters — would address the expensive request, and is deliberately not done.
// The startup window that ends at that fetch is the window in which every
// switch resolves to its local value: fail-safe for enabling, not for
// disabling, since a relay's false does not survive a restart. Lengthening it
// prolongs exactly the case an operator reaches for the relay to stop,
// including otel-mongo writing an _oteltrace field the relay was configured to
// stop writing — and a fleet restart is when a kill switch is most likely to be
// in use. That trades flag correctness for a spike a relay proxy answers from
// memory. The hop where correlated polling genuinely hurts is the relay's own
// retriever, and the relay proxy jitters that itself via enablePollingJitter.
//
// The deviation is fixed per process, not per tick, because the ticker's
// interval is chosen once inside Init and is not reachable afterwards. Phases
// therefore decorrelate by drift rather than immediately.
//
// The arithmetic deliberately mirrors upstream's newBackgroundUpdater
// (go-feature-flag, retriever/background_updater.go), so both hops of the
// polling chain deviate by the same rule: draw a magnitude uniformly from
// [0, 10% of the interval), then take the sign from that draw's parity in
// nanoseconds. Coupling the sign to the magnitude's low bit is upstream's
// choice and not one worth copying on its own merits — the bit is close enough
// to fair that the result is near-symmetric, and matching upstream is worth more
// than a marginally better distribution.
//
// An interval too short to yield a non-zero magnitude — below ten nanoseconds,
// including zero and negatives — is returned unchanged. pollIntervalFromEnv
// never produces one, but rand panics on a non-positive bound, and returning the
// input keeps the provider's own handling of such an interval reachable.
func jitterInterval(d time.Duration) time.Duration {
	maxJitter := float64(d) * pollJitterFraction
	if int64(maxJitter) <= 0 {
		return d
	}
	//nolint:gosec // G404: load spreading, not a secret; an adversary who can predict this learns when a poll happens and nothing else.
	jitter := time.Duration(rand.Int64N(int64(maxJitter)))
	if jitter%2 == 0 {
		return d + jitter
	}
	return d - jitter
}
