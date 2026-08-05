package otelmongo

import (
	otelflags "github.com/akira-core/instrumentation-go/otel-flags"
)

// gateState holds one client's switch state as fixed at construction. Client and
// Database share it so the effective gates are not hand-copied (design R16).
//
// Every field is fixed at construction and never re-read: no environment
// variable is touched on any hot path. What remains per operation is two or
// three relay evaluations and nothing else.
type gateState struct {
	// relayPossible was resolved once, at construction — an endpoint is
	// configured, or a provider is already bound. False means the relay is
	// structurally incapable of returning anything but the value passed to it,
	// so this client resolves statically and never touches the OpenFeature SDK.
	relayPossible bool

	// masterLocal, tracingLocal and propLocal are the env > option > default
	// answers. They are the evaluation defaults handed to the relay, not a
	// separate tier.
	masterLocal  bool
	tracingLocal bool
	propLocal    bool
}

// tracedPossible reports whether the instrumented implementations must be built
// at all, and whether shared.NewCommandMonitor is registered.
//
// It is `relayPossible || (masterLocal && tracingLocal)` rather than the static
// answer alone, because the relay can now ENABLE: a client whose environment
// says off must still be able to start tracing when the relay says so, and
// construction happens once. When no relay can exist the static answer is final,
// a switched-off client allocates nothing instrumented, and the command monitor
// — which runs on EVERY MongoDB command — is not registered either.
func (g gateState) tracedPossible() bool {
	return g.relayPossible || (g.masterLocal && g.tracingLocal)
}

// effectiveTracing reports whether THIS operation traces: the master switch AND
// this module's tracing switch, each resolved down the full ladder so a relay
// change reaches a live client on its next operation.
//
// With no relay possible it is arithmetic on two booleans fixed at construction —
// no evaluation, no allocation.
func (g gateState) effectiveTracing() bool {
	if !g.relayPossible {
		return g.masterLocal && g.tracingLocal
	}
	return otelflags.MasterEnabled(g.masterLocal) &&
		mongoResolver.Value(flagKeyMongoTracing, g.tracingLocal)
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
// however that off came about. WithTracePropagationEnabled(true) only supplies
// one rung of the propagation ladder — it can never bypass a master veto, a
// disabled tracing switch, or a relay that turned either off. Nor can it bypass
// OTEL_MONGO_PROPAGATION_ENABLED=false, which sits above it: that is what lets
// an operator stop writes into their own documents without touching the code.
func (g gateState) propagationGiven(tracing bool) bool {
	if !tracing {
		return false
	}
	if !g.relayPossible {
		return g.propLocal
	}
	return mongoResolver.Value(flagKeyMongoPropagation, g.propLocal)
}

// propagationWhenTracing is propagationGiven(true), for the instrumented
// implementations.
//
// It is what closes design R5. Those implementations are reachable only through
// a facade impl() that has ALREADY resolved tracing to true for this operation,
// so re-resolving it here would let one operation emit a CLIENT span on one
// value and decide _oteltrace on another. Passing the resolved value is the
// rule; assuming it is how a zero-argument func() bool obeys that rule.
func (g gateState) propagationWhenTracing() bool {
	return g.propagationGiven(true)
}
