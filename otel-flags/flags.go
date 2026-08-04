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
// instance of installOnce below, and therefore exactly one install.
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
// interval, not anything this package adds.
package otelflags

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
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
	// catastrophic rather than merely wrong.
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
// and therefore the upper bound on how long a flag change takes to reach a
// running process. It is set explicitly because the provider's own default is
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

// RelayPossible reports whether a relay could ever have an opinion in this
// process: an endpoint is configured, or a provider is already bound to
// FlagDomain (or installed as the default, which NamedProviderMetadata falls
// back to).
//
// When it is false, Client.Boolean can only ever return the value passed to it,
// so the relay is not merely silent — it is structurally incapable of speaking.
// Callers use that to keep the pre-dynamic zero-cost path: resolve from
// env > option > default alone, allocate the instrumented implementation only if
// that answer is on, and never touch the OpenFeature SDK.
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
	return openfeature.NamedProviderMetadata(FlagDomain).Name != noopProviderName
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
func (r *Resolver) Value(i int, local bool) bool {
	if i < 0 || i >= len(r.keys) {
		return false
	}
	return r.evaluator().Boolean(context.Background(), r.keys[i], local, r.evalCtx)
}

// evaluator lazily installs the environment-configured provider, if any, and
// creates the domain-scoped OpenFeature client.
func (r *Resolver) evaluator() openfeature.IClient {
	r.clientOnce.Do(func() {
		r.evalCtx = installProvider()
		r.client = openfeature.NewClient(FlagDomain)
	})
	return r.client
}

// Provider-install state, guarded by installMu.
//
// This is package-level rather than per-Resolver, and that is the whole point of
// the module. A process holds one Resolver for the master switch and one per
// instrumentation module; with the check-then-register sequence inside each
// Resolver's own sync.Once, two of them could observe NoopProvider concurrently
// and both register. One mutex here serialises the sequence across all of them,
// so "one shared module" becomes "exactly one install".
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
// its own provider with SetProviderAndWait, which also makes this stand down.
func installProviderFromEnv() openfeature.EvaluationContext {
	var empty openfeature.EvaluationContext

	endpoint := strings.TrimSpace(os.Getenv(EnvFlagsEndpoint))
	if endpoint == "" {
		return empty
	}
	// NamedProviderMetadata, not ProviderMetadata: it reports the provider bound
	// to FlagDomain and falls back to the default provider's metadata when none
	// is. One check therefore covers all three ways an application can already
	// have made its choice — a default provider, a named provider on our domain,
	// or an earlier install by this same code — and only a process that has made
	// none of them reads back "NoopProvider". Checking the default alone would
	// clobber an application that deliberately bound a provider to this domain.
	if openfeature.NamedProviderMetadata(FlagDomain).Name != noopProviderName {
		return empty
	}

	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:                  endpoint,
		APIKey:                    os.Getenv(EnvFlagsAPIKey),
		FlagChangePollingInterval: pollIntervalFromEnv(),

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
		slog.Warn("feature flag provider unavailable; instrumentation switches cannot be changed remotely",
			"var", EnvFlagsEndpoint, "error", err)
		return empty
	}

	if err := openfeature.SetNamedProvider(FlagDomain, provider); err != nil {
		slog.Warn("feature flag provider registration failed; instrumentation switches cannot be changed remotely",
			"domain", FlagDomain, "error", err)
		return empty
	}

	// Targeting attribute, supplied only here. Passed at the invocation site
	// rather than through SetEvaluationContext, so it composes with an
	// application's global context instead of replacing it — and confined to this
	// path, so it can never override a service.name the application set.
	if svc := strings.TrimSpace(os.Getenv(EnvServiceName)); svc != "" {
		return openfeature.NewTargetlessEvaluationContext(map[string]any{
			"service.name": svc,
		})
	}
	return empty
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
