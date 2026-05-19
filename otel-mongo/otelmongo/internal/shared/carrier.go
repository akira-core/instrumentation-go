package shared

// fixedCarrier is a stack-allocatable TextMapCarrier holding only the two
// W3C TraceContext fields ("traceparent" and "tracestate"). It avoids the
// map allocation that propagation.MapCarrier requires on the propagation
// hot path. Other keys are silently dropped — acceptable for W3C
// TraceContext propagators which only emit these two keys.
//
// Methods take pointer receivers so a value declared on the caller's stack
// can implement propagation.TextMapCarrier without escaping to the heap.
type fixedCarrier struct {
	traceparent string
	tracestate  string
}

func (f *fixedCarrier) Get(key string) string {
	switch key {
	case "traceparent":
		return f.traceparent
	case "tracestate":
		return f.tracestate
	}
	return ""
}

func (f *fixedCarrier) Set(key, value string) {
	switch key {
	case "traceparent":
		f.traceparent = value
	case "tracestate":
		f.tracestate = value
	}
}

// Keys returns the W3C TraceContext keys. The W3C TraceContext propagator's
// Extract path does not call Keys; this method exists only to satisfy the
// propagation.TextMapCarrier interface for custom propagators that iterate.
func (f *fixedCarrier) Keys() []string {
	if f.tracestate == "" {
		return []string{"traceparent"}
	}
	return []string{"traceparent", "tracestate"}
}
