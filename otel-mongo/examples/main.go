// Example demonstrates how to initialize the OpenTelemetry TracerProvider and
// TextMapPropagator at process startup, then use otelmongo to:
//   - Insert a document with automatic trace context injection (_oteltrace field)
//   - Query documents and extract trace context with DecodeWithContext
//
// The instrumentation package does NOT provide InitTracer; the application is
// responsible for creating and setting the global provider and propagator
// (per OTel Go Contrib instrumentation guidelines).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	otelmongo "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	// ---------------------------------------------------------------
	// 1) Create TracerProvider and set global provider + propagator.
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
	// 2) Connect to MongoDB using NewClient.
	//
	//    NewClient(uri, ...ClientOption) parses the server address from
	//    the URI so that spans include server.address / server.port
	//    semantic convention attributes, and enables synthetic "deliver"
	//    spans for change stream consumers.
	//
	//    It falls back to otel.GetTracerProvider() and
	//    otel.GetTextMapPropagator() set above. To override per-client,
	//    pass WithTracerProvider(tp) or WithPropagators(p).
	// ---------------------------------------------------------------
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}

	client, err := otelmongo.NewClient(uri)
	if err != nil {
		log.Fatalf("NewClient: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	}()

	// Quick connectivity check.
	if err := client.Ping(context.Background(), nil); err != nil {
		log.Printf("Ping failed (is MongoDB running?): %v", err)
		return
	}

	coll := client.Database("mydb").Collection("mycoll")

	// ---------------------------------------------------------------
	// 3) InsertOne — trace injection demonstration.
	//
	//    InsertOne automatically serialises the active trace context
	//    from ctx into the document under the "_oteltrace" field before
	//    the insert. This allows downstream consumers (e.g. change
	//    stream watchers) to extract the originating trace and continue
	//    the same distributed trace.
	// ---------------------------------------------------------------
	tracer := otel.Tracer("otel-mongo-example")
	ctx, span := tracer.Start(context.Background(), "example-workflow")
	defer span.End()

	doc := bson.M{
		"text":      "hello from otelmongo example",
		"createdAt": time.Now(),
	}
	result, err := coll.InsertOne(ctx, doc)
	if err != nil {
		log.Printf("InsertOne: %v", err)
		return
	}
	log.Printf("Inserted document ID: %v", result.InsertedID)

	// ---------------------------------------------------------------
	// 4) Find + Cursor.DecodeWithContext — trace extraction from
	//    documents.
	//
	//    DecodeWithContext decodes the current cursor document into the
	//    target struct AND extracts the "_oteltrace" field to return a
	//    context enriched with the original trace context. This lets
	//    you create child spans that join the trace that produced the
	//    document.
	//
	//    If the document has no "_oteltrace" field (e.g. it was
	//    inserted without otelmongo), the returned context is unchanged.
	// ---------------------------------------------------------------
	cursor, err := coll.Find(ctx, bson.M{"text": "hello from otelmongo example"})
	if err != nil {
		log.Printf("Find: %v", err)
		return
	}
	defer cursor.Close(context.Background())

	type Message struct {
		Text      string    `bson:"text"`
		CreatedAt time.Time `bson:"createdAt"`
	}

	for cursor.Next(context.Background()) {
		var msg Message
		// DecodeWithContext returns a context carrying the trace from
		// the document's _oteltrace field, allowing child spans to
		// continue the original trace.
		docCtx, err := cursor.DecodeWithContext(ctx, &msg)
		if err != nil {
			log.Printf("DecodeWithContext: %v", err)
			continue
		}

		// docCtx now carries the trace context from the inserted document.
		// Any span started from docCtx will be a child of the insert trace.
		_, processSpan := tracer.Start(docCtx, "process-document")
		log.Printf("Found: %q (traceID=%s)", msg.Text,
			trace.SpanFromContext(docCtx).SpanContext().TraceID())
		processSpan.End()
	}

	// ---------------------------------------------------------------
	// 5) Watch + ChangeStream.DecodeWithContext (comment only).
	//
	//    For change stream consumers, the pattern is similar:
	//
	//      cs, err := coll.Watch(ctx, pipeline)
	//      for cs.Next(ctx) {
	//          var event ChangeEvent
	//          docCtx, err := cs.DecodeWithContext(ctx, &event)
	//          // docCtx carries the trace from the changed document.
	//          // Start child spans from docCtx to continue the trace.
	//      }
	//
	//    Watch requires a MongoDB replica set. See the dbwatcher
	//    service in the main project for a complete working example.
	// ---------------------------------------------------------------

	fmt.Println("Example done.")
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
			attribute.String("service.name", "otel-mongo-example"),
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
