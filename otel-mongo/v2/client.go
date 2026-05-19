package otelmongo

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/v2/internal/traced"
)

// Client wraps *mongo.Client with OpenTelemetry instrumentation.
//
// Disabled-mode invariant via nullable pointer: `traced` is nil whenever
// mongoTracingEnabled() returned false at Connect time. See
// `otel-mongo-flag-wiring` spec Requirement "Client and Database isolate
// SDK state behind a nullable traced pointer" for rationale.
type Client struct {
	*mongo.Client
	traced *traced.ClientState // nil ⇔ disabled
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
func Connect(opts ...*options.ClientOptions) (*Client, error) {
	return ConnectWithOptions(nil, opts...)
}

// ConnectWithOptions creates a Client. The nullable *traced.ClientState pointer
// is populated only when mongoTracingEnabled() returns true; otherwise `traced`
// is nil and the disabled call path is structurally unreachable from SDK code.
func ConnectWithOptions(traceOpts []ClientOption, opts ...*options.ClientOptions) (*Client, error) {
	merged := options.MergeClientOptions(opts...)
	mc, err := mongo.Connect(merged)
	if err != nil {
		return nil, err
	}
	if !mongoTracingEnabled() {
		return &Client{Client: mc}, nil
	}
	addr, port := parseServerFromURI(merged.GetURI())
	cfg := newClientConfig(traceOpts)
	tp := cfg.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	prop := cfg.Propagators
	if prop == nil {
		prop = otel.GetTextMapPropagator()
	}
	tracer := tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
	mongoTP, deliverTracer := initMongoProvider(addr, port)
	return &Client{
		Client: mc,
		traced: &traced.ClientState{
			Tracer:             tracer,
			Propagator:         prop,
			PropagationEnabled: resolveDocumentPropagation(cfg.PropagationEnabled),
			DeliverTracer:      deliverTracer,
			MongoTP:            mongoTP,
			ServerAddr:         addr,
			ServerPort:         port,
		},
	}, nil
}

// NewClient connects to MongoDB using uri and returns an instrumented Client.
func NewClient(uri string, traceOpts ...ClientOption) (*Client, error) {
	return ConnectWithOptions(traceOpts, options.Client().ApplyURI(uri))
}

// Disconnect disconnects the MongoDB client and (when tracing is enabled)
// shuts down the deliver TracerProvider.
func (c *Client) Disconnect(ctx context.Context) error {
	err := c.Client.Disconnect(ctx)
	if c.traced != nil {
		c.traced.ShutdownDeliver(ctx)
	}
	return err
}

// Ping runs a ping command against the server. Use readpref.Primary() or nil for default.
func (c *Client) Ping(ctx context.Context, rp *readpref.ReadPref) error {
	return c.Client.Ping(ctx, rp)
}

// StartSession starts a new session.
func (c *Client) StartSession(opts ...options.Lister[options.SessionOptions]) (*mongo.Session, error) {
	return c.Client.StartSession(opts...)
}

// parseServerFromURI extracts server address and port from a MongoDB URI for semconv server.* attributes.
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
	slog.Debug("otelmongo: deliver tracer enabled", "service", serviceName, "endpoint", endpoint)

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

// Database returns a wrapped Database for document-level tracing. The
// `traced` pointer propagates: nil parent ⇒ nil child, non-nil parent ⇒
// child gets a DatabaseState inheriting the client's gates.
func (c *Client) Database(name string, opts ...options.Lister[options.DatabaseOptions]) *Database {
	raw := c.Client.Database(name, opts...)
	if c.traced == nil {
		return &Database{Database: raw}
	}
	return &Database{Database: raw, traced: c.traced.ForDatabase()}
}
