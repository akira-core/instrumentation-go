package otelflags

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
)

// No test in this file may call t.Parallel: the OpenFeature provider registry,
// the process environment and slog's default logger are all process-global.

const testEnvKey = "OTEL_INSTRUMENTATION_GO_FLAGS_TEST_SWITCH"

// resetInstallState clears the package-level provider-install latch so a test
// can exercise the install path from scratch.
//
// This is a test helper in the same package, not an exported reset hook — the
// design deliberately has none, because the resolver caches no flag values and a
// rebound provider is observed on the very next call. The only thing that
// latches is "did we already install", and that has to be resettable for the
// auto-install tests to be more than one test long.
// It also bumps installGen, which retires any watchProviderInit goroutine an
// earlier test left running against an unreachable endpoint. Without that, one
// of them could wake up mid-test and rebind FlagDomain underneath the in-memory
// provider the current test installed.
func resetInstallState(t *testing.T) {
	t.Helper()
	installMu.Lock()
	defer installMu.Unlock()
	installDone = false
	installedEvalCtx.Store(nil)
	explicitBind.Store(false)
	relayPossibleRead.Store(false)
	autoInstalled.Store(false)
	installGen.Add(1)
}

// mustValidate runs the process-level entry point every wrapper constructor
// calls, and fails the test if the environment it reads is unreadable.
//
// Tests call it wherever a wrapper's construction would: the provider install
// happens here, not on the first evaluation.
func mustValidate(t *testing.T) {
	t.Helper()
	if err := ValidateAndInstall(); err != nil {
		t.Fatalf("ValidateAndInstall: %v", err)
	}
}

// setDefaultProvider installs a provider in the DEFAULT slot — an application's
// own feature flags, with no relation to this library — and restores the no-op
// provider afterwards.
func setDefaultProvider(t *testing.T, name string) {
	t.Helper()
	if err := openfeature.SetProviderAndWait(memprovider.NewInMemoryProvider(
		map[string]memprovider.InMemoryFlag{name: boolFlag(true)})); err != nil {
		t.Fatalf("SetProviderAndWait: %v", err)
	}
	t.Cleanup(func() {
		if err := openfeature.SetProviderAndWait(openfeature.NoopProvider{}); err != nil {
			t.Fatalf("SetProviderAndWait(Noop): %v", err)
		}
	})
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		set       bool
		wantValue bool
		wantSet   bool
		wantErr   bool
	}{
		{name: "unset has no opinion"},

		{name: "one", value: "1", set: true, wantValue: true, wantSet: true},
		{name: "true", value: "true", set: true, wantValue: true, wantSet: true},
		{name: "yes", value: "yes", set: true, wantValue: true, wantSet: true},
		{name: "on", value: "on", set: true, wantValue: true, wantSet: true},
		{name: "upper TRUE", value: "TRUE", set: true, wantValue: true, wantSet: true},
		{name: "mixed On", value: "On", set: true, wantValue: true, wantSet: true},
		{name: "padded yes", value: "  yes  ", set: true, wantValue: true, wantSet: true},

		{name: "zero", value: "0", set: true, wantSet: true},
		{name: "false", value: "false", set: true, wantSet: true},
		{name: "no", value: "no", set: true, wantSet: true},
		{name: "off", value: "off", set: true, wantSet: true},
		{name: "upper FALSE", value: "FALSE", set: true, wantSet: true},
		{name: "padded off", value: " off ", set: true, wantSet: true},

		// Everything below fails construction rather than being guessed at.
		{name: "empty string", value: "", set: true, wantSet: true, wantErr: true},
		{name: "whitespace only", value: "   ", set: true, wantSet: true, wantErr: true},
		{name: "enabled word", value: "enabled", set: true, wantSet: true, wantErr: true},
		{name: "two", value: "2", set: true, wantSet: true, wantErr: true},
		{name: "y", value: "y", set: true, wantSet: true, wantErr: true},
		{name: "t", value: "t", set: true, wantSet: true, wantErr: true},
		{name: "arbitrary string", value: "hello", set: true, wantSet: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(testEnvKey, tc.value)
			}
			value, set, err := Lookup(testEnvKey)

			if (err != nil) != tc.wantErr {
				t.Fatalf("Lookup(%q=%q) err = %v, wantErr %v", testEnvKey, tc.value, err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidFlagValue) {
					t.Errorf("error does not wrap ErrInvalidFlagValue: %v", err)
				}
				if !strings.Contains(err.Error(), testEnvKey) {
					t.Errorf("error does not name the variable: %v", err)
				}
				if tc.value != "" && !strings.Contains(err.Error(), tc.value) {
					t.Errorf("error does not name the observed value: %v", err)
				}
				return
			}
			if value != tc.wantValue || set != tc.wantSet {
				t.Fatalf("Lookup(%q=%q) = (%v, %v), want (%v, %v)",
					testEnvKey, tc.value, value, set, tc.wantValue, tc.wantSet)
			}
		})
	}
}

func TestMasterLocal(t *testing.T) {
	t.Run("unset defaults to enabled", func(t *testing.T) {
		got, err := MasterLocal()
		if err != nil {
			t.Fatalf("MasterLocal: %v", err)
		}
		if !got {
			t.Fatalf("MasterLocal() = false with the variable unset; the master is a veto and must default to true")
		}
	})

	t.Run("explicitly falsy vetoes", func(t *testing.T) {
		t.Setenv(EnvGlobalTracing, "0")
		got, err := MasterLocal()
		if err != nil {
			t.Fatalf("MasterLocal: %v", err)
		}
		if got {
			t.Fatalf("MasterLocal() = true for an explicitly falsy value")
		}
	})

	t.Run("truthy is inert but legal", func(t *testing.T) {
		t.Setenv(EnvGlobalTracing, "true")
		got, err := MasterLocal()
		if err != nil || !got {
			t.Fatalf("MasterLocal() = (%v, %v), want (true, nil)", got, err)
		}
	})

	t.Run("invalid value is an error", func(t *testing.T) {
		t.Setenv(EnvGlobalTracing, "maybe")
		if _, err := MasterLocal(); !errors.Is(err, ErrInvalidFlagValue) {
			t.Fatalf("MasterLocal() err = %v, want ErrInvalidFlagValue", err)
		}
	})
}

