package otelflags

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
	installEvalCtx = openfeature.EvaluationContext{}
	explicitBind.Store(false)
	installGen.Add(1)
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

	r := NewResolver(WithFlagKeys("some-key"))
	for _, local := range []bool{true, false} {
		if got := r.Value(0, local); got != local {
			t.Fatalf("Value(0, %v) = %v with no provider installed; the local value must stand", local, got)
		}
	}
}

func TestValue_MissingFlagReturnsLocal(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"other-key": boolFlag(false)})

	r := NewResolver(WithFlagKeys("absent-key"))
	for _, local := range []bool{true, false} {
		if got := r.Value(0, local); got != local {
			t.Fatalf("Value(0, %v) = %v for a key absent from the relay; the local value must stand", local, got)
		}
	}
}

func TestValue_RelayOverridesLocalInBothDirections(t *testing.T) {
	t.Run("relay disables what the deployment enabled", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})

		r := NewResolver(WithFlagKeys("k"))
		if r.Value(0, true) {
			t.Fatalf("Value(0, true) = true while the relay serves false")
		}
	})

	t.Run("relay enables what the deployment left off", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

		r := NewResolver(WithFlagKeys("k"))
		if !r.Value(0, false) {
			t.Fatalf("Value(0, false) = false while the relay serves true; the relay is authoritative in both directions")
		}
	})
}

func TestValue_ChangeIsVisibleOnTheNextCall(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

	r := NewResolver(WithFlagKeys("k"))
	if !r.Value(0, false) {
		t.Fatalf("Value = false while the relay serves true")
	}

	// Re-bind with the flag flipped. No sleep, no clock, no reset hook: the
	// resolver caches nothing, so the very next call must observe it.
	if err := openfeature.SetNamedProviderAndWait(FlagDomain,
		memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	if r.Value(0, true) {
		t.Fatalf("Value = true on the call after a change; the value must not be cached")
	}
}

func TestValue_OutOfRangeIndexIsDisabled(t *testing.T) {
	clearProvider(t)

	r := NewResolver(WithFlagKeys("k"))
	for _, i := range []int{-1, 1, 99} {
		// local=true so a pass-through implementation would return true; only a
		// genuine bounds check returns false here.
		if r.Value(i, true) {
			t.Errorf("Value(%d, true) = true for an out-of-range index; a mis-wired module must degrade to disabled", i)
		}
	}
}

func TestNewResolver_NilOptionIsSkipped(t *testing.T) {
	r := NewResolver(nil, WithFlagKeys("k"), nil)
	if len(r.keys) != 1 {
		t.Fatalf("keys = %v, want one key; a nil option must be skipped rather than panic", r.keys)
	}
}

func TestValue_Concurrent(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

	r := NewResolver(WithFlagKeys("k"))
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Value(0, false)
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
		// indistinguishable from the fallback. InstallProvider closes it.
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

func TestInstallProvider(t *testing.T) {
	t.Run("binds the domain and records the choice", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		if err := InstallProvider(memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})); err != nil {
			t.Fatalf("InstallProvider: %v", err)
		}

		if !RelayPossible() {
			t.Fatalf("RelayPossible() = false after InstallProvider")
		}
		r := NewResolver(WithFlagKeys("k"))
		if !r.Value(0, false) {
			t.Fatalf("Value(0, false) = false; the installed provider must decide")
		}
	})

	t.Run("the record survives a provider that reads back as the default", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		setDefaultProvider(t, "checkout-experiment")
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		// Same provider type in both slots — the heuristic alone reads this as
		// unbound. The explicit record is what makes it exact.
		if err := InstallProvider(memprovider.NewInMemoryProvider(
			map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})); err != nil {
			t.Fatalf("InstallProvider: %v", err)
		}
		if !RelayPossible() {
			t.Fatalf("RelayPossible() = false after an explicit install; the record must outrank the heuristic")
		}
	})

	t.Run("a nil provider is an error, not a panic", func(t *testing.T) {
		if err := InstallProvider(nil); err == nil {
			t.Fatalf("InstallProvider(nil) = nil, want an error")
		}
	})
}

