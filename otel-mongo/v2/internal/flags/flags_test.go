package flags

import (
	"sync"
	"testing"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
)

func TestEnvEnabled(t *testing.T) {
	const key = "OTELMONGO_TEST_FLAGS_ENVENABLED"

	tests := []struct {
		name  string
		setup func(t *testing.T)
		want  bool
	}{
		{name: "unset", setup: func(t *testing.T) {}, want: false},
		{name: "explicit zero", setup: func(t *testing.T) { t.Setenv(key, "0") }, want: false},
		{name: "lower false", setup: func(t *testing.T) { t.Setenv(key, "false") }, want: false},
		{name: "upper FALSE", setup: func(t *testing.T) { t.Setenv(key, "FALSE") }, want: false},
		{name: "no", setup: func(t *testing.T) { t.Setenv(key, "no") }, want: false},
		{name: "off", setup: func(t *testing.T) { t.Setenv(key, "off") }, want: false},
		{name: "padded zero", setup: func(t *testing.T) { t.Setenv(key, " 0 ") }, want: false},
		{name: "one", setup: func(t *testing.T) { t.Setenv(key, "1") }, want: true},
		{name: "lower true", setup: func(t *testing.T) { t.Setenv(key, "true") }, want: true},
		{name: "yes", setup: func(t *testing.T) { t.Setenv(key, "yes") }, want: true},
		{name: "on", setup: func(t *testing.T) { t.Setenv(key, "on") }, want: true},
		{name: "enabled word", setup: func(t *testing.T) { t.Setenv(key, "enabled") }, want: true},
		{name: "arbitrary string", setup: func(t *testing.T) { t.Setenv(key, "hello") }, want: true},
		{name: "empty string", setup: func(t *testing.T) { t.Setenv(key, "") }, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			got := EnvEnabled(key)
			if got != tc.want {
				t.Fatalf("EnvEnabled(%q) = %v, want %v", key, got, tc.want)
			}
		})
	}
}

// fakeClock is a race-safe manual time source for TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
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

// setProvider installs an in-memory provider for the duration of the test and
// restores the no-op provider afterwards. Not parallel-safe: the OpenFeature
// provider is process-global, so tests calling this MUST NOT call t.Parallel.
func setProvider(t *testing.T, flags map[string]memprovider.InMemoryFlag) {
	t.Helper()
	if err := openfeature.SetProviderAndWait(memprovider.NewInMemoryProvider(flags)); err != nil {
		t.Fatalf("SetProviderAndWait: %v", err)
	}
	t.Cleanup(func() {
		if err := openfeature.SetProviderAndWait(openfeature.NoopProvider{}); err != nil {
			t.Fatalf("restoring NoopProvider: %v", err)
		}
	})
}

// clearProvider installs the no-op provider, modelling an application that never
// wires OpenFeature at all.
func clearProvider(t *testing.T) {
	t.Helper()
	if err := openfeature.SetProviderAndWait(openfeature.NoopProvider{}); err != nil {
		t.Fatalf("SetProviderAndWait(Noop): %v", err)
	}
}

func TestResolver_NoProviderFallsBackToEnv(t *testing.T) {
	const envVar = "OTEL_TEST_FLAGS_NO_PROVIDER"
	clearProvider(t)

	t.Setenv(envVar, "true")
	r := NewResolver("test", WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}))
	if !r.Enabled(0) {
		t.Fatal("Enabled(0) = false with truthy env and no provider, want true")
	}

	t.Setenv(envVar, "false")
	r2 := NewResolver("test", WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}))
	if r2.Enabled(0) {
		t.Fatal("Enabled(0) = true with falsy env and no provider, want false")
	}
}

func TestResolver_MissingFlagFallsBackToEnv(t *testing.T) {
	const envVar = "OTEL_TEST_FLAGS_MISSING_KEY"
	// Provider is installed but knows nothing about our key: Boolean returns the
	// default, which is the env value.
	setProvider(t, map[string]memprovider.InMemoryFlag{"some-other-flag": boolFlag(true)})

	t.Setenv(envVar, "true")
	r := NewResolver("test", WithSpecs(Spec{Key: "absent-flag", EnvVar: envVar}))
	if !r.Enabled(0) {
		t.Fatal("Enabled(0) = false for an absent flag with truthy env, want true")
	}
}