// syncBuffer is a bytes.Buffer safe to read while something else writes it.
//
// slog's TextHandler serialises writers against each other but not against a
// reader, and the loggers these tests capture are shared with goroutines the
// test does not own: the provider's initialisation runs on one, and
// gofeatureflag.NewProvider captures slog.Default() permanently. Reading a bare
// buffer from the test goroutine is a data race under -race, and CI runs -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs redirects slog's default logger into a buffer for the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// boolFlag builds an in-memory flag that always resolves to v.
func boolFlag(v bool) memprovider.InMemoryFlag {
	variant := "off"
	if v {
		variant = "on"
	}
	return memprovider.InMemoryFlag{
		State:          memprovider.Enabled,
		DefaultVariant: variant,
		Variants:       map[string]any{"on": true, "off": false},
	}
}

// setProvider installs an in-memory provider on FlagDomain for the duration of
// the test.
//
// It binds the NAMED domain, not the default provider. A named provider outranks
// the default for the clients this package creates, so a test that installed a
// default provider would be silently shadowed the moment anything in the process
// had auto-installed.
func setProvider(t *testing.T, flags map[string]memprovider.InMemoryFlag) {
	t.Helper()
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, memprovider.NewInMemoryProvider(flags)); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	t.Cleanup(func() { clearProvider(t) })
}

// clearProvider rebinds FlagDomain to the no-op provider, modelling an
// application that never wires OpenFeature at all.
//
// It is the closest this suite can get to the real thing, and the gap is worth
// knowing about: the SDK offers no way to UNBIND a domain, so once anything in
// the process binds FlagDomain — as this does — the state where the domain is
// absent from the SDK's map, and ForEvaluation falls back to the application's
// default provider, is unreachable for the rest of the binary. Value's
// providerBound guard is what makes that state harmless in production; no test
// here can enter it to prove the guard fires.
func clearProvider(t *testing.T) {
	t.Helper()
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, openfeature.NoopProvider{}); err != nil {
		t.Fatalf("SetNamedProviderAndWait(Noop): %v", err)
	}
}

// --- the precedence ladder -------------------------------------------------

func TestValue_NoProviderReturnsLocal(t *testing.T) {
	clearProvider(t)

	r := NewResolver()
	for _, local := range []bool{true, false} {
		if got := r.Value("some-key", local); got != local {
			t.Fatalf("Value(%v) = %v with no provider installed; the local value must stand", local, got)
		}
	}
}

func TestValue_MissingFlagReturnsLocal(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"other-key": boolFlag(false)})

	r := NewResolver()
	for _, local := range []bool{true, false} {
		if got := r.Value("absent-key", local); got != local {
			t.Fatalf("Value(%v) = %v for a key absent from the relay; the local value must stand", local, got)
		}
	}
}

func TestValue_RelayOverridesLocalInBothDirections(t *testing.T) {
	t.Run("relay disables what the deployment enabled", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})

		r := NewResolver()
		if r.Value("k", true) {
			t.Fatalf("Value(true) = true while the relay serves false")
		}
	})

	t.Run("relay enables what the deployment left off", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

		r := NewResolver()
		if !r.Value("k", false) {
			t.Fatalf("Value(false) = false while the relay serves true; the relay is authoritative in both directions")
		}
	})
}

func TestValue_ChangeIsVisibleOnTheNextCall(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

	r := NewResolver()
	if !r.Value("k", false) {
		t.Fatalf("Value = false while the relay serves true")
	}

	// Re-bind with the flag flipped. No sleep, no clock, no reset hook: the
	// resolver caches nothing, so the very next call must observe it.
	if err := openfeature.SetNamedProviderAndWait(FlagDomain,
		memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	if r.Value("k", true) {
		t.Fatalf("Value = true on the call after a change; the value must not be cached")
	}
}

// TestValue_ResolvesTheKeyItIsGiven is what the key parameter buys over the
// index it replaced.
//
// A resolver holds no key list, so there is no positional coupling left to get
// wrong: two modules sharing one resolver cannot swap each other's flags by
// listing them in the wrong order.
func TestValue_ResolvesTheKeyItIsGiven(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{
		"otel-mongo-tracing":     boolFlag(true),
		"otel-mongo-propagation": boolFlag(false),
	})

	r := NewResolver()
	if !r.Value("otel-mongo-tracing", false) {
		t.Errorf("otel-mongo-tracing resolved false while the relay serves true")
	}
	if r.Value("otel-mongo-propagation", true) {
		t.Errorf("otel-mongo-propagation resolved true while the relay serves false")
	}
}

func TestValue_Concurrent(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

	r := NewResolver()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Value("k", false)
		}()
	}
	wg.Wait()
}

// --- the master switch -----------------------------------------------------

func TestMasterEnabled(t *testing.T) {
	t.Run("no relay leaves the local value in charge", func(t *testing.T) {
		clearProvider(t)
		if !MasterEnabled(true) {
			t.Fatalf("MasterEnabled(true) = false with no relay")
		}
		if MasterEnabled(false) {
			t.Fatalf("MasterEnabled(false) = true with no relay")
		}
	})

	t.Run("relay false vetoes a locally-enabled master", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{FlagKeyGlobalTracing: boolFlag(false)})
		if MasterEnabled(true) {
			t.Fatalf("MasterEnabled(true) = true while the relay vetoes the master")
		}
	})

	t.Run("relay true is inert against the default", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{FlagKeyGlobalTracing: boolFlag(true)})
		if !MasterEnabled(true) {
			t.Fatalf("MasterEnabled(true) = false while the relay serves true")
		}
	})
}

// --- relayPossible ---------------------------------------------------------

func TestRelayPossible(t *testing.T) {
	t.Run("false with no endpoint and no provider", func(t *testing.T) {
		clearProvider(t)
		if RelayPossible() {
			t.Fatalf("RelayPossible() = true with no endpoint and no provider bound")
		}
	})

	t.Run("true when the endpoint is set", func(t *testing.T) {
		clearProvider(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		if !RelayPossible() {
			t.Fatalf("RelayPossible() = false with an endpoint configured")
		}
	})

	t.Run("true when a provider is already bound", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
		if !RelayPossible() {
			t.Fatalf("RelayPossible() = false with a provider bound to the domain")
		}
	})

	t.Run("an empty endpoint is not an endpoint", func(t *testing.T) {
		clearProvider(t)
		t.Setenv(EnvFlagsEndpoint, "   ")
		if RelayPossible() {
			t.Fatalf("RelayPossible() = true for a blank endpoint")
		}
	})

	// One reading of the variable, shared with ValidateAndInstall. "relay:1031"
	// is non-blank but fails validation, so a hand-rolled non-blank check here
	// would call a relay possible that can never be built.
	t.Run("an endpoint that fails validation is not an endpoint", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })
		t.Setenv(EnvFlagsEndpoint, "relay:1031")

		if RelayPossible() {
			t.Fatalf("RelayPossible() = true for an endpoint ValidateAndInstall rejects")
		}
	})

	t.Run("a provider the application installed for its own flags does not count", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		setDefaultProvider(t, "checkout-experiment")

		if RelayPossible() {
			t.Fatalf("RelayPossible() = true with only a default provider installed; " +
				"an application's own feature flags say nothing about the instrumentation switches")
		}
	})
}

