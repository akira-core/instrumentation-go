package otelgorillaws

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
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

func applyOptions(c *Conn, opts []Option) {
	cfg := connOptions{}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.propagator != nil {
		c.propagator = cfg.propagator
	} else {
		c.propagator = otel.GetTextMapPropagator()
	}

	tp := cfg.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	c.tracer = tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()), trace.WithSchemaURL(semconv.SchemaURL))
}