func TestResolver_ProviderOverridesEnvBothDirections(t *testing.T) {
	const envVar = "OTEL_TEST_FLAGS_OVERRIDE"

	t.Run("relay true beats env off", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"test-flag": boolFlag(true)})
		t.Setenv(envVar, "false")
		r := NewResolver("test", WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}))
		if !r.Enabled(0) {
			t.Fatal("Enabled(0) = false, want true (relay overrides falsy env)")
		}
	})

	t.Run("relay false beats env on", func(t *testing.T) {
		setProvider(t, map[string]memprovider.InMemoryFlag{"test-flag": boolFlag(false)})
		t.Setenv(envVar, "true")
		r := NewResolver("test", WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}))
		if r.Enabled(0) {
			t.Fatal("Enabled(0) = true, want false (relay overrides truthy env)")
		}
	})
}

func TestResolver_TTLBoundary(t *testing.T) {
	const envVar = "OTEL_TEST_FLAGS_TTL"
	setProvider(t, map[string]memprovider.InMemoryFlag{"test-flag": boolFlag(false)})
	t.Setenv(envVar, "false")

	clock := newFakeClock()
	r := NewResolver("test",
		WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}),
		WithClock(clock.Now),
	)

	if r.Enabled(0) {
		t.Fatal("initial Enabled(0) = true, want false")
	}

	// Flip the provider; the cached snapshot must survive until the TTL elapses.
	setProvider(t, map[string]memprovider.InMemoryFlag{"test-flag": boolFlag(true)})

	clock.Advance(900 * time.Millisecond)
	if r.Enabled(0) {
		t.Fatal("Enabled(0) = true at 900ms, want false (snapshot still fresh)")
	}

	clock.Advance(200 * time.Millisecond)
	if !r.Enabled(0) {
		t.Fatal("Enabled(0) = false at 1100ms, want true (snapshot expired)")
	}
}

func TestResolver_SnapshotIsConsistentAcrossSpecs(t *testing.T) {
	const (
		envA = "OTEL_TEST_FLAGS_CONSISTENT_A"
		envB = "OTEL_TEST_FLAGS_CONSISTENT_B"
	)
	setProvider(t, map[string]memprovider.InMemoryFlag{
		"flag-a": boolFlag(false),
		"flag-b": boolFlag(false),
	})
	t.Setenv(envA, "false")
	t.Setenv(envB, "false")

	clock := newFakeClock()
	r := NewResolver("test",
		WithSpecs(
			Spec{Key: "flag-a", EnvVar: envA},
			Spec{Key: "flag-b", EnvVar: envB},
		),
		WithClock(clock.Now),
	)

	if r.Enabled(0) || r.Enabled(1) {
		t.Fatalf("initial values = (%v, %v), want (false, false)", r.Enabled(0), r.Enabled(1))
	}

	// Both flags change together on the relay.
	setProvider(t, map[string]memprovider.InMemoryFlag{
		"flag-a": boolFlag(true),
		"flag-b": boolFlag(true),
	})
	clock.Advance(2 * time.Second)

	// Reading both must yield the same generation — never one old, one new.
	a, b := r.Enabled(0), r.Enabled(1)
	if a != b {
		t.Fatalf("torn read across specs: (%v, %v), want both equal", a, b)
	}
	if !a {
		t.Fatal("values = (false, false) after refresh, want (true, true)")
	}
}

func TestResolver_OutOfRangeIndexIsDisabled(t *testing.T) {
	const envVar = "OTEL_TEST_FLAGS_RANGE"
	setProvider(t, map[string]memprovider.InMemoryFlag{"test-flag": boolFlag(true)})
	t.Setenv(envVar, "true")

	r := NewResolver("test", WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}))
	if r.Enabled(1) {
		t.Fatal("Enabled(1) = true for a single-spec resolver, want false")
	}
	if r.Enabled(-1) {
		t.Fatal("Enabled(-1) = true, want false")
	}
}

func TestResolver_NilOptionIsSkipped(t *testing.T) {
	clearProvider(t)
	r := NewResolver("test", nil, WithSpecs(Spec{Key: "k", EnvVar: "OTEL_TEST_FLAGS_NIL_OPT"}), nil)
	if r.Enabled(0) {
		t.Fatal("Enabled(0) = true with unset env, want false")
	}
}

func TestResolver_ConcurrentEnabled(t *testing.T) {
	const envVar = "OTEL_TEST_FLAGS_CONCURRENT"
	setProvider(t, map[string]memprovider.InMemoryFlag{"test-flag": boolFlag(true)})
	t.Setenv(envVar, "false")

	clock := newFakeClock()
	r := NewResolver("test",
		WithSpecs(Spec{Key: "test-flag", EnvVar: envVar}),
		WithClock(clock.Now),
	)

	// Hammer Enabled while the clock keeps crossing the TTL boundary, so readers
	// and refreshers overlap. Correctness here is "no race, always true".
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !r.Enabled(0) {
					t.Error("Enabled(0) = false under concurrency, want true")
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			clock.Advance(10 * time.Millisecond)
		}
	}()
	wg.Wait()
}