// TestBoundToDomain is where the default-provider fallback is actually pinned.
//
// It tests the comparison rather than the two SDK calls that feed it, because
// the row that matters most cannot be built through the SDK: the fallback fires
// only while FlagDomain is unbound, and once any test binds it there is no way
// to unbind it again. The rows are the full truth table of what
// NamedProviderMetadata can return.
func TestBoundToDomain(t *testing.T) {
	const (
		noop     = noopProviderName
		business = "BusinessProvider"
		ours     = "GO Feature Flag"
	)
	meta := func(name string) openfeature.Metadata { return openfeature.Metadata{Name: name} }

	tests := []struct {
		name  string
		named string
		def   string
		want  bool
	}{
		{name: "nothing installed anywhere", named: noop, def: noop},
		{name: "domain explicitly bound to the no-op provider", named: noop, def: business},

		// The regression. With nothing bound to FlagDomain, NamedProviderMetadata
		// returns the DEFAULT provider's metadata, so the application's business
		// provider used to read back as a binding on our domain.
		{name: "default provider only, echoed by the fallback", named: business, def: business},

		{name: "application bound its own provider to the domain", named: business, def: noop, want: true},
		{name: "application bound one to each slot", named: ours, def: business, want: true},
		{name: "we auto-installed", named: ours, def: noop, want: true},

		// The one surviving false negative: the same provider in both slots is
		// indistinguishable from the fallback. SetNamedProvider closes it.
		{name: "same provider in both slots reads as unbound", named: business, def: business},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundToDomain(meta(tc.named), meta(tc.def)); got != tc.want {
				t.Fatalf("boundToDomain(named=%q, default=%q) = %v, want %v",
					tc.named, tc.def, got, tc.want)
			}
		})
	}
}

func TestSetNamedProvider(t *testing.T) {
	t.Run("binds the domain and records the choice", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		if err := SetNamedProvider(memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})); err != nil {
			t.Fatalf("SetNamedProvider: %v", err)
		}

		if !RelayPossible() {
			t.Fatalf("RelayPossible() = false after SetNamedProvider")
		}
		r := NewResolver()
		if !r.Value("k", false) {
			t.Fatalf("Value(false) = false; the installed provider must decide")
		}
	})

	t.Run("the record survives a provider that reads back as the default", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		setDefaultProvider(t, "checkout-experiment")
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		// Same provider type in both slots — the heuristic alone reads this as
		// unbound. The explicit record is what makes it exact, and it is the only
		// way to share ONE provider instance between the application and this
		// library.
		if err := SetNamedProvider(memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})); err != nil {
			t.Fatalf("SetNamedProvider: %v", err)
		}
		if !RelayPossible() {
			t.Fatalf("RelayPossible() = false after an explicit install; the record must outrank the heuristic")
		}
	})

	t.Run("a nil provider is an error, not a panic", func(t *testing.T) {
		if err := SetNamedProvider(nil); err == nil {
			t.Fatalf("SetNamedProvider(nil) = nil, want an error")
		}
	})

	// The auto-install's targeting attributes are confined to the auto-install
	// path so they can never override a context the application owns. Leaving
	// them published after the application takes the domain over sends them to
	// ITS provider on every evaluation — the same override by a slower route.
	t.Run("the auto-install's service attributes go with its provider", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		mustValidate(t)
		if got := currentEvalCtx().Attributes()["serviceName"]; got != "checkout-api" {
			t.Fatalf("serviceName = %v before the takeover, want checkout-api", got)
		}

		if err := SetNamedProvider(memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})); err != nil {
			t.Fatalf("SetNamedProvider: %v", err)
		}

		if attrs := currentEvalCtx().Attributes(); len(attrs) != 0 {
			t.Fatalf("evaluation context = %v after the application took the domain over; "+
				"the auto-install's attributes must not reach its provider", attrs)
		}
		if got := currentEvalCtx().TargetingKey(); got != processTargetingKey {
			t.Errorf("targeting key = %q, want the process key %q; bucketing rules must still apply",
				got, processTargetingKey)
		}
	})
}

// TestSetNamedProvider_WarnsWhenWrappersAlreadyExist covers the ordering rule
// that RelayPossible's construction-time snapshot creates.
//
// A wrapper built before the provider was bound resolved relayPossible as false
// and allocated no instrumented implementation, so a relay bound afterwards can
// never reach it. Nothing can repair that from here — the warning is the whole
// remedy, and it is why the install belongs before the first constructor.
func TestSetNamedProvider_WarnsWhenWrappersAlreadyExist(t *testing.T) {
	provider := func() openfeature.FeatureProvider {
		return memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
	}

	t.Run("warns after a wrapper has resolved its snapshot", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		buf := captureLogs(t)
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		_ = RelayPossible() // a wrapper being constructed

		if err := SetNamedProvider(provider()); err != nil {
			t.Fatalf("SetNamedProvider: %v", err)
		}
		if !strings.Contains(buf.String(), "wrapper") {
			t.Fatalf("no warning about wrappers constructed before the install: %q", buf.String())
		}
	})

	t.Run("silent when installed first", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		buf := captureLogs(t)
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		if err := SetNamedProvider(provider()); err != nil {
			t.Fatalf("SetNamedProvider: %v", err)
		}
		if strings.Contains(buf.String(), "wrapper") {
			t.Fatalf("warned about wrappers that do not exist yet: %q", buf.String())
		}
	})
}

func TestAutoInstall_ProceedsWhenOnlyADefaultProviderExists(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	setDefaultProvider(t, "checkout-experiment")
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	mustValidate(t)

	// The operator set the endpoint. Standing down for a provider the application
	// installed for its own flags would leave that endpoint with nothing behind
	// it, and every instrumentation key evaluated against the business provider.
	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("no provider installed on %q; an unrelated default provider must not "+
			"stand the auto-install down", FlagDomain)
	}
}

// --- the startup window ----------------------------------------------------

// notReadyProvider models a provider between registration and its first
// successful fetch: bound, but with nothing to say.
type notReadyProvider struct{ openfeature.NoopProvider }

func (notReadyProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "NotReadyProvider"}
}

