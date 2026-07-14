package otelmongo

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/akira-core/instrumentation-go/otel-mongo/v2/internal/shared"
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
// Signature aligns with mongo.Connect(opts ...*options.ClientOptions). TracerProvider and Propagators default to global.
// Set them at process startup (see ../examples/, which demos this v2 package) or pass WithTracerProvider/WithPropagators via ConnectWithOptions.
func Connect(opts ...*options.ClientOptions) (*Client, error) {
	return ConnectWithOptions(nil, opts...)
}

// ConnectWithOptions creates a Client. When WithTracerProvider/WithPropagators are passed, they are
// stored in the Client and used for all tracing — the otel globals are never overwritten.
// Without options, falls back to otel.GetTracerProvider()/otel.GetTextMapPropagator() at connect time.
func ConnectWithOptions(traceOpts []ClientOption, opts ...*options.ClientOptions) (*Client, error) {
	if !mongoTracingEnabled() {
		merged := options.MergeClientOptions(opts...)
		mc, err := mongo.Connect(merged)
		if err != nil {
			return nil, err
		}
		addr, port := parseServerFromURI(merged.GetURI())
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
	merged.SetMonitor(shared.NewCommandMonitor(merged.Monitor))
	mc, err := mongo.Connect(merged)
	if err != nil {
		return nil, err
	}
	addr, port := parseServerFromURI(merged.GetURI())
	return &Client{
		Client:             mc,
		serverAddr:         addr,
		serverPort:         port,
		tracer:             tracer,
		propagator:         prop,
		tracingEnabled:     true,
		propagationEnabled: propEnabled,
	}, nil
}

// NewClient connects to MongoDB using uri and returns an instrumented Client.
// For custom TracerProvider/Propagators pass ClientOptions.
func NewClient(uri string, traceOpts ...ClientOption) (*Client, error) {
	return ConnectWithOptions(traceOpts, options.Client().ApplyURI(uri))
}

// Disconnect disconnects the MongoDB client.
func (c *Client) Disconnect(ctx context.Context) error {
	return c.Client.Disconnect(ctx)
}

// Ping runs a ping command against the server. Use readpref.Primary() or nil for default.
func (c *Client) Ping(ctx context.Context, rp *readpref.ReadPref) error {
	return c.Client.Ping(ctx, rp)
}

// StartSession starts a new session. Operations executed with the session
// should use this client's Database/Collection so document-level tracing applies.
func (c *Client) StartSession(opts ...options.Lister[options.SessionOptions]) (*mongo.Session, error) {
	return c.Client.StartSession(opts...)
}

// stripURIWhitespace removes raw ASCII whitespace (space, tab, CR, LF) from a
// URI. MongoDB URIs never legitimately contain raw whitespace — hosts are
// comma-separated with none between them, and any whitespace elsewhere must be
// percent-encoded — but a URI assembled across config-file lines can pick up
// stray spaces or newlines around the multi-host list. url.Parse would reject
// such a string wholesale ("invalid control character" / "invalid character in
// host name"), collapsing server.* to ("", 0); stripping first lets the first
// host still be recovered. Safe here because parseServerFromURI consumes only
// the host/port, never userinfo, path, or query.
func stripURIWhitespace(uri string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, uri)
}

// parseServerFromURI extracts server address and port from a MongoDB URI for semconv server.* attributes.
// Uses the first host when the URI has multiple hosts (e.g. replica set). Handles IPv6 (e.g. [::1]).
// Stray whitespace around the multi-host list is tolerated (see stripURIWhitespace).
// Returns ("", 0) when the URI has no host (e.g. some SRV forms or a scheme-only URI).
//
// The authority is sliced out by hand rather than via url.Parse: url.Parse
// validates the whole string as one host:port, so it rejects any legitimate
// multi-host replica-set URI whose last host omits a port (mongodb://a:27017,b)
// or that lists three or more hosts, and it cannot parse a comma-joined IPv6
// host list. Peeling scheme → path/query → userinfo → first host keeps only the
// part we need and sidesteps all of that.
func parseServerFromURI(uri string) (addr string, port int) {
	uri = stripURIWhitespace(uri)
	if i := strings.Index(uri, "://"); i >= 0 {
		uri = uri[i+3:]
	}
	if i := strings.IndexAny(uri, "/?#"); i >= 0 { // drop /path, ?query, #fragment
		uri = uri[:i]
	}
	if i := strings.LastIndexByte(uri, '@'); i >= 0 { // drop user:pass@ userinfo
		uri = uri[i+1:]
	}
	if i := strings.IndexByte(uri, ','); i >= 0 { // first host of a replica-set list
		uri = uri[:i]
	}
	return shared.SplitHostPort(uri)
}

// Database returns a wrapped Database for document-level tracing.
func (c *Client) Database(name string, opts ...options.Lister[options.DatabaseOptions]) *Database {
	return &Database{
		Database:           c.Client.Database(name, opts...),
		serverAddr:         c.serverAddr,
		serverPort:         c.serverPort,
		tracer:             c.tracer,
		propagator:         c.propagator,
		tracingEnabled:     c.tracingEnabled,
		propagationEnabled: c.propagationEnabled,
	}
}
