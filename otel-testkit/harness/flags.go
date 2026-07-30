package harness

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/akira-core/instrumentation-go/otel-sampler/otelsampler"
)

// GateEnv names the feature-flag environment variables a plugin's instrumentation
// reads. Propagation may be empty for transports without an independent
// propagation gate (e.g. NATS), in which case propagation tracks tracing.
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

// ExpectationFromEnv resolves the expected behavior from the plugin's gate env.
func ExpectationFromEnv(g GateEnv) Expectation {
	tracing := envTruthy(g.Global) && envTruthy(g.Tracing)
	prop := tracing
	if g.Propagation != "" {
		prop = tracing && envTruthy(g.Propagation)
	}
	return Expectation{TracingEnabled: tracing, PropagationEnabled: prop}
}

// envTruthy mirrors the modules' internal flags.EnvEnabled semantics exactly: an
// absent value is false; a present value is truthy unless it is one of the
// recognized falsy tokens ("0", "false", "no", "off", case-insensitive, after
// trimming). A set-but-empty value is therefore TRUE — the same as the modules
// treat it. Any divergence here makes the matrix assert the wrong branch, so
// TestEnvTruthyMatchesModuleFlags pins the whole table.
//
// NOTE: this is effectively a fifth copy of internal/flags.EnvEnabled (the four
// modules vendor byte-identical copies and cannot export theirs). Keep it in
// sync when that semantics changes.
func envTruthy(key string) bool {
	if key == "" {
		return false
	}
	v, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
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