func (notReadyProvider) BooleanEvaluation(_ context.Context, _ string, defaultValue bool,
	_ openfeature.FlattenedContext,
) openfeature.BoolResolutionDetail {
	return openfeature.BoolResolutionDetail{
		Value: defaultValue,
		ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
			ResolutionError: openfeature.NewProviderNotReadyResolutionError("first fetch has not completed"),
			Reason:          openfeature.ErrorReason,
		},
	}
}

// TestValue_NotReadyProviderLeavesLocalInCharge pins the startup window's
// contract in BOTH directions.
//
// Between a non-blocking install and the provider's first successful fetch,
// every switch resolves to its local value. That is fail-safe for enabling — the
// window can delay a relay-driven enable but never introduce one — and it is
// deliberately NOT fail-safe for disabling: a relay value of false does not
// survive a restart, because a provider with nothing to say must not be able to
// veto. Reading not-ready as "disabled" would apply to the master key too, whose
// local default is true, so every restart of every relay-configured process
// would be fully vetoed until its first fetch, and indefinitely while the relay
// is down. Durable state belongs in the environment variable.
func TestValue_NotReadyProviderLeavesLocalInCharge(t *testing.T) {
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, notReadyProvider{}); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	t.Cleanup(func() { clearProvider(t) })

	r := NewResolver()
	for _, local := range []bool{true, false} {
		if got := r.Value("k", local); got != local {
			t.Fatalf("Value(%v) = %v before the provider's first fetch; the local value must stand", local, got)
		}
	}

	if got := MasterEnabled(true); !got {
		t.Fatalf("MasterEnabled(true) = false before the provider's first fetch; "+
			"a provider with nothing to say must not veto the master (got %v)", got)
	}
}

// --- evaluation diagnostics ------------------------------------------------

// codeProvider fails every boolean evaluation with a fixed error code, the way
// a real provider reports a missing targeting key or an unreachable relay.
//
// The zero code models success: the SDK returns the value with no error, which
// is what a recovery looks like from here.
type codeProvider struct {
	openfeature.NoopProvider
	code openfeature.ErrorCode
}

func (codeProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "CodeProvider"}
}

func (p codeProvider) BooleanEvaluation(_ context.Context, _ string, defaultValue bool,
	_ openfeature.FlattenedContext,
) openfeature.BoolResolutionDetail {
	if p.code == "" {
		return openfeature.BoolResolutionDetail{
			Value:                    defaultValue,
			ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason},
		}
	}
	var resErr openfeature.ResolutionError
	switch p.code {
	case openfeature.TargetingKeyMissingCode:
		resErr = openfeature.NewTargetingKeyMissingResolutionError("no targeting key")
	case openfeature.FlagNotFoundCode:
		resErr = openfeature.NewFlagNotFoundResolutionError("no such flag")
	case openfeature.ProviderNotReadyCode:
		resErr = openfeature.NewProviderNotReadyResolutionError("first fetch has not completed")
	case openfeature.ProviderFatalCode:
		resErr = openfeature.NewProviderFatalResolutionError("unrecoverable")
	default:
		resErr = openfeature.NewGeneralResolutionError("something went wrong")
	}
	return openfeature.BoolResolutionDetail{
		Value: defaultValue,
		ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
			ResolutionError: resErr,
			Reason:          openfeature.ErrorReason,
		},
	}
}

// freshKey returns a flag key this process has no remembered error code for.
//
// The memory is per key and per process by design — that is what makes the
// steady state silent — so a test that asserts on a TRANSITION has to start from
// nothing, including on the second pass of a -count=2 run.
func freshKey(t *testing.T, key string) string {
	t.Helper()
	evaluationErrorCodes.Delete(key)
	t.Cleanup(func() { evaluationErrorCodes.Delete(key) })
	return key
}

// setCodeProvider binds a provider that fails every evaluation with code.
func setCodeProvider(t *testing.T, code openfeature.ErrorCode) {
	t.Helper()
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, codeProvider{code: code}); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	t.Cleanup(func() { clearProvider(t) })
}

// TestValue_LogsWhatWasInvisible is the whole reason evaluation reads the
// details variant.
//
// The two highest-severity findings of the August review were both silent for
// the same reason: a provider that failed to initialise reported
// PROVIDER_NOT_READY on every evaluation, and a missing targeting key reported
// TARGETING_KEY_MISSING on every evaluation. The error code was populated the
// whole time and nothing read it. The RETURN value stays indistinguishable
// between relay silence and relay failure — that is deliberate, and the ladder
// depends on it — but the log no longer is.
func TestValue_LogsWhatWasInvisible(t *testing.T) {
	buf := captureLogs(t)
	setCodeProvider(t, openfeature.TargetingKeyMissingCode)

	key := freshKey(t, "logs-targeting-key")
	r := NewResolver()
	if got := r.Value(key, true); !got {
		t.Fatalf("Value(true) = false on a failed evaluation; the local value must stand")
	}

	log := buf.String()
	if !strings.Contains(log, "level=WARN") {
		t.Fatalf("a broken evaluation was not reported at warn: %q", log)
	}
	for _, want := range []string{key, string(openfeature.TargetingKeyMissingCode)} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not name %q: %q", want, log)
		}
	}
}

// TestValue_LogsOncePerTransition keeps the diagnostic from becoming the noise
// it exists to cut through: an instrumented operation evaluates two flags, so a
// line per evaluation would be thousands per second under a relay outage.
func TestValue_LogsOncePerTransition(t *testing.T) {
	buf := captureLogs(t)
	setCodeProvider(t, openfeature.GeneralCode)

	key := freshKey(t, "logs-once")
	r := NewResolver()
	for range 5 {
		_ = r.Value(key, false)
	}

	if n := strings.Count(buf.String(), key); n != 1 {
		t.Fatalf("%d lines for five identical failures, want 1: %q", n, buf.String())
	}
}

func TestValue_LogsTheRecovery(t *testing.T) {
	buf := captureLogs(t)
	setCodeProvider(t, openfeature.GeneralCode)

	key := freshKey(t, "logs-recovery")
	r := NewResolver()
	_ = r.Value(key, false)

	// The relay comes back.
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, codeProvider{}); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	_ = r.Value(key, false)

	if !strings.Contains(buf.String(), "level=INFO") {
		t.Fatalf("the recovery was not reported: %q", buf.String())
	}
}