func TestAutoInstall_ProceedsWhenOnlyADefaultProviderExists(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	setDefaultProvider(t, "checkout-experiment")
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

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

	r := NewResolver(WithFlagKeys("k"))
	for _, local := range []bool{true, false} {
		if got := r.Value(0, local); got != local {
			t.Fatalf("Value(0, %v) = %v before the provider's first fetch; the local value must stand", local, got)
		}
	}

	if got := MasterEnabled(true); !got {
		t.Fatalf("MasterEnabled(true) = false before the provider's first fetch; "+
			"a provider with nothing to say must not veto the master (got %v)", got)
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

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("no provider was installed on %q; want a GO Feature Flag provider", FlagDomain)
	}
}

func TestAutoInstall_StandsDownWhenAProviderExists(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})
	resetInstallState(t)
	t.Cleanup(func() { resetInstallState(t) })
	before := openfeature.NamedProviderMetadata(FlagDomain).Name
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)

	r := NewResolver(WithFlagKeys("k"))
	if r.Value(0, true) {
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

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got != noopProviderName {
		t.Fatalf("provider = %q with no endpoint configured; want no OpenFeature state written", got)
	}
	if len(r.evalCtx.Attributes()) != 0 {
		t.Fatalf("evaluation context = %v, want empty off the auto-install path", r.evalCtx.Attributes())
	}
}

func TestAutoInstall_HappensOnceForEveryResolver(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvServiceName, "checkout-api")
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	// Four module resolvers, as a binary linking all four instrumentation
	// modules would hold, evaluating for the first time concurrently.
	resolvers := []*Resolver{
		NewResolver(WithFlagKeys("otel-mongo-tracing")),
		NewResolver(WithFlagKeys("otel-nats-tracing")),
		NewResolver(WithFlagKeys("otel-gorilla-ws-tracing")),
		NewResolver(WithFlagKeys("k")),
	}
	var wg sync.WaitGroup
	for _, r := range resolvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Value(0, false)
		}()
	}
	wg.Wait()

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("no provider installed")
	}
	// The install is remembered, so every resolver — not only the one that won
	// the race — evaluates with the targeting attribute.
	for i, r := range resolvers {
		if got := r.evalCtx.Attributes()["service.name"]; got != "checkout-api" {
			t.Errorf("resolver %d: service.name = %v, want checkout-api", i, got)
		}
	}
}

func TestAutoInstall_ServiceNameOnlyOnThisPath(t *testing.T) {
	t.Run("attached when auto-installed", func(t *testing.T) {
		clearProvider(t)
		resetInstallState(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")
		t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

		r := NewResolver(WithFlagKeys("k"))
		_ = r.Value(0, false)

		if got := r.evalCtx.Attributes()["service.name"]; got != "checkout-api" {
			t.Fatalf("service.name = %v, want checkout-api", got)
		}
	})

	t.Run("absent when the application installed the provider", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
		resetInstallState(t)
		t.Cleanup(func() { resetInstallState(t) })
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")

		r := NewResolver(WithFlagKeys("k"))
		_ = r.Value(0, false)

		if len(r.evalCtx.Attributes()) != 0 {
			t.Fatalf("evaluation context = %v, want empty; the application owns its own context",
				r.evalCtx.Attributes())
		}
	})
}

func TestPollIntervalFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		set      bool
		want     time.Duration
		wantWarn bool
	}{
		{name: "unset uses the default", want: defaultPollInterval},
		{name: "duration string", value: "5s", set: true, want: 5 * time.Second},
		{name: "minutes", value: "2m", set: true, want: 2 * time.Minute},
		// A bare integer must NOT be read as milliseconds: misreading a polling
		// interval that way turns 60 into 60ms.
		{name: "bare integer is rejected", value: "60", set: true, want: defaultPollInterval, wantWarn: true},
		{name: "garbage is rejected", value: "soon", set: true, want: defaultPollInterval, wantWarn: true},
		{name: "zero is rejected", value: "0s", set: true, want: defaultPollInterval, wantWarn: true},
		{name: "negative is rejected", value: "-5s", set: true, want: defaultPollInterval, wantWarn: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			if tc.set {
				t.Setenv(EnvFlagsPollInterval, tc.value)
			}
			if got := pollIntervalFromEnv(); got != tc.want {
				t.Fatalf("pollIntervalFromEnv() = %v, want %v", got, tc.want)
			}
			if warned := strings.Contains(buf.String(), EnvFlagsPollInterval); warned != tc.wantWarn {
				t.Fatalf("warning emitted = %v, want %v (log: %q)", warned, tc.wantWarn, buf.String())
			}
		})
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
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
	resetInstallState(t)
	t.Cleanup(func() { resetInstallState(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

	// Without one, every percentage and progressiveRollout rule fails with
	// TARGETING_KEY_MISSING and silently resolves to the local value.
	if r.evalCtx.TargetingKey() == "" {
		t.Fatalf("no targeting key; bucketing rules on the relay cannot apply to this process")
	}
	if got := r.evalCtx.TargetingKey(); got != processTargetingKey {
		t.Errorf("targeting key = %q, want the process key %q", got, processTargetingKey)
	}
}

func TestAutoInstall_ServiceNameIsMatchableByARule(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvServiceName, "checkout-api")
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

	// A dot is a nested-path separator in both query languages the relay
	// supports, so service.name alone is unmatchable however it is written.
	if got := r.evalCtx.Attributes()["serviceName"]; got != "checkout-api" {
		t.Fatalf("serviceName = %v, want checkout-api; no relay rule can target this process", got)
	}
	if got := r.evalCtx.Attributes()["service.name"]; got != "checkout-api" {
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

	r := NewResolver(WithFlagKeys("k"))
	if r.Value(0, false) {
		t.Fatalf("Value consulted the application's default provider; the local value must decide "+
			"when nothing is bound to %q", FlagDomain)
	}
}

func TestInstallProvider_LatchesTheAutoInstallShut(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	own := memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
	if err := InstallProvider(own); err != nil {
		t.Fatalf("InstallProvider: %v", err)
	}

	r := NewResolver(WithFlagKeys("k"))
	if !r.Value(0, false) {
		t.Fatalf("the application's own provider was not consulted")
	}
	if got, want := openfeature.NamedProviderMetadata(FlagDomain).Name, own.Metadata().Name; got != want {
		t.Fatalf("provider bound to %q, want the application's own %q; the auto-install fired behind it", got, want)
	}

	installMu.Lock()
	done := installDone
	installMu.Unlock()
	if !done {
		t.Errorf("InstallProvider did not latch installDone, so a later auto-install can still replace the application's provider")
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
	// directly, without going through InstallProvider, and leaves no record.
	if rebindRelayProvider("GO Feature Flag Provider", unreachableEndpoint, defaultPollInterval, gen) {
		t.Errorf("rebindRelayProvider was willing to steal a domain owned by %q", bound)
	}

	explicitBind.Store(true)
	if rebindRelayProvider(bound, unreachableEndpoint, defaultPollInterval, gen) {
		t.Errorf("rebindRelayProvider ran after an explicit InstallProvider call")
	}
	explicitBind.Store(false)

	if rebindRelayProvider(bound, unreachableEndpoint, defaultPollInterval, gen-1) {
		t.Errorf("a retired watchProviderInit goroutine kept rebinding")
	}
}

func TestAutoInstall_MalformedIntervalStillInstalls(t *testing.T) {
	clearProvider(t)
	resetInstallState(t)
	buf := captureLogs(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvFlagsPollInterval, "not-a-duration")
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("a malformed poll interval prevented the install; a typo in an optional " +
			"tuning value must not delete the control plane")
	}
	if !strings.Contains(buf.String(), EnvFlagsPollInterval) {
		t.Errorf("the malformed value was not reported: %q", buf.String())
	}
}

func TestAutoInstall_APIKeyIsNeverLogged(t *testing.T) {
	const secret = "super-secret-relay-key"
	clearProvider(t)
	resetInstallState(t)
	buf := captureLogs(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvFlagsAPIKey, secret)
	t.Setenv(EnvFlagsPollInterval, "also-broken") // force the warning path
	t.Cleanup(func() { clearProvider(t); resetInstallState(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Value(0, false)

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("the API key appeared in a log line: %q", buf.String())
	}
}

func TestVersion(t *testing.T) {
	if Version() != instrumentationVersion {
		t.Fatalf("Version() = %q, want %q", Version(), instrumentationVersion)
	}
}
