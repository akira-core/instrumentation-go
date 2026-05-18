package otelmongo

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Client wraps *mongo.Client with OpenTelemetry instrumentation.
// Tracer and propagator are derived once at Connect time from WithTracerProvider/WithPropagators options,
// falling back to otel globals when not provided. The globals are never overwritten.
type Client struct {
	*mongo.Client
	serverAddr         string
	serverPort         int
	tracer             trace.Tracer                  // derived from option or otel.GetTracerProvider()
	propagator         propagation.TextMapPropagator // from option or otel.GetTextMapPropagator()
	tracingEnabled     bool                          // cached mongoTracingEnabled() result; gates wrapper CLIENT spans
	propagationEnabled bool
	deliverTracer      trace.Tracer             // MongoDB deliver span tracer (nil when disabled)
	mongoTP            *sdktrace.TracerProvider // independent TracerProvider for deliver spans (nil when disabled)
}

// ClientOption configures Connect/NewClient. Per OTel contrib: accept TracerProvider and Propagators.
type ClientOption interface {
	apply(*clientConfig)
}

type clientOptionFunc func(*clientConfig)

func (f clientOptionFunc) apply(c *clientConfig) { f(c) }

type clientConfig struct {
	TracerProvider     trace.TracerProvider
	Propagators        propagation.TextMapPropagator
	PropagationEnabled *bool
}

// WithTracerProvider sets the TracerProvider for the client. Defaults to otel.GetTracerProvider().
func WithTracerProvider(tp trace.TracerProvider) ClientOption {
	return clientOptionFunc(func(c *clientConfig) {
		if tp != nil {
			c.TracerProvider = tp
		}
	})
}

// WithPropagators sets the TextMapPropagator. Defaults to otel.GetTextMapPropagator().
func WithPropagators(p propagation.TextMapPropagator) ClientOption {
	return clientOptionFunc(func(c *clientConfig) {
		if p != nil {
			c.Propagators = p
		}
	})
}

// WithTracePropagationEnabled sets whether _oteltrace propagation is enabled for this client
// when both OTEL_INSTRUMENTATION_GO_TRACING_ENABLED and OTEL_MONGO_TRACING_ENABLED are truthy.
// It overrides OTEL_MONGO_PROPAGATION_ENABLED for the module default only; it cannot enable
// propagation while either tracing gate is unset or false (propagation is force-disabled
// whenever Mongo tracing is off, so callers get a single kill switch).
func WithTracePropagationEnabled(v bool) ClientOption {
	return clientOptionFunc(func(c *clientConfig) {
		c.PropagationEnabled = &v
	})
}

func newClientConfig(opts []ClientOption) *clientConfig {
	cfg := &clientConfig{}
	for _, o := range opts {
		o.apply(cfg)
	}
	return cfg
}

// Connect creates a new Client with the given configuration options, with OpenTelemetry instrumentation.
// Signature aligns with mongo.Connect(ctx, opts ...*options.ClientOptions). TracerProvider and Propagators default to global.
func Connect(ctx context.Context, opts ...*options.ClientOptions) (*Client, error) {
	return ConnectWithOptions(ctx, nil, opts...)
}

// ConnectWithOptions creates a Client. When WithTracerProvider/WithPropagators are passed, they are
// stored in the Client and used for all tracing — the otel globals are never overwritten.
// Without options, falls back to otel.GetTracerProvider()/otel.GetTextMapPropagator() at connect time.
func ConnectWithOptions(ctx context.Context, traceOpts []ClientOption, opts ...*options.ClientOptions) (*Client, error) {
	if !mongoTracingEnabled() {
		merged := options.MergeClientOptions(opts...)
		mc, err := mongo.Connect(ctx, merged)
		if err != nil {
			return nil, err
		}
		addr, port := parseServerFromClientOptions(merged)
		tracer := noop.NewTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
		return &Client{
			Client:             mc,
			serverAddr:         addr,
			serverPort:         port,
			tracer:             tracer,
			propagator:         otel.GetTextMapPropagator(),
			tracingEnabled:     false,
			propagationEnabled: false,
		}, nil
	}
	cfg := newClientConfig(traceOpts)
	tp := cfg.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	prop := cfg.Propagators
	if prop == nil {
		prop = otel.GetTextMapPropagator()
	}
	propEnabled := resolveDocumentPropagation(cfg.PropagationEnabled)
	tracer := tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
	merged := options.MergeClientOptions(opts...)
	mc, err := mongo.Connect(ctx, merged)
	if err != nil {
		return nil, err
	}
	addr, port := parseServerFromClientOptions(merged)
	mongoTP, deliverTracer := initMongoProvider(addr, port)
	return &Client{
		Client:             mc,
		serverAddr:         addr,
		serverPort:         port,
		tracer:             tracer,
		propagator:         prop,
		tracingEnabled:     true,
		propagationEnabled: propEnabled,
		mongoTP:            mongoTP,
		deliverTracer:      deliverTracer,
	}, nil
}

