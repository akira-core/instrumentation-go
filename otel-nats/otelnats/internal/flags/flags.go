// Package flags provides shared feature-flag helpers used by the
// instrumentation modules. The file contents (excluding the package
// declaration) MUST be byte-identical across every module copy of this package.
//
// That rule is maintained by code review, not by a check: no CI step compares
// the copies. It is worth naming what a single drifted copy would do, because
// this file concentrates the logic the whole kill-switch model rests on:
//
//   - the literal true evaluation default in Allowed — if one copy reverted it
//     to an env read, that module's relay could ENABLE again, in a diff that
//     looks like the previous release;
//   - the truthiness allow-list — that module would accept "enabled" or an
//     empty string where the others do not;
//   - EnvSet — the mutual-exclusion check would misfire, or fail to fire;
//   - FlagDomain — that module would resolve through a domain no provider is
//     bound to, silently losing relay control, and no test would go red;
//   - the lazy client construction — "switches off means the OpenFeature SDK is
//     never touched" would stop holding for that module.
//
// Exported primitives:
//
//   - EnvEnabled / EnvSet read a single env var: an explicit truthy allow-list,
//     and a bare presence predicate for the mutual-exclusion check.
//   - EnvGlobalTracing / GlobalTracingPossible / GlobalTracingSet name and read
//     the process-wide kill switch.
//   - EnvFlagsEndpoint / EnvFlagsAPIKey / EnvFlagsPollInterval configure the
//     provider auto-install; EnvServiceName supplies its targeting attribute.
//   - FlagDomain is the single OpenFeature domain all modules resolve through.
//   - Resolver resolves a module's flag keys through the OpenFeature client on
//     EVERY call. It caches nothing: no snapshot, no TTL, no clock, no refresh.
//
// Every name this file defines is process-scoped. Module-scoped flag keys and
// module-scoped env var names cannot live here — each module supplies its keys
// through WithFlagKeys and owns the paired env var, ANDing it itself.
//
// This package never installs a DEFAULT provider, never sets a global
// evaluation context, never adds hooks and never shuts the SDK down — exactly
// as the instrumentation packages never initialize a TracerProvider. The one
// piece of OpenFeature state it may write is a NAMED provider bound to
// FlagDomain, and only when the environment asks for one (EnvFlagsEndpoint is
// set) and the application installed none of its own. Nothing it does can
// change how the application's own feature flags resolve.
//
// The global switch is read via GlobalTracingPossible and ANDed ahead of the
// Resolver, never expressed as a flag key: it is an out-of-band kill switch
// with no relay counterpart, and while it is off no OpenFeature code path runs
// at all.
package flags

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
	"github.com/open-feature/go-sdk/openfeature"
)

const (
	// EnvGlobalTracing is the process-wide kill switch. It has no relay
	// counterpart: it is the only brake that works when the relay is
	// unreachable or misconfigured.
	EnvGlobalTracing = "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED"

	// EnvFlagsEndpoint is the GO Feature Flag relay proxy URL. Setting it is an
	// operator's request for relay control; leaving it unset means no provider
	// is ever constructed and no OpenFeature state is written.
	EnvFlagsEndpoint = "OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT"

	// EnvFlagsAPIKey authenticates against a relay proxy that requires it. Its
	// value is never logged.
	EnvFlagsAPIKey = "OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY"

	// EnvFlagsPollInterval overrides how often the provider polls the relay.
	// Go duration strings only: a bare integer is rejected rather than read as
	// milliseconds, because misreading a polling interval that way is
	// catastrophic rather than merely wrong.
	EnvFlagsPollInterval = "OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL"

	// EnvServiceName is the OpenTelemetry-specified service name. It is the
	// only source of targeting attributes this package uses, and only on the
	// auto-install path.
	EnvServiceName = "OTEL_SERVICE_NAME"
)

// FlagDomain is the single OpenFeature domain every module resolves through.
//
// One domain rather than one per module is forced by the provider: the
// in-process evaluator's Init is not idempotent, so registering one instance
// under N domains starts N polling goroutines of which N-1 can never be
// stopped, and N separate instances would poll the relay N times over identical
// configuration.
//
// Exported because module-package tests install their in-memory provider on it.
const FlagDomain = "otel-instrumentation-go"

