package flags

import (
	"sync"
	"sync/atomic"
	"testing"
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

func TestGate_EvaluatesResolverOnce(t *testing.T) {
	var calls atomic.Int32
	g := NewGate(func() bool {
		calls.Add(1)
		return true
	})

	for i := 0; i < 100; i++ {
		if !g.Enabled() {
			t.Fatalf("Enabled() = false, want true at iteration %d", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver invoked %d times, want 1", got)
	}
}

func TestGate_ReturnsCachedValue(t *testing.T) {
	var resolved atomic.Bool
	g := NewGate(func() bool {
		return resolved.Load()
	})

	if g.Enabled() {
		t.Fatalf("first Enabled() = true, want false (resolver returned false)")
	}

	resolved.Store(true)

	if g.Enabled() {
		t.Fatalf("second Enabled() = true, want false (cached)")
	}
}

func TestGate_ResetForTest(t *testing.T) {
	var resolved atomic.Bool
	g := NewGate(func() bool {
		return resolved.Load()
	})

	if g.Enabled() {
		t.Fatal("initial Enabled() = true, want false")
	}

	resolved.Store(true)
	g.ResetForTest()

	if !g.Enabled() {
		t.Fatal("after ResetForTest, Enabled() = false, want true")
	}
}

func TestGate_ConcurrentEnabled(t *testing.T) {
	var calls atomic.Int32
	g := NewGate(func() bool {
		calls.Add(1)
		return true
	})

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = g.Enabled()
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver invoked %d times under concurrency, want 1", got)
	}
}