// TestValue_QuietWhenTheRelaySimplyHasNoOpinion covers the tier split.
//
// A deployment that creates the master kill switch and none of the module keys
// is entirely reasonable, and would emit a warning per module per process under
// a uniform rule. Logs that are noisy in the normal case train people to ignore
// them, which is the failure the diagnostic exists to prevent. The line survives
// at debug because it is the only signal available to someone who mistyped a key
// name on the relay.
func TestValue_QuietWhenTheRelaySimplyHasNoOpinion(t *testing.T) {
	for _, code := range []openfeature.ErrorCode{openfeature.FlagNotFoundCode, openfeature.ProviderNotReadyCode} {
		t.Run(string(code), func(t *testing.T) {
			buf := captureLogs(t)
			setCodeProvider(t, code)

			key := freshKey(t, "quiet-"+string(code))
			r := NewResolver()
			_ = r.Value(key, false)

			log := buf.String()
			if strings.Contains(log, "level=WARN") {
				t.Fatalf("%s was reported at warn; the relay having no opinion is a normal state: %q", code, log)
			}
			if !strings.Contains(log, key) {
				t.Errorf("%s was not reported at all; a mistyped key name would have no signal: %q", code, log)
			}
		})
	}
}

// TestCodeFromError covers the failures the SDK reports with NO error code.
//
// Client.evaluate short-circuits a domain in NOT_READY or FATAL state before it
// builds any resolution detail, so BooleanValueDetails hands back an empty
// ErrorCode next to a sentinel error. NOT_READY that way is not an exotic case:
// it is the whole startup window between a non-blocking install and the
// provider's first fetch, in every relay-configured process. Folding it into
// GENERAL reported that window at warn, as a fault — the exact noise
// quietErrorCodes exists to prevent.
func TestCodeFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want openfeature.ErrorCode
	}{
		{name: "the not-ready short circuit", err: openfeature.ProviderNotReadyError,
			want: openfeature.ProviderNotReadyCode},
		{name: "the fatal short circuit", err: openfeature.ProviderFatalError,
			want: openfeature.ProviderFatalCode},
		{name: "wrapped, as a hook error arrives",
			err:  fmt.Errorf("before hook: %w", openfeature.ProviderNotReadyError),
			want: openfeature.ProviderNotReadyCode},
		{name: "anything else", err: errors.New("boom"), want: openfeature.GeneralCode},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeFromError(tc.err); got != tc.want {
				t.Fatalf("codeFromError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestRecordEvaluation_ClassifiesTheCodelessShortCircuits is the same defect one
// level up: the tier the log lands in, not the code it derives.
func TestRecordEvaluation_ClassifiesTheCodelessShortCircuits(t *testing.T) {
	t.Run("a not-ready short circuit is quiet", func(t *testing.T) {
		buf := captureLogs(t)
		key := freshKey(t, "codeless-not-ready")

		recordEvaluation(key, "", openfeature.ProviderNotReadyError)

		log := buf.String()
		if strings.Contains(log, "level=WARN") {
			t.Fatalf("the startup window was reported at warn: %q", log)
		}
		if !strings.Contains(log, string(openfeature.ProviderNotReadyCode)) {
			t.Errorf("the log does not name PROVIDER_NOT_READY: %q", log)
		}
	})

	t.Run("an unrecognised error is still a fault", func(t *testing.T) {
		buf := captureLogs(t)
		key := freshKey(t, "codeless-general")

		recordEvaluation(key, "", errors.New("boom"))

		if !strings.Contains(buf.String(), "level=WARN") {
			t.Fatalf("an unexplained failure was not reported at warn: %q", buf.String())
		}
	})
}

// TestValue_EveryErrorCodeLeavesLocalInCharge closes the fallback contract over
// the full error-code vocabulary: whatever way an evaluation fails, and in
// whichever direction the local value points, the local value stands.
//
// The per-code tests above assert the logging; this one asserts the VALUE, so a
// future code path that handled one code specially — returning false on
// PROVIDER_FATAL, say — is caught even if its logging looks right.
func TestValue_EveryErrorCodeLeavesLocalInCharge(t *testing.T) {
	codes := []openfeature.ErrorCode{
		openfeature.FlagNotFoundCode,
		openfeature.ProviderNotReadyCode,
		openfeature.ProviderFatalCode,
		openfeature.TargetingKeyMissingCode,
		openfeature.GeneralCode,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			setCodeProvider(t, code)

			r := NewResolver()
			key := freshKey(t, "fallback-"+string(code))
			for _, local := range []bool{true, false} {
				if got := r.Value(key, local); got != local {
					t.Errorf("Value(%v) = %v under %s; the local value must stand", local, got, code)
				}
			}
		})
	}
}

// TestValue_TypeMismatchLeavesLocalInCharge covers the one failure the
// codeProvider cannot model faithfully: a relay that serves the key with the
// WRONG TYPE. The SDK detects the mismatch after the provider resolves, so this
// drives it through a real in-memory flag whose variants are strings.
func TestValue_TypeMismatchLeavesLocalInCharge(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{
		"string-typed-flag": {
			State:          memprovider.Enabled,
			DefaultVariant: "on",
			Variants:       map[string]any{"on": "definitely", "off": "nope"},
		},
	})

	r := NewResolver()
	key := freshKey(t, "string-typed-flag")
	for _, local := range []bool{true, false} {
		if got := r.Value(key, local); got != local {
			t.Errorf("Value(%v) = %v for a string-typed flag; the local value must stand", local, got)
		}
	}
}

// deadlineProbe records whether the evaluation context carried a deadline.
type deadlineProbe struct {
	openfeature.NoopProvider
	sawDeadline atomic.Bool
}

func (*deadlineProbe) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "DeadlineProbe"}
}

func (p *deadlineProbe) BooleanEvaluation(ctx context.Context, _ string, defaultValue bool,
	_ openfeature.FlattenedContext,
) openfeature.BoolResolutionDetail {
	_, ok := ctx.Deadline()
	p.sawDeadline.Store(ok)
	return openfeature.BoolResolutionDetail{Value: defaultValue}
}

// TestValue_DeadlineOnlyForApplicationProviders pins where the 250 ms bound
// applies. A provider the application installed can be anything, including one
// that evaluates over HTTP, so its evaluations carry a deadline and a stall
// falls through to the local value. The auto-installed provider evaluates in
// process and cannot block, so the deadline — pure overhead there — is skipped.
func TestValue_DeadlineOnlyForApplicationProviders(t *testing.T) {
	resetInstallState(t)
	probe := &deadlineProbe{}
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, probe); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver()
	key := freshKey(t, "deadline-probe")

	_ = r.Value(key, false)
	if !probe.sawDeadline.Load() {
		t.Errorf("no deadline on an application-installed provider's evaluation; " +
			"a stalled flag backend would block the instrumented operation indefinitely")
	}

	autoInstalled.Store(true)
	_ = r.Value(key, false)
	if probe.sawDeadline.Load() {
		t.Errorf("a deadline was built for the auto-installed provider, which evaluates in process and cannot block")
	}
}

