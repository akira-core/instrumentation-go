package harness

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
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

// envTruthy mirrors the modules' flags.EnvEnabled semantics: a present value is
// truthy unless it is a recognized falsy token; an absent value is false.
func envTruthy(key string) bool {
	if key == "" {
		return false
	}
	v, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Consistent-sampling constants (mirror otel-sampler/otelsampler).
const (
	maxAdjustedCount = uint64(1) << 56
	randomnessMask   = maxAdjustedCount - 1
)

// simpleThreshold returns threshold ≈ (1-p)·2^56. A span is sampled iff its
// randomness value rv >= threshold. The harness only asserts predicted
// decisions for rv values chosen with large margins, so the sampler's
// sub-threshold precision rounding never flips a predicted decision.
func simpleThreshold(p float64) uint64 {
	if p >= 1.0 {
		return 0
	}
	if p <= 0 {
		return maxAdjustedCount
	}
	return uint64(math.Round((1 - p) * float64(maxAdjustedCount)))
}

// ExpectedSampled reports whether sampling probability p samples randomness rv
// (i.e. rv ≥ threshold(p)). Use it to predict which services should appear for a
// chosen rv. Choose rv values with a large margin from the threshold so the
// sampler's sub-threshold rounding never flips a predicted decision.
func ExpectedSampled(p float64, rv uint64) bool {
	return rv >= simpleThreshold(p)
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
