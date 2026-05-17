package otelmongo

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo/internal/traced"
)

// Client wraps *mongo.Client with OpenTelemetry instrumentation.
//
// Disabled-mode invariant via nullable pointer: `traced` is nil whenever
// mongoTracingEnabled() returned false at Connect time. The
// *traced.ClientState owns the OTel SDK state (tracer, propagator, deliver
// TracerProvider, etc.) — a nil pointer has no Shutdown to accidentally
// call, so the disabled call path is structurally unreachable from SDK
// code. See `otel-mongo-flag-wiring` spec Requirement "Client and Database
// isolate SDK state behind a nullable traced pointer" for rationale.
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

// ConnectWithOptions creates a Client. The nullable *traced.ClientState pointer is
// populated only when mongoTracingEnabled() returns true; otherwise the returned
// *Client has `traced == nil` and the disabled call path is structurally unreachable
// from any SDK code. The otel globals are never overwritten — TracerProvider /
// Propagator come from options or are looked up here and stored on the state.
func ConnectWithOptions(ctx context.Context, traceOpts []ClientOption, opts ...*options.ClientOptions) (*Client, error) {
	merged := options.MergeClientOptions(opts...)
	mc, err := mongo.Connect(ctx, merged)
	if err != nil {
		return nil, err
	}
	if !mongoTracingEnabled() {
		return &Client{Client: mc}, nil
	}
	addr, port := parseServerFromClientOptions(merged)
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
	mongoTP, deliverTracer := initMongoProvider(ctx, addr, port)
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

// Disconnect disconnects the MongoDB client and (when tracing is enabled)
// shuts down the deliver TracerProvider. The nil-guard ensures the disabled
// path never reaches any SDK Shutdown call.
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
func initMongoProvider(ctx context.Context, addr string, port int) (*sdktrace.TracerProvider, trace.Tracer) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}

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
// child gets a DatabaseState inheriting the client's gates. The branch is
// constructor-site (per `instrumentation-feature-flags` exemption) and
// frozen for the lifetime of the returned Database.
func (c *Client) Database(name string, opts ...*options.DatabaseOptions) *Database {
	raw := c.Client.Database(name, opts...)
	if c.traced == nil {
		return &Database{Database: raw}
	}
	return &Database{Database: raw, traced: c.traced.ForDatabase()}
}
