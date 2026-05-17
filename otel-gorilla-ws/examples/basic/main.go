// Example demonstrates how to initialize the OpenTelemetry TracerProvider and
// TextMapPropagator at process startup, then use otelgorillaws.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
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

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Use otelgorillaws.NewConn after gorilla/websocket upgrade."))
	})
	log.Println("Example server")
	srv := &http.Server{
		Addr:              ":0",
		Handler:           nil,
		ReadHeaderTimeout: 10 * time.Second,
	}
	_ = srv.ListenAndServe()
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
			attribute.String("service.name", "otel-gorilla-ws-example"),
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
