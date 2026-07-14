package otelnats

import (
	nats "github.com/nats-io/nats.go"
)

// Connect establishes a NATS connection with tracing. Signature aligns with nats.Connect.
// Tracer and propagator are read from otel globals. Call otel.SetTracerProvider and
// otel.SetTextMapPropagator at process startup, or pass WithTracerProvider/WithPropagators
// to ConnectWithOptions for per-connection overrides (these do NOT update globals).
func Connect(url string, natsOpts ...nats.Option) (*Conn, error) {
	return ConnectWithOptions(url, filterNilOptions(natsOpts))
}

// ConnectWithOptions establishes a NATS connection with tracing.
// WithTracerProvider/WithPropagators override the global for this Conn only (globals are not modified).
func ConnectWithOptions(url string, natsOpts []nats.Option, traceOpts ...Option) (*Conn, error) {
	nc, err := nats.Connect(url, natsOpts...)
	if err != nil {
		return nil, err
	}
	return newConn(nc, traceOpts...), nil
}

func filterNilOptions(natsOpts []nats.Option) []nats.Option {
	var out []nats.Option
	for _, o := range natsOpts {
		if o != nil {
			out = append(out, o)
		}
	}
	return out
}

// ConnectTLS establishes a TLS-secured NATS connection with tracing.
// certFile and keyFile are paths to PEM-encoded client certificate and private key.
// caFile is the path to a PEM-encoded CA certificate for server verification (empty string skips CA override).
func ConnectTLS(url, certFile, keyFile, caFile string, natsOpts ...nats.Option) (*Conn, error) {
	return ConnectTLSWithOptions(url, certFile, keyFile, caFile, filterNilOptions(natsOpts))
}

// ConnectTLSWithOptions is ConnectTLS with additional trace options (WithTracerProvider, WithPropagators).
// natsOpts are passed to the underlying nats.Connect; traceOpts configure the OTel instrumentation.
func ConnectTLSWithOptions(url, certFile, keyFile, caFile string, natsOpts []nats.Option, traceOpts ...Option) (*Conn, error) {
	opts := make([]nats.Option, 0, len(natsOpts)+2)
	opts = append(opts, natsOpts...)
	opts = append(opts, nats.ClientCert(certFile, keyFile))
	if caFile != "" {
		opts = append(opts, nats.RootCAs(caFile))
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, err
	}
	return newConn(nc, traceOpts...), nil
}

// ConnectWithCredentials connects to NATS using a credentials file (JWT + NKey), with tracing.
// credFile is the path to a NATS credentials file (.creds).
func ConnectWithCredentials(url, credFile string, natsOpts ...nats.Option) (*Conn, error) {
	return ConnectWithCredentialsWithOptions(url, credFile, filterNilOptions(natsOpts))
}

// ConnectWithCredentialsWithOptions is ConnectWithCredentials with additional trace options
// (WithTracerProvider, WithPropagators). natsOpts are passed to the underlying nats.Connect;
// traceOpts configure the OTel instrumentation.
func ConnectWithCredentialsWithOptions(url, credFile string, natsOpts []nats.Option, traceOpts ...Option) (*Conn, error) {
	opts := make([]nats.Option, 0, len(natsOpts)+1)
	opts = append(opts, natsOpts...)
	opts = append(opts, nats.UserCredentials(credFile))
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, err
	}
	return newConn(nc, traceOpts...), nil
}