// defaultPollInterval is how often the auto-installed provider polls the relay,
// and therefore the upper bound on how long a revocation takes to reach a
// running process. It is set explicitly because the provider's own default is
// two minutes, which is too slow for an emergency brake, and because that
// default applies whenever the configured interval is non-positive.
const defaultPollInterval = 60 * time.Second

// noopProviderName is what the OpenFeature SDK's built-in no-op provider
// reports. It is the reliable test for "the application has installed no
// provider", and the second half of the auto-install condition.
const noopProviderName = "NoopProvider"

// EnvEnabled reports whether the named environment variable is set to a truthy
// value. Default-off: an unset variable returns false.
//
// Truthy is an explicit allow-list — "1", "true", "yes", "on", lowercased and
// whitespace-trimmed — mirroring the falsy list one for one. Everything else is
// disabled, including the empty string: `export VAR=` must not open a kill
// switch.
//
// A set value in neither list is a misconfiguration, so it warns before
// returning false. Unset, truthy and explicitly falsy values stay silent, so a
// correct deployment logs nothing. The warning is not deduplicated: suppressing
// repeats would need mutable state in this file to quieten a message that only
// appears when a deployment is already wrong.
func EnvEnabled(name string) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "", "0", "false", "no", "off":
		return false
	default:
		slog.Warn("unrecognised boolean value; treated as disabled",
			"var", name,
			"value", v,
			"accepted", "1,true,yes,on / 0,false,no,off")
		return false
	}
}

// EnvSet reports only whether the named environment variable is present.
//
// It exists for the mutual-exclusion check, which must distinguish "the
// deployment expressed no opinion" from "the deployment explicitly set this
// off" — a distinction EnvEnabled cannot make. It MUST NOT be used to decide
// whether a switch is enabled.
func EnvSet(name string) bool {
	_, ok := os.LookupEnv(name)
	return ok
}

// GlobalTracingPossible reports whether this process may ever run instrumented
// paths or negotiate otel-ws. It reads EnvGlobalTracing only, never OpenFeature.
func GlobalTracingPossible() bool { return EnvEnabled(EnvGlobalTracing) }

// GlobalTracingSet reports whether EnvGlobalTracing is present, for the
// mutual-exclusion check against WithTracingEnabled.
func GlobalTracingSet() bool { return EnvSet(EnvGlobalTracing) }

// Resolver resolves one module's flag keys through the OpenFeature client.
//
// It caches nothing. Allowed evaluates on every call, so a revocation takes
// effect on the very next operation and there is no TTL, no snapshot timestamp,
// no clock to inject and no cross-flag consistency window beyond the
// microseconds between two consecutive calls.
//
// The measured cost is roughly 2 µs and 7 allocations per call against 82 ns
// for an atomic snapshot read. That is the SDK's evaluation pipeline — hook
// chains, evaluation-context merging, the provider registry lock — not the flag
// lookup, and an in-memory provider does not make it cheaper. Caching is a
// permitted future optimisation: it fits entirely inside this type without
// changing Allowed's signature or any call site.
type Resolver struct {
	// keys are OpenFeature flag keys in Allowed-index order.
	keys []string

	clientOnce sync.Once
	client     openfeature.IClient

	// evalCtx is populated only when this Resolver auto-installed the provider.
	// An application that installs its own provider owns its evaluation context
	// outright, and this stays zero so nothing it set can be overridden.
	evalCtx openfeature.EvaluationContext
}

// ResolverOption configures a Resolver at construction.
type ResolverOption func(*Resolver)

// WithFlagKeys sets the OpenFeature flag keys this Resolver resolves. Each
// key's index is the index callers pass to Allowed.
func WithFlagKeys(keys ...string) ResolverOption {
	return func(r *Resolver) { r.keys = keys }
}

// NewResolver returns a Resolver for one module.
//
// There is no domain parameter: the domain is process-scoped (FlagDomain), so
// making it a parameter would only create a string that has to agree across
// every module with nothing checking it.
//
// No OpenFeature client is created here, and no provider is installed. Both
// happen lazily on the first Allowed call, which is reached only when a wrapper
// was built on the instrumented path — so a process whose switches are off
// never touches the OpenFeature SDK at all.
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

