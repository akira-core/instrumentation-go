package flags

import (
	"bytes"
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

func TestEnvEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset", want: false},

		{name: "one", value: "1", set: true, want: true},
		{name: "true", value: "true", set: true, want: true},
		{name: "yes", value: "yes", set: true, want: true},
		{name: "on", value: "on", set: true, want: true},
		{name: "upper TRUE", value: "TRUE", set: true, want: true},
		{name: "mixed On", value: "On", set: true, want: true},
		{name: "padded yes", value: "  yes  ", set: true, want: true},

		{name: "zero", value: "0", set: true, want: false},
		{name: "false", value: "false", set: true, want: false},
		{name: "no", value: "no", set: true, want: false},
		{name: "off", value: "off", set: true, want: false},
		{name: "upper FALSE", value: "FALSE", set: true, want: false},

		// The rows that changed in this release. Each one enabled the switch
		// before the allow-list.
		{name: "empty string", value: "", set: true, want: false},
		{name: "enabled word", value: "enabled", set: true, want: false},
		{name: "two", value: "2", set: true, want: false},
		{name: "y", value: "y", set: true, want: false},
		{name: "t", value: "t", set: true, want: false},
		{name: "arbitrary string", value: "hello", set: true, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(testEnvKey, tc.value)
			}
			if got := EnvEnabled(testEnvKey); got != tc.want {
				t.Fatalf("EnvEnabled(%q=%q) = %v, want %v", testEnvKey, tc.value, got, tc.want)
			}
		})
	}
}

// captureLogs redirects slog's default logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestEnvEnabled_WarnsOnlyOnUnrecognisedValue(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		set      bool
		wantWarn bool
	}{
		{name: "unset is silent", wantWarn: false},
		{name: "truthy is silent", value: "true", set: true, wantWarn: false},
		{name: "explicitly falsy is silent", value: "off", set: true, wantWarn: false},
		{name: "empty string is silent", value: "", set: true, wantWarn: false},
		{name: "unrecognised warns", value: "enabled", set: true, wantWarn: true},
		{name: "numeric two warns", value: "2", set: true, wantWarn: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			if tc.set {
				t.Setenv(testEnvKey, tc.value)
			}
			_ = EnvEnabled(testEnvKey)

			got := strings.Contains(buf.String(), "unrecognised boolean value")
			if got != tc.wantWarn {
				t.Fatalf("warning emitted = %v, want %v (log: %q)", got, tc.wantWarn, buf.String())
			}
			if tc.wantWarn {
				if !strings.Contains(buf.String(), testEnvKey) {
					t.Errorf("warning does not name the variable: %q", buf.String())
				}
				if !strings.Contains(buf.String(), tc.value) {
					t.Errorf("warning does not name the observed value: %q", buf.String())
				}
			}
		})
	}
}

func TestEnvSet_DistinguishesUnsetFromFalsy(t *testing.T) {
	if EnvSet(testEnvKey) {
		t.Fatalf("EnvSet reported an unset variable as present")
	}

	t.Setenv(testEnvKey, "false")
	if !EnvSet(testEnvKey) {
		t.Fatalf("EnvSet(%q=false) = false, want true", testEnvKey)
	}
	if EnvEnabled(testEnvKey) {
		t.Fatalf("EnvEnabled(%q=false) = true, want false", testEnvKey)
	}
}

func TestGlobalTracingHelpers(t *testing.T) {
	if GlobalTracingSet() || GlobalTracingPossible() {
		t.Fatalf("expected the global switch to be unset in the test environment")
	}
	t.Setenv(EnvGlobalTracing, "0")
	if !GlobalTracingSet() {
		t.Errorf("GlobalTracingSet() = false for an explicitly falsy value")
	}
	if GlobalTracingPossible() {
		t.Errorf("GlobalTracingPossible() = true for an explicitly falsy value")
	}
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
// It binds the NAMED domain, not the default provider. A named provider
// outranks the default for the clients this package creates, so a test that
// installed a default provider would be silently shadowed the moment anything
// in the process had auto-installed — and clientOnce makes that unrepeatable.
func setProvider(t *testing.T, flags map[string]memprovider.InMemoryFlag) {
	t.Helper()
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, memprovider.NewInMemoryProvider(flags)); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	t.Cleanup(func() { clearProvider(t) })
}

// clearProvider rebinds FlagDomain to the no-op provider, modelling an
// application that never wires OpenFeature at all.
func clearProvider(t *testing.T) {
	t.Helper()
	if err := openfeature.SetNamedProviderAndWait(FlagDomain, openfeature.NoopProvider{}); err != nil {
		t.Fatalf("SetNamedProviderAndWait(Noop): %v", err)
	}
}

func TestResolver_NoProviderAllows(t *testing.T) {
	clearProvider(t)

	r := NewResolver(WithFlagKeys("some-key"))
	if !r.Allowed(0) {
		t.Fatalf("Allowed = false with no provider installed; an unresolvable flag must mean 'do not interfere'")
	}
}

func TestResolver_MissingFlagAllows(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"other-key": boolFlag(false)})

	r := NewResolver(WithFlagKeys("absent-key"))
	if !r.Allowed(0) {
		t.Fatalf("Allowed = false for a key absent from the relay configuration; want true")
	}
}

