package otelgorillaws

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a Conn.
type Option func(*connOptions)

type connOptions struct {
	propagator     propagation.TextMapPropagator
	tracerProvider trace.TracerProvider
}

// WithPropagators sets a TextMapPropagator for this connection only.
// If not provided, the global propagator is used.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *connOptions) {
		if p != nil {
			o.propagator = p
		}
	}
}

// WithTracerProvider sets a TracerProvider for this connection only.
// If not provided, the global provider is used.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *connOptions) {
		if tp != nil {
			o.tracerProvider = tp
		}
	}
}

// resolveOptions materialises the tracer + propagator the traced impl uses.
// Only called from the env-enabled branch of newConn — the env-disabled
// branch picks direct.NewConn which holds no tracer at all, so caller-
// supplied TracerProvider is never touched in the disabled-mode path.
func resolveOptions(opts []Option) (trace.Tracer, propagation.TextMapPropagator) {
	cfg := connOptions{}
	for _, opt := range opts {
		opt(&cfg)
	}

	prop := cfg.propagator
	if prop == nil {
		prop = otel.GetTextMapPropagator()
	}

	tp := cfg.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	tr := tp.Tracer(ScopeName,
		trace.WithInstrumentationVersion(Version()),
		trace.WithSchemaURL(semconv.SchemaURL),
	)
	return tr, prop
}
