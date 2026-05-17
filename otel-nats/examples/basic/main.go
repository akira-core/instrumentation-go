// Example demonstrates how to initialize the OpenTelemetry TracerProvider and
// TextMapPropagator at process startup, then use otelnats and oteljetstream.
// The instrumentation packages do not provide InitTracer; the application is
// responsible for creating and setting the global provider and propagator
// (per OTel Go Contrib instrumentation guidelines).
//
// Sections covered:
//  1. TracerProvider setup
//  2. Core NATS publish (with trace context)
//  3. Core NATS subscribe (handler receives Msg)
//  4. JetStream publish
//  5. JetStream consumer Consume callback (push-based)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	// ---------------------------------------------------------------
	// 1) Create TracerProvider and set global provider + propagator.
	//    All instrumentation packages fall back to otel.GetTracerProvider()
	//    and otel.GetTextMapPropagator() when no explicit option is given.
	// ---------------------------------------------------------------
	tp, err := newTracerProvider()
	if err != nil {
		log.Fatalf("newTracerProvider: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	}()

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// ---------------------------------------------------------------
	// 2) Connect to NATS using the traced wrapper.
	//    otelnats.Connect mirrors nats.Connect but returns *otelnats.Conn.
	// ---------------------------------------------------------------
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	conn, err := otelnats.Connect(natsURL, nil)
	if err != nil {
		log.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// ---------------------------------------------------------------
	// 3) Core NATS Subscribe — handler receives Msg.
	//    m.Msg is the original *nats.Msg; m.Context() carries the
	//    extracted trace context (linked to the producer span).
	// ---------------------------------------------------------------
	sub, err := conn.Subscribe("example.core", func(m otelnats.Msg) {
		// m.Context() contains the consumer span context — pass it
		// to downstream calls (DB, HTTP, etc.) to continue the trace.
		ctx := m.Context()
		_ = ctx // use ctx for downstream instrumented calls

		log.Printf("[Core NATS] received on %s: %s", m.Msg.Subject, string(m.Msg.Data))
	})
	if err != nil {
		log.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// ---------------------------------------------------------------
	// 4) Core NATS Publish — pass context to propagate trace headers.
	//    conn.Publish(ctx, subject, data) injects traceparent/tracestate
	//    into NATS message headers and creates a producer span.
	// ---------------------------------------------------------------
	ctx := context.Background()
	if err := conn.Publish(ctx, "example.core", []byte("hello from core nats")); err != nil {
		log.Printf("Core Publish: %v", err)
	}
	log.Println("Core NATS publish done.")

	// ---------------------------------------------------------------
	// 5) JetStream setup — create stream and publish.
	//    oteljetstream.New wraps the JetStream API with tracing.
	// ---------------------------------------------------------------
	js, err := oteljetstream.New(conn)
	if err != nil {
		log.Printf("JetStream not available: %v", err)
		return
	}

	// Create (or update) a stream. oteljetstream.StreamConfig is a type
	// alias for jetstream.StreamConfig.
	stream, err := js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
		Name:     "EXAMPLE",
		Subjects: []string{"example.>"},
	})
	if err != nil {
		log.Fatalf("CreateOrUpdateStream: %v", err)
	}

	// Publish a message via JetStream (creates a producer span).
	if _, err := js.Publish(ctx, "example.hello", []byte("world")); err != nil {
		log.Printf("JetStream Publish: %v", err)
	}
	log.Println("JetStream publish done.")

	// ---------------------------------------------------------------
	// 6) JetStream Consumer with Consume (push-based callback).
	//    The handler receives oteljetstream.Msg which embeds
	//    jetstream.Msg — use m.Data(), m.Ack(), m.Headers(), and
	//    m.Context() for the trace context.
	// ---------------------------------------------------------------

	// Create a durable consumer. oteljetstream.ConsumerConfig is a type
	// alias for jetstream.ConsumerConfig. DeliverPolicy comes from the
	// jetstream package directly.
	consumer, err := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
		Durable:       "example-consumer",
		AckPolicy:     oteljetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		log.Fatalf("CreateOrUpdateConsumer: %v", err)
	}

	// Consume starts a push-based message loop. The handler receives
	// Msg: m.Context() carries the consumer span (linked to
	// the producer span via the propagated trace headers).
	cc, err := consumer.Consume(func(m oteljetstream.Msg) {
		// m embeds jetstream.Msg — call m.Data(), m.Subject(), etc. directly.
		// m.Context() returns the context with the consumer span.
		ctx := m.Context()
		_ = ctx // use ctx for downstream instrumented calls

		log.Printf("[JetStream] received on %s: %s", m.Subject(), string(m.Data()))

		// Acknowledge the message (required with AckExplicit policy).
		if err := m.Ack(); err != nil {
			log.Printf("Ack error: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("Consume: %v", err)
	}
	defer cc.Stop()

	fmt.Println("Listening... Press Ctrl+C to exit.")

	// Wait for interrupt signal to gracefully shut down.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down.")
}

func newTracerProvider() (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}
	exp, err := otlptracegrpc.New(context.Background(),
		otlptracegrpc.WithEndpoint(endpoint),
	)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			attribute.String("service.name", "otel-nats-example"),
			attribute.String("service.version", "0.0.1"),
		),
	)
	if err != nil {
		return nil, err
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}