func TestResolver_RelayCanRevoke(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})

	r := NewResolver(WithFlagKeys("k"))
	if r.Allowed(0) {
		t.Fatalf("Allowed = true while the relay serves false")
	}
}

func TestResolver_RevocationIsVisibleOnTheNextCall(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

	r := NewResolver(WithFlagKeys("k"))
	if !r.Allowed(0) {
		t.Fatalf("Allowed = false while the relay serves true")
	}

	// Re-bind with the flag revoked. No sleep, no clock, no reset hook: the
	// resolver caches nothing, so the very next call must observe it.
	if err := openfeature.SetNamedProviderAndWait(FlagDomain,
		memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})); err != nil {
		t.Fatalf("SetNamedProviderAndWait: %v", err)
	}
	if r.Allowed(0) {
		t.Fatalf("Allowed = true on the call after a revocation; the verdict must not be cached")
	}
}

func TestResolver_OutOfRangeIndexIsDisabled(t *testing.T) {
	clearProvider(t)

	r := NewResolver(WithFlagKeys("k"))
	for _, i := range []int{-1, 1, 99} {
		if r.Allowed(i) {
			t.Errorf("Allowed(%d) = true for an out-of-range index; a mis-wired module must degrade to disabled", i)
		}
	}
}

func TestResolver_NilOptionIsSkipped(t *testing.T) {
	r := NewResolver(nil, WithFlagKeys("k"), nil)
	if len(r.keys) != 1 {
		t.Fatalf("keys = %v, want one key; a nil option must be skipped rather than panic", r.keys)
	}
}

func TestResolver_ConcurrentAllowed(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})

	r := NewResolver(WithFlagKeys("k"))
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Allowed(0)
		}()
	}
	wg.Wait()
}

// --- provider auto-install -------------------------------------------------

// A syntactically valid endpoint that nothing listens on. Construction performs
// no I/O, and registration is non-blocking, so the provider's background fetch
// failing is exactly the "relay down at startup" path — logged, not fatal.
const unreachableEndpoint = "http://127.0.0.1:1"

func TestAutoInstall_FiresWhenEndpointSetAndNoProvider(t *testing.T) {
	clearProvider(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Cleanup(func() { clearProvider(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Allowed(0)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("no provider was installed on %q; want a GO Feature Flag provider", FlagDomain)
	}
}

func TestAutoInstall_StandsDownWhenAProviderExists(t *testing.T) {
	setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(false)})
	before := openfeature.NamedProviderMetadata(FlagDomain).Name
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)

	r := NewResolver(WithFlagKeys("k"))
	if r.Allowed(0) {
		t.Fatalf("Allowed = true; the application's own provider must still decide")
	}
	if after := openfeature.NamedProviderMetadata(FlagDomain).Name; after != before {
		t.Fatalf("provider changed from %q to %q; the auto-install must stand down", before, after)
	}
}

func TestAutoInstall_UnsetEndpointInstallsNothing(t *testing.T) {
	clearProvider(t)

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Allowed(0)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got != noopProviderName {
		t.Fatalf("provider = %q with no endpoint configured; want no OpenFeature state written", got)
	}
	if len(r.evalCtx.Attributes()) != 0 {
		t.Fatalf("evaluation context = %v, want empty off the auto-install path", r.evalCtx.Attributes())
	}
}

func TestAutoInstall_ServiceNameOnlyOnThisPath(t *testing.T) {
	t.Run("attached when auto-installed", func(t *testing.T) {
		clearProvider(t)
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")
		t.Cleanup(func() { clearProvider(t) })

		r := NewResolver(WithFlagKeys("k"))
		_ = r.Allowed(0)

		if got := r.evalCtx.Attributes()["service.name"]; got != "checkout-api" {
			t.Fatalf("service.name = %v, want checkout-api", got)
		}
	})

	t.Run("absent when the application installed the provider", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"k": boolFlag(true)})
		t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
		t.Setenv(EnvServiceName, "checkout-api")

		r := NewResolver(WithFlagKeys("k"))
		_ = r.Allowed(0)

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

func TestAutoInstall_MalformedIntervalStillInstalls(t *testing.T) {
	clearProvider(t)
	buf := captureLogs(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvFlagsPollInterval, "not-a-duration")
	t.Cleanup(func() { clearProvider(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Allowed(0)

	if got := openfeature.NamedProviderMetadata(FlagDomain).Name; got == noopProviderName {
		t.Fatalf("a malformed poll interval prevented the install; a typo in an optional " +
			"tuning value must not delete the kill switch")
	}
	if !strings.Contains(buf.String(), EnvFlagsPollInterval) {
		t.Errorf("the malformed value was not reported: %q", buf.String())
	}
}

func TestAutoInstall_APIKeyIsNeverLogged(t *testing.T) {
	const secret = "super-secret-relay-key"
	clearProvider(t)
	buf := captureLogs(t)
	t.Setenv(EnvFlagsEndpoint, unreachableEndpoint)
	t.Setenv(EnvFlagsAPIKey, secret)
	t.Setenv(EnvFlagsPollInterval, "also-broken") // force the warning path
	t.Cleanup(func() { clearProvider(t) })

	r := NewResolver(WithFlagKeys("k"))
	_ = r.Allowed(0)

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("the API key appeared in a log line: %q", buf.String())
	}
}