// TestValue_FirstSuccessIsSilent keeps the ordinary case — every process, every
// flag, forever — from emitting a line.
func TestValue_FirstSuccessIsSilent(t *testing.T) {
	buf := captureLogs(t)
	setCodeProvider(t, "")

	key := freshKey(t, "silent-success")
	r := NewResolver()
	_ = r.Value(key, false)

	if strings.Contains(buf.String(), key) {
		t.Fatalf("a successful evaluation was logged: %q", buf.String())
	}
}

// --- provider auto-install -------------------------------------------------

// A syntactically valid endpoint that nothing listens on. Construction performs
// no I/O, and registration is non-blocking, so the provider's background fetch
// failing is exactly the "relay down at startup" path — logged, not fatal.
const unreachableEndpoint = "http://127.0.0.1:1"

func TestAutoInstall_FiresWhenEndpointSetAndNoProvider(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	mustValidate(t)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("no provider was installed on %q; want a GO Feature Flag provider", FlagDomain)
	}
}

// TestValue_DoesNotInstallAProvider pins where the install happens.
//
// It used to run inside the first evaluation, under installMu — the same mutex
// SetNamedProvider holds across a blocking provider initialisation. An
// application installing its own provider concurrently with the first
// instrumented operation could therefore park that operation for as long as the
// provider's HTTP timeout. Construction is where a wrapper is allowed to block;
// the hot path is not.
func TestValue_DoesNotInstallAProvider(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver()
	if got := r.Value("k", true); !got {
		t.Errorf("Value(true) = false with nothing bound; the local value must stand")
	}

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got != noopProviderName {
		t.Fatalf("evaluating installed %q; the install belongs to ValidateAndInstall, off the hot path", got)
	}
}

func TestAutoInstall_StandsDownWhenAProviderExists(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})
	resetInstallState(t)
	t.Cleanup(func() { resetInstallState(t) })
	before := openfeature.NamedProviderMetadata(FlagDomain).Name
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)

	mustValidate(t)

	r := NewResolver()
	if r.Value("k", true) {
		t.Fatalf("Value = true; the application's own provider must still decide")
	}
	if after := openfeature.NamedProviderMetadata(FlagDomain).Name; after != before {
		t.Fatalf("provider changed from %q to %q; the auto-install must stand down", before, after)
	}
}

func TestAutoInstall_UnsetEndpointInstallsNothing(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Cleanup(func() { resetInstallState(t) })

	mustValidate(t)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got != noopProviderName {
		t.Fatalf("provider = %q with no endpoint configured; want no OpenFeature state written", got)
	}
	if attrs := currentEvalCtx().Attributes(); len(attrs) != 0 {
		t.Fatalf("evaluation context = %v, want no attributes off the auto-install path", attrs)
	}
}

func TestAutoInstall_HappensOnceForEveryConstructor(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvServiceName, "checkout-api")
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	// Four wrappers, as a binary linking all four instrumentation modules would
	// hold, constructed concurrently. Each construction validates and installs.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ValidateAndInstall(); err != nil {
				t.Errorf("ValidateAndInstall: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("no provider installed")
	}
	// The install is remembered process-wide, so every module — not only the one
	// that won the race — evaluates with the targeting attribute.
	if got := currentEvalCtx().Attributes()["service.name"]; got != "checkout-api" {
		t.Errorf("service.name = %v, want checkout-api", got)
	}
}

func TestAutoInstall_ServiceNameOnlyOnThisPath(t *testing.T) {
	t.Run("attached when auto-installed", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		mustValidate(t)

		if got := currentEvalCtx().Attributes()["service.name"]; got != "checkout-api" {
			t.Fatalf("service.name = %v, want checkout-api", got)
		}
	})

	t.Run("absent when the application installed the provider", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")

		mustValidate(t)

		if attrs := currentEvalCtx().Attributes(); len(attrs) != 0 {
			t.Fatalf("evaluation context = %v, want no attributes; the application owns its own context", attrs)
		}
	})
}