func parseServerFromClientOptions(opts *options.ClientOptions) (addr string, port int) {
	if opts == nil {
		return "", 0
	}
	return parseServerFromURI(opts.GetURI())
}

// NewClient connects to MongoDB using uri and returns an instrumented Client.
// For custom TracerProvider/Propagators pass traceOpts.
func NewClient(ctx context.Context, uri string, traceOpts ...ClientOption) (*Client, error) {
	return ConnectWithOptions(ctx, traceOpts, options.Client().ApplyURI(uri))
}

// Disconnect disconnects the MongoDB client and shuts down the deliver TracerProvider if active.
func (c *Client) Disconnect(ctx context.Context) error {
	err := c.Client.Disconnect(ctx)
	if c.mongoTP != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.mongoTP.Shutdown(shutCtx) // best-effort; deliver spans may be lost on failure
	}
	return err
}

// Ping runs a ping command against the server. Use readpref.Primary() or nil for default.
func (c *Client) Ping(ctx context.Context, rp *readpref.ReadPref) error {
	return c.Client.Ping(ctx, rp)
}

// StartSession starts a new session. Operations executed with the session
// should use this client's Database/Collection so document-level tracing applies.
func (c *Client) StartSession(opts ...*options.SessionOptions) (mongo.Session, error) {
	return c.Client.StartSession(opts...)
}

// parseServerFromURI extracts server address and port from a MongoDB URI for semconv server.* attributes.
// Uses the first host when the URI has multiple hosts (e.g. replica set). Handles IPv6 (e.g. [::1]).
// Returns ("", 0) on parse failure or when the URI has no host (e.g. some SRV forms).
func parseServerFromURI(uri string) (addr string, port int) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return "", 0
	}
	firstHost := u.Host
	if i := strings.Index(u.Host, ","); i >= 0 {
		firstHost = strings.TrimSpace(u.Host[:i])
	}
	u2, err := url.Parse("//" + firstHost)
	if err != nil {
		return "", 0
	}
	addr = u2.Hostname()
	if addr == "" {
		return "", 0
	}
	portStr := u2.Port()
	if portStr == "" {
		port = 27017
		return addr, port
	}
	p, _ := strconv.Atoi(portStr)
	if p <= 0 {
		p = 27017
	}
	return addr, p
}

// initMongoProvider creates an independent TracerProvider with service.name = "mongodb://{addr}"
// for synthetic deliver spans. Only enabled when OTEL_EXPORTER_OTLP_ENDPOINT is set.
// The endpoint must be a full URL (e.g. "http://otel-collector:4318") for HTTP,
// or a host:port (e.g. "otel-collector:4317") for gRPC.
func initMongoProvider(addr string, port int) (*sdktrace.TracerProvider, trace.Tracer) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	ctx := context.Background()

	var exp sdktrace.SpanExporter
	var err error
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		exp, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(endpoint),
		)
	} else {
		exp, err = otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoint),
		)
	}
	if err != nil {
		slog.Warn("otelmongo: deliver tracer disabled", "reason", "exporter creation failed", "error", err)
		return nil, nil
	}

	serviceName := mongoServiceName(addr, port)
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		slog.Warn("otelmongo: deliver tracer disabled", "reason", "resource creation failed", "error", err)
		_ = exp.Shutdown(ctx) // avoid leaking the exporter connection
		return nil, nil
	}
	slog.Debug("otelmongo: deliver tracer enabled", "service", serviceName, "endpoint", endpoint) //nolint:gosec // intentional diagnostic log of internal config values

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	tracer := tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
	return tp, tracer
}

// mongoServiceName returns the service.name for the MongoDB deliver span TracerProvider.
func mongoServiceName(addr string, port int) string {
	if addr == "" {
		return "mongodb"
	}
	if port > 0 && port != 27017 {
		return fmt.Sprintf("mongodb://%s:%d", addr, port)
	}
	return "mongodb://" + addr
}

// Database returns a wrapped Database for document-level tracing.
func (c *Client) Database(name string, opts ...*options.DatabaseOptions) *Database {
	return &Database{
		Database:           c.Client.Database(name, opts...),
		serverAddr:         c.serverAddr,
		serverPort:         c.serverPort,
		tracer:             c.tracer,
		propagator:         c.propagator,
		tracingEnabled:     c.tracingEnabled,
		propagationEnabled: c.propagationEnabled,
		deliverTracer:      c.deliverTracer,
	}
}
