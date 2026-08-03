package otelmongo

// gateState holds one client's static, environment-derived tiers. Client and
// Database share it so the effective gates are not hand-copied (design R16).
//
// Both fields are fixed at construction and never re-read: no environment
// variable is touched on any hot path. What remains per operation is the relay
// verdict and nothing else.
type gateState struct {
	// tracedBuilt is gate1 AND OTEL_MONGO_TRACING_ENABLED. False ⇒ only the
	// passthrough implementation is constructed, no OTel SDK code path is
	// reachable, and the resolver is never consulted.
	tracedBuilt bool

	// propagation is OTEL_MONGO_PROPAGATION_ENABLED, or the
	// WithTracePropagationEnabled option when one was supplied. It gates
	// _oteltrace writes into the caller's documents, one tier below tracing.
	propagation bool
}

// effectiveTracing reports whether THIS operation traces: the static ceiling AND
// the relay verdict, resolved fresh so a revocation reaches a live client on its
// next operation.
func (g gateState) effectiveTracing() bool {
	return g.tracedBuilt && mongoRelayAllowsTracing()
}

// effectivePropagation resolves _oteltrace for a call that has not already
// resolved tracing. Prefer propagationGiven with a tracing value resolved for
// the same operation (design R5): re-resolving here would let one operation see
// tracing on for its span decision and off for its propagation decision.
func (g gateState) effectivePropagation() bool {
	return g.propagationGiven(g.effectiveTracing())
}

// propagationGiven applies the propagation policy to an ALREADY-resolved tracing
// decision. It must not call effectiveTracing again.
//
// Tracing short-circuits: with it off there is no _oteltrace inject or extract,
// however that off came about. WithTracePropagationEnabled(true) only chooses
// the propagation tier — it can never bypass a disabled first tier, a disabled
// module switch, or a relay revocation.
func (g gateState) propagationGiven(tracing bool) bool {
	return tracing && g.propagation && mongoRelayAllowsPropagation()
}

// propagationWhenTracing is propagationGiven(true), for the instrumented
// implementations.
//
// It is what closes design R5. Those implementations are reachable only through
// a facade impl() that has ALREADY resolved tracing to true for this operation,
// so re-resolving it here would let one operation emit a CLIENT span on one
// verdict and decide _oteltrace on another. Passing the resolved value is the
// rule; assuming it is how a zero-argument func() bool obeys that rule.
func (g gateState) propagationWhenTracing() bool {
	return g.propagationGiven(true)
}
