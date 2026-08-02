package otelmongo

// gateState holds the per-client tracing/propagation overrides and whether the
// instrumented path was built. Client and Database share this so effective
// gates are not hand-copied (design R16).
type gateState struct {
	tracingOverride     *bool
	propagationOverride *bool
	tracedBuilt         bool
}

func (g gateState) effectiveTracing() bool {
	if !g.tracedBuilt {
		return false
	}
	if g.tracingOverride != nil {
		return *g.tracingOverride
	}
	return mongoTracingEnabled()
}

// effectivePropagation resolves _oteltrace for a call that may re-enter tracing.
// Prefer propagationGiven with a tracing value already resolved for the same
// operation (design R5).
func (g gateState) effectivePropagation() bool {
	return g.propagationGiven(g.effectiveTracing())
}

// propagationGiven applies the propagation policy for an already-resolved
// tracing decision — it must not call effectiveTracing again.
func (g gateState) propagationGiven(tracing bool) bool {
	if g.tracingOverride != nil {
		if !tracing {
			return false
		}
		return resolveFlag(g.propagationOverride, mongoPropagationEnvOnly())
	}
	return resolveDocumentPropagation(tracing, g.propagationOverride)
}