// TestPollIntervalFromEnv pins the reversal decision 1 makes: a tuning value
// whose intent cannot be read now fails construction instead of warning and
// falling back.
//
// The old rule was keyed to how bad the consequence is — a switch is severe, an
// interval is not — and that has to be re-argued for every variable added. "Can
// the operator's intent be read" is decidable by looking at the value.
func TestPollIntervalFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses the default", want: defaultPollInterval},
		{name: "duration string", value: "5s", set: true, want: 5 * time.Second},
		{name: "minutes", value: "2m", set: true, want: 2 * time.Minute},
		// Blank is the absence of a value, not an unreadable one: a duration has
		// no second reading the way a boolean does, so there is nothing to guess
		// between. Contrast Lookup, where `export VAR=` is an error.
		{name: "blank is the same as unset", value: "  ", set: true, want: defaultPollInterval},
		// A bare integer must NOT be read as milliseconds: misreading a polling
		// interval that way turns 60 into 60ms.
		{name: "bare integer is rejected", value: "60", set: true, wantErr: true},
		{name: "garbage is rejected", value: "soon", set: true, wantErr: true},
		{name: "zero is rejected", value: "0s", set: true, wantErr: true},
		{name: "negative is rejected", value: "-5s", set: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(EnvFlagsPollInterval, tc.value)
			}
			got, err := pollIntervalFromEnv()

			if (err != nil) != tc.wantErr {
				t.Fatalf("pollIntervalFromEnv() err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidFlagValue) {
					t.Errorf("error does not wrap ErrInvalidFlagValue: %v", err)
				}
				if !strings.Contains(err.Error(), EnvFlagsPollInterval) {
					t.Errorf("error does not name the variable: %v", err)
				}
				if !strings.Contains(err.Error(), tc.value) {
					t.Errorf("error does not name the observed value: %v", err)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("pollIntervalFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEndpointFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		set     bool
		want    string
		wantErr bool
	}{
		{name: "unset means no relay"},
		{name: "blank means no relay", value: "   ", set: true},
		{name: "http", value: "http://relay:1031", set: true, want: "http://relay:1031"},
		{name: "https with a path", value: "https://flags.example.com/gofeatureflag", set: true,
			want: "https://flags.example.com/gofeatureflag"},
		{name: "surrounding whitespace is trimmed", value: "  http://relay:1031  ", set: true, want: "http://relay:1031"},

		// url.Parse accepts these; the provider cannot use them. A host:port with
		// no scheme parses as scheme "relay" with opaque "1031", which is why the
		// host is checked too.
		{name: "no scheme", value: "relay:1031", set: true, wantErr: true},
		{name: "bare host", value: "relay", set: true, wantErr: true},
		{name: "scheme with no host", value: "http://", set: true, wantErr: true},
		{name: "unparseable", value: "http://[::1", set: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(EnvFlagsEndpoint, tc.value)
			}
			got, err := endpointFromEnv()

			if (err != nil) != tc.wantErr {
				t.Fatalf("endpointFromEnv() err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidFlagValue) {
					t.Errorf("error does not wrap ErrInvalidFlagValue: %v", err)
				}
				if !strings.Contains(err.Error(), EnvFlagsEndpoint) {
					t.Errorf("error does not name the variable: %v", err)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("endpointFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidateAndInstall_RejectsUnreadableConfiguration is decision 1 at the
// entry point: every provider variable is validated whether or not a relay is
// configured, and an unreadable one fails construction rather than installing
// something the operator did not ask for.
func TestValidateAndInstall_RejectsUnreadableConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		interval string
		wantVars []string
	}{
		{name: "a bare integer poll interval, with no relay configured",
			interval: "60", wantVars: []string{EnvFlagsPollInterval}},
		{name: "a garbage poll interval alongside a good endpoint",
			endpoint: unreachableEndpoint, interval: "soon", wantVars: []string{EnvFlagsPollInterval}},
		{name: "a non-positive poll interval",
			endpoint: unreachableEndpoint, interval: "0s", wantVars: []string{EnvFlagsPollInterval}},
		{name: "an endpoint with no scheme",
			endpoint: "relay:1031", wantVars: []string{EnvFlagsEndpoint}},

		// Both are read before either is reported: a deployment carrying two bad
		// values must not have to fix one to discover the other.
		{name: "both unreadable are reported together",
			endpoint: "relay:1031", interval: "60",
			wantVars: []string{EnvFlagsEndpoint, EnvFlagsPollInterval}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearProvider(t)
			resetInstallState(t)
			t.Cleanup(func() { clearProvider(t); resetInstallState(t) })
			if tc.endpoint != "" {
				t.Setenv(EnvFlagsEndpoint, tc.endpoint)
			}
			if tc.interval != "" {
				t.Setenv(EnvFlagsPollInterval, tc.interval)
			}

			err := ValidateAndInstall()
			if !errors.Is(err, ErrInvalidFlagValue) {
				t.Fatalf("ValidateAndInstall() err = %v, want ErrInvalidFlagValue", err)
			}
			for _, v := range tc.wantVars {
				if !strings.Contains(err.Error(), v) {
					t.Errorf("error does not name %s: %v", v, err)
				}
			}
			if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got != noopProviderName {
				t.Errorf("provider %q was installed despite unreadable configuration", got)
			}
		})
	}
}

func TestValidateAndInstall_AcceptsWhatItCanRead(t *testing.T) {
	t.Run("nothing configured installs nothing", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })

		mustValidate(t)

		if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got != noopProviderName {
			t.Fatalf("provider = %q with nothing configured; want no OpenFeature state written", got)
		}
	})

	t.Run("an endpoint installs the provider before any evaluation", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvFlagsPollInterval, "5s")
		// Any string is a legitimate API key, so it is never validated.
		t.Setenv(EnvFlagsAPIKey, "%%not-a-url%%")
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		mustValidate(t)

		if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
			t.Fatalf("no provider installed on %q", FlagDomain)
		}
	})
}

// TestValidateAndInstall_IsIdempotent pins the latch: a wrapper constructed
// second must not rebind the domain underneath the first.
func TestValidateAndInstall_IsIdempotent(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	mustValidate(t)
	installed := openfeature.NamedProviderMetadata(FlagDomain).Name

	// The application takes the domain over afterwards, as it may.
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, memprovider.NewInMemoryProvider(
		map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}

	mustValidate(t)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == installed {
		t.Fatalf("the second ValidateAndInstall rebound %q over the application's provider", got)
	}
}

func TestJitterInterval(t *testing.T) {
	const (
		base    = 60 * time.Second
		samples = 500
	)
	maxJitter := time.Duration(float64(base) * pollJitterFraction)
	lo, hi := base-maxJitter, base+maxJitter

	seen := make(map[time.Duration]struct{}, samples)
	for range samples {
		got := jitterInterval(base)
		if got < lo || got > hi {
			t.Fatalf("jitterInterval(%v) = %v, want within [%v, %v]", base, got, lo, hi)
		}
		// Upstream takes the sign from the magnitude's parity in nanoseconds; an
		// even magnitude is added, an odd one subtracted. Pinned so a rewrite that
		// silently diverges from newBackgroundUpdater is visible here.
		magnitude := got - base
		if magnitude < 0 {
			magnitude = -magnitude
		}
		if wantAdded := magnitude%2 == 0; wantAdded != (got >= base) {
			t.Fatalf("jitterInterval(%v) = %v: magnitude %v is even=%v, so it should have been %s",
				base, got, magnitude, wantAdded, map[bool]string{true: "added", false: "subtracted"}[wantAdded])
		}
		seen[got] = struct{}{}
	}
	// A constant would satisfy the bounds check above while leaving the fleet in
	// lockstep, which is the whole point of the function.
	if len(seen) == 1 {
		t.Fatalf("jitterInterval returned the same value %v across %d calls; no jitter was applied",
			base, samples)
	}
}

func TestJitterInterval_NonPositiveAndTinyIntervalsPassThrough(t *testing.T) {
	// Below ten nanoseconds the magnitude truncates to zero, and rand.Int64N
	// panics on a non-positive bound.
	for _, d := range []time.Duration{-time.Second, 0, 1, 9} {
		if got := jitterInterval(d); got != d {
			t.Errorf("jitterInterval(%v) = %v, want it returned unchanged", d, got)
		}
	}
}

