package otelmongo

import (
	"os"
	"testing"
)

func TestMongoTracingEnabled_DefaultFalse(t *testing.T) {
	prev, existed := os.LookupEnv(envMongoTracingEnabled)
	_ = os.Unsetenv(envMongoTracingEnabled)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envMongoTracingEnabled, prev)
		} else {
			_ = os.Unsetenv(envMongoTracingEnabled)
		}
	})
	if mongoTracingEnabled() {
		t.Fatal("expected tracing disabled when env var is unset")
	}
}

func TestMongoTracingEnabled_EmptyStringIsEnabled(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "")
	t.Setenv(envMongoTracingEnabled, "")
	if !mongoTracingEnabled() {
		t.Fatal("expected empty string to mean enabled")
	}
}

func TestMongoTracingEnabled_FalseTokens(t *testing.T) {
	for _, v := range []string{"false", "0", "off", "no"} {
		t.Setenv(envMongoTracingEnabled, v)
		if mongoTracingEnabled() {
			t.Fatalf("expected disabled for value %q", v)
		}
	}
}

func TestMongoTracingEnabled_GlobalOffOverridesModule(t *testing.T) {
	t.Setenv(envGlobalTracingEnabled, "false")
	t.Setenv(envMongoTracingEnabled, "true")
	if mongoTracingEnabled() {
		t.Fatal("expected global flag to disable mongo tracing")
	}
}

func TestMongoPropagationEnabled(t *testing.T) {
	t.Run("unset global and propagation -> false", func(t *testing.T) {
		_ = os.Unsetenv(envGlobalTracingEnabled)
		_ = os.Unsetenv(envMongoPropagationEnabled)
		if mongoPropagationEnabled() {
			t.Fatal("expected propagation disabled when env vars are unset")
		}
	})

	t.Run("global on and propagation unset -> false", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		_ = os.Unsetenv(envMongoPropagationEnabled)
		if mongoPropagationEnabled() {
			t.Fatal("expected propagation disabled when module flag is unset")
		}
	})

	t.Run("global on and propagation on -> true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		if !mongoPropagationEnabled() {
			t.Fatal("expected propagation enabled")
		}
	})
}

func TestResolveFlag(t *testing.T) {
	t.Run("override true wins env false", func(t *testing.T) {
		override := true
		if !resolveFlag(&override, false) {
			t.Fatal("expected override true to win")
		}
	})

	t.Run("override false wins env true", func(t *testing.T) {
		override := false
		if resolveFlag(&override, true) {
			t.Fatal("expected override false to win")
		}
	})
}

func TestConnectPropagationResolution(t *testing.T) {
	t.Run("global off cannot enable propagation via WithTracePropagationEnabled", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "false")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("expected propagation disabled when global tracing is off")
		}
	})

	t.Run("global on option false wins over env propagation true", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		t.Setenv(envMongoPropagationEnabled, "true")
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(false)})
		if resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("expected WithTracePropagationEnabled(false) to disable propagation")
		}
	})

	t.Run("global on option true enables when env propagation unset", func(t *testing.T) {
		t.Setenv(envGlobalTracingEnabled, "true")
		_ = os.Unsetenv(envMongoPropagationEnabled)
		cfg := newClientConfig([]ClientOption{WithTracePropagationEnabled(true)})
		if !resolveDocumentPropagation(cfg.PropagationEnabled) {
			t.Fatal("expected WithTracePropagationEnabled(true) to enable propagation when global is on")
		}
	})
}
