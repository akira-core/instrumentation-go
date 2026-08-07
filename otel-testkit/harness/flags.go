package harness

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
	"github.com/akira-core/instrumentation-go/otel-sampler/otelsampler"
)

// GateEnv names the feature-flag environment variables a plugin's
// instrumentation reads. Propagation may be empty for transports without an
// independent propagation gate (e.g. NATS), in which case propagation tracks
// tracing.
//
// Global may be empty, in which case otel-flags' own master-switch variable is
// used. There is rarely a reason to set it: the master switch is process-scoped
// and there is exactly one of it.
type GateEnv struct {
	Global      string
	Tracing     string
	Propagation string
}

// Expectation is the behavior the harness should expect given the current env.
type Expectation struct {
	TracingEnabled     bool
	PropagationEnabled bool
}

// ExpectationFromEnv resolves the expected behavior from the plugin's gate env,
// assuming no relay is configured — which is the harness's situation, since it
// drives real containers through environment variables alone.
//
// It mirrors the modules' composition exactly: the master switch (default
// ENABLED, because it is a veto rather than an enabler) ANDed with the module's
// own switch (default DISABLED), and propagation ANDed one level below tracing.
//
// It delegates the per-variable read to otelflags.Lookup rather than restating
// the truthiness rules, so this file can no longer drift from what the modules
// do. An unreadable value is treated as disabled here: the harness has no error
// channel, and a module carrying one would fail at construction anyway, which is
// the failure the matrix would then observe.
func ExpectationFromEnv(g GateEnv) Expectation {
	master := envValue(g.Global, otelflags.EnvGlobalTracing, true)
	tracing := master && envValue(g.Tracing, "", false)
	prop := tracing
	if g.Propagation != "" {
		prop = tracing && envValue(g.Propagation, "", false)
	}
	return Expectation{TracingEnabled: tracing, PropagationEnabled: prop}
}

// envValue reads key (or fallback when key is empty) down the env > default
// rungs of the ladder. There is no option rung here: the harness configures
// containers, not Go constructors.
func envValue(key, fallbackKey string, def bool) bool {
	if key == "" {
		key = fallbackKey
	}
	if key == "" {
		return def
	}
	v, set, err := otelflags.Lookup(key)
	if err != nil || !set {
		return def && err == nil
	}
	return v
}

// randomnessMask masks a value down to the 56 randomness bits.
const randomnessMask = (uint64(1) << 56) - 1

// ExpectedSampled reports whether sampling probability p samples randomness rv.
// It delegates to the sampler's own threshold arithmetic (otelsampler.Sampled),
// so the prediction matches the real decision for every rv — including values
// sitting exactly on a threshold boundary, where a re-derived "(1-p)·2^56"
// approximation diverges from the sampler's precision rounding.
func ExpectedSampled(p float64, rv uint64) bool {
	return otelsampler.Sampled(p, rv)
}

// formatRV renders rv the way the sampler writes it into tracestate ("%014x").
func formatRV(rv uint64) string {
	return fmt.Sprintf("%014x", rv&randomnessMask)
}

// EnvSamplerArg reads OTEL_TRACES_SAMPLER_ARG as a probability, falling back to def.
func EnvSamplerArg(def float64) float64 {
	p, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")), 64)
	if err != nil {
		return def
	}
	return p
}
