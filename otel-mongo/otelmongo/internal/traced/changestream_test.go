package traced

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestChangeStreamReaderAttrs_CarriesStaticServerAddr locks the invariant that the
// ChangeStream reader's getMore spans keep the static server.address/
// server.port snapshot. Those spans are seeded from baseSpanOpts built by Watch and
// never get a post-call ServerAttributes overwrite, so if DBAttributes stops emitting
// server.* (as it does since the emit-once-post-call refactor) the reader attrs must
// append ServerAttributes explicitly or the spans carry no server.address at all.
func TestChangeStreamReaderAttrs_CarriesStaticServerAddr(t *testing.T) {
	impl := &Collection{
		ServerAddr: "static-cs-host",
		ServerPort: 27019, // non-default: proves server.port survives too
	}
	attrs := impl.changeStreamReaderAttrs("db", "coll")

	var addr string
	var port int64
	var sawPort bool
	for _, kv := range attrs {
		switch string(kv.Key) {
		case "server.address":
			addr = kv.Value.AsString()
		case "server.port":
			port = kv.Value.AsInt64()
			sawPort = true
		}
	}
	assert.Equal(t, "static-cs-host", addr, "reader spans must keep the static server.address")
	require.True(t, sawPort, "expected server.port for a non-default static port")
	assert.Equal(t, int64(27019), port)
}

func TestBuildLinkedSpanCtx_NewTraceIDAndLinksOriginTrace(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	tracer := tp.Tracer("test")
	originCtx, originSpan := tracer.Start(context.Background(), "origin")
	originSpanCtx := originSpan.SpanContext()
	originSpan.End()

	// Pass a context that still carries origin span context, and ensure the helper detaches it.
	newCtx, span := buildLinkedSpanCtx(originCtx, tracer, "test-decode-span", nil, originSpanCtx)
	span.End()

	recovered := trace.SpanContextFromContext(newCtx)
	require.True(t, recovered.IsValid())
	assert.NotEqual(t, originSpanCtx.TraceID(), recovered.TraceID())

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "test-decode-span" {
			continue
		}
		found = true
		links := s.Links()
		require.NotEmpty(t, links)
		assert.Equal(t, originSpanCtx.TraceID(), links[0].SpanContext.TraceID())
		break
	}
	require.True(t, found, `expected a span named "test-decode-span"`)
}