// Allowed returns the relay verdict for the key at index i.
//
// The evaluation default is a literal true, which is what makes the relay a
// kill switch: Client.Boolean returns that default on every failure path — no
// provider installed, provider not ready, key absent from the relay
// configuration, evaluation error, type mismatch — so all of them mean "do not
// interfere" and the environment alone decides. Nothing on the relay can enable
// what the environment left off.
//
// An out-of-range index returns false rather than panicking, so a mis-wired
// module degrades to the disabled path instead of taking the process down.
func (r *Resolver) Allowed(i int) bool {
	if i < 0 || i >= len(r.keys) {
		return false
	}
	return r.evaluator().Boolean(context.Background(), r.keys[i], true, r.evalCtx)
}

// evaluator lazily installs the environment-configured provider, if any, and
// creates the domain-scoped OpenFeature client.
func (r *Resolver) evaluator() openfeature.IClient {
	r.clientOnce.Do(func() {
		r.evalCtx = installProviderFromEnv()
		r.client = openfeature.NewClient(FlagDomain)
	})
	return r.client
}

// installProviderFromEnv registers a GO Feature Flag provider on FlagDomain
// when the environment asks for one and the application installed none, and
// returns the evaluation context to use with it.
//
// Two conditions, both necessary. EnvFlagsEndpoint is the operator's expression
// of intent. The NoopProvider check is what makes this an allowance rather than
// a takeover: an application that installs its own provider before constructing
// any wrapper keeps it, and this returns without writing anything.
//
// Registration is non-blocking on purpose. Blocking would put a relay round
// trip in front of the first instrumented operation, and a brake must not
// become a latency source. The window that leaves — every flag reads true until
// the provider's first fetch — is the application's to close, by installing its
// own provider with SetProviderAndWait.
//
// Because four module copies of this package share no state, two modules
// evaluating for the first time concurrently may both observe NoopProvider and
// both register. The SDK replaces the earlier registration and shuts that
// provider down, leaving one live provider and one poller; the cost is a
// duplicated first fetch. No lock can span four packages that do not import
// each other.
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
	// or an earlier auto-install by a sibling module — and only a process that
	// has made none of them reads back "NoopProvider". Checking the default
	// alone would clobber an application that deliberately bound a provider to
	// this domain.
	if openfeature.NamedProviderMetadata(FlagDomain).Name != noopProviderName {
		return empty
	}

	provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
		Endpoint:                  endpoint,
		APIKey:                    os.Getenv(EnvFlagsAPIKey),
		FlagChangePollingInterval: pollIntervalFromEnv(),

		// Both hardcoded, and deliberately not exposed as environment
		// variables, so the zero-code path cannot be misconfigured into either
		// failure. The data collector appends one event per evaluation to a
		// bounded buffer that a failed flush never drains; once full, every
		// append flushes synchronously on the evaluating goroutine, turning a
		// relay outage into stalled Mongo queries and NATS publishes. Remote
		// evaluation would put an HTTP request on that same path.
		DataCollectorDisabled: true,
		EvaluationType:        gofeatureflag.EvaluationTypeInProcess,
	})
	if err != nil {
		slog.Warn("feature flag provider unavailable; tracing cannot be revoked remotely",
			"var", EnvFlagsEndpoint, "error", err)
		return empty
	}

	if err := openfeature.SetNamedProvider(FlagDomain, provider); err != nil {
		slog.Warn("feature flag provider registration failed; tracing cannot be revoked remotely",
			"domain", FlagDomain, "error", err)
		return empty
	}

	// Targeting attribute, supplied only here. Passed at the invocation site
	// rather than through SetEvaluationContext, so it composes with an
	// application's global context instead of replacing it — and confined to
	// this path, so it can never override a service.name the application set.
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
// A malformed value warns and falls back rather than aborting the install: a
// typo in an optional tuning knob must not silently delete the entire kill
// switch, which is the highest-severity outcome reachable from the
// lowest-severity mistake.
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