func TestResolveLocal(t *testing.T) {
	ptr := func(v bool) *bool { return &v }

	tests := []struct {
		name    string
		option  *bool
		env     string
		envSet  bool
		def     bool
		want    bool
		wantErr bool
	}{
		{name: "nothing set falls to the default", def: true, want: true},
		{name: "nothing set falls to a false default"},

		{name: "the option beats the default", option: ptr(true), def: false, want: true},
		{name: "the option beats a true default", option: ptr(false), def: true},

		// The ordering the whole ladder is built around: an operator's variable
		// must survive application code that asked for the opposite.
		{name: "the env beats the option", option: ptr(true), env: "false", envSet: true, def: false},
		{name: "the env beats the option the other way", option: ptr(false), env: "true", envSet: true, def: false, want: true},
		{name: "the env beats the default", env: "true", envSet: true, def: false, want: true},

		// An unreadable variable is an error even when an option supplied a
		// perfectly good value: the operator's intent is unknown, not absent.
		{name: "an invalid env is an error", env: "maybe", envSet: true, def: false, wantErr: true},
		{name: "an invalid env is an error despite an option", option: ptr(true), env: "maybe", envSet: true, def: false, wantErr: true},
		{name: "an empty env is an error", env: "", envSet: true, def: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv(testEnvKey, tc.env)
			}
			got, err := ResolveLocal(tc.option, testEnvKey, tc.def)

			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveLocal err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidFlagValue) {
					t.Errorf("error does not wrap ErrInvalidFlagValue: %v", err)
				}
				if got {
					t.Errorf("ResolveLocal = true alongside an error; the caller must not be handed a usable value")
				}
				return
			}
			if got != tc.want {
				t.Fatalf("ResolveLocal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValue_SuppliesATargetingKey(t *testing.T) {
	t.Run("after the install", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })

		mustValidate(t)

		// Without one, every percentage and progressiveRollout rule fails with
		// TARGETING_KEY_MISSING and silently resolves to the local value.
		if got := currentEvalCtx().TargetingKey(); got != processTargetingKey {
			t.Fatalf("targeting key = %q, want the process key %q; bucketing rules on the relay "+
				"cannot apply to this process", got, processTargetingKey)
		}
	})

	t.Run("even with no install at all", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })

		// An application that binds FlagDomain directly never reaches the install
		// path, and its evaluations must still bucket.
		if got := currentEvalCtx().TargetingKey(); got != processTargetingKey {
			t.Fatalf("targeting key = %q, want the process key %q", got, processTargetingKey)
		}
	})
}

func TestAutoInstall_ServiceNameIsMatchableByARule(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvServiceName, "checkout-api")
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	mustValidate(t)

	// A dot is a nested-path separator in both query languages the relay
	// supports, so service.name alone is unmatchable however it is written.
	if got := currentEvalCtx().Attributes()["serviceName"]; got != "checkout-api" {
		t.Fatalf("serviceName = %v, want checkout-api; no relay rule can target this process", got)
	}
	if got := currentEvalCtx().Attributes()["service.name"]; got != "checkout-api" {
		t.Errorf("service.name = %v, want it kept alongside the matchable spelling", got)
	}
}

func TestProviderBound(t *testing.T) {
	t.Run("a no-op provider on the domain is not a binding", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })

		if providerBound() {
			t.Fatalf("providerBound() = true for NoopProvider")
		}
	})

	t.Run("an application's default provider is not a binding", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		setDefaultProvider(t, "business-flag")
		t.Cleanup(func() { resetInstallState(t) })

		if providerBound() {
			t.Fatalf("providerBound() = true for a provider in the DEFAULT slot; " +
				"instrumentation keys would be evaluated against the application's own flags")
		}
	})

	t.Run("a provider on the domain is a binding", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })

		if !providerBound() {
			t.Fatalf("providerBound() = false for a provider bound to %q", FlagDomain)
		}
	})
}

func TestValue_DoesNotReachTheDefaultProvider(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	// The application's own flag backend, which happens to define a key by the
	// same name as one of ours.
	setDefaultProvider(t, "k")
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver()
	if r.Value("k", false) {
		t.Fatalf("Value consulted the application's default provider; the local value must decide "+
			"when nothing is bound to %q", FlagDomain)
	}
}

func TestSetNamedProvider_LatchesTheAutoInstallShut(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	own := memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
	if err := SetNamedProvider(own); err != nil {
		t.Fatalf("SetNamedProvider: %v", err)
	}

	// A wrapper constructed afterwards validates and would auto-install, the
	// endpoint being set.
	mustValidate(t)

	r := NewResolver()
	if !r.Value("k", false) {
		t.Fatalf("the application's own provider was not consulted")
	}
	if got, want := openfeature.NamedProviderMetadata(FlagDomain).Name, own.Metadata().Name; got != want {
		t.Fatalf("provider bound to %q, want the application's own %q; the auto-install fired behind it", got, want)
	}

	installMu.Lock()
	done := installDone
	installMu.Unlock()
	if !done {
		t.Errorf("SetNamedProvider did not latch installDone, so a later auto-install can still replace the application's provider")
	}
}

func TestRebindRelayProvider_StandsDown(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	gen := installGen.Load()
	bound := openfeature.NamedProviderMetadata(FlagDomain).Name

	// The domain is held by a provider that is not the auto-installed one. A
	// watchdog must never replace it — an application can bind FlagDomain
	// directly, without going through SetNamedProvider, and leaves no record.
	if rebindRelayProvider("GO Feature Flag Provider", unreachableEndpoint, defaultPollInterval, gen) {
		t.Errorf("rebindRelayProvider was willing to steal a domain owned by %q", bound)
	}

	explicitBind.Store(true)
	if rebindRelayProvider(bound, unreachableEndpoint, defaultPollInterval, gen) {
		t.Errorf("rebindRelayProvider ran after an explicit SetNamedProvider call")
	}
	explicitBind.Store(false)

	if rebindRelayProvider(bound, unreachableEndpoint, defaultPollInterval, gen-1) {
		t.Errorf("a retired watchProviderInit goroutine kept rebinding")
	}
}

func TestValidateAndInstall_APIKeyIsNeverReported(t *testing.T) {
	const secret = "super-secret-relay-key"

	t.Run("not in the error from a neighbouring bad value", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		buf := captureLogs(t)
		t.Setenv(EnvFlagsEndpoint, "relay:1031") // fails validation, and names itself
		t.Setenv(EnvFlagsAPIKey, secret)
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		err := ValidateAndInstall()
		if err == nil {
			t.Fatalf("ValidateAndInstall() = nil for an endpoint with no scheme")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the API key appeared in an error: %v", err)
		}
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("the API key appeared in a log line: %q", buf.String())
		}
	})

	t.Run("not on the install path", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		buf := captureLogs(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvFlagsAPIKey, secret)
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		mustValidate(t)

		if strings.Contains(buf.String(), secret) {
			t.Fatalf("the API key appeared in a log line: %q", buf.String())
		}
	})
}

func TestVersion(t *testing.T) {
	if Version() != instrumentationVersion {
		t.Fatalf("Version() = %q, want %q", Version(), instrumentationVersion)
	}
}
