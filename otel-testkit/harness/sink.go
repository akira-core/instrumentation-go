package harness

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Link is a flattened view of a span link as received over OTLP.
type Link struct {
	TraceID    string
	SpanID     string
	TraceState string
}

// Span is a flattened, assertion-friendly view of a single exported span.
type Span struct {
	ServiceName  string
	Scope        string
	Name         string
	TraceID      string
	SpanID       string
	ParentSpanID string
	TraceState   string
	Attributes   map[string]string
	Links        []Link
}

// RV returns the consistent-sampling randomness value parsed from the span's
// tracestate ("ot=rv:<14 hex>"), if present.
func (s Span) RV() (uint64, bool) {
	v, ok := otSubValue(s.TraceState, "rv")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// TH returns the threshold value written by the sampler ("ot=th:<hex>"), if present.
func (s Span) TH() (string, bool) {
	return otSubValue(s.TraceState, "th")
}

// otSubValue extracts a sub-value (e.g. "rv", "th") from the "ot" member of a
// W3C tracestate string of the form "ot=rv:abcd;th:80,vendor=...".
func otSubValue(traceState, key string) (string, bool) {
	var ot string
	for _, member := range strings.Split(traceState, ",") {
		member = strings.TrimSpace(member)
		if strings.HasPrefix(member, "ot=") {
			ot = member[len("ot="):]
			break
		}
	}
	if ot == "" {
		return "", false
	}
	for _, kv := range strings.Split(ot, ";") {
		if strings.HasPrefix(kv, key+":") {
			return kv[len(key)+1:], true
		}
	}
	return "", false
}

// Sink is an in-process OTLP/gRPC trace receiver. The collector container
// re-exports received spans back to this sink so tests can assert on them.
type Sink struct {
	coltracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	spans []Span

	srv  *grpc.Server
	port int
}

// StartSink starts the in-process OTLP/gRPC sink on a free host port and
// registers shutdown via t.Cleanup.
func StartSink(t *testing.T) *Sink {
	t.Helper()
	// Bind to localhost; the testcontainers host-gateway tunnel reaches it.
	lis, err := net.Listen("tcp", "127.0.0.1:0") //nolint:gosec // local test sink
	if err != nil {
		t.Fatalf("sink listen: %v", err)
	}
	s := &Sink{
		srv:  grpc.NewServer(),
		port: lis.Addr().(*net.TCPAddr).Port,
	}
	coltracepb.RegisterTraceServiceServer(s.srv, s)
	go func() { _ = s.srv.Serve(lis) }()
	t.Cleanup(s.srv.Stop)
	return s
}

// Port returns the host port the sink listens on (passed to the collector via
// host.testcontainers.internal:<port>).
func (s *Sink) Port() int { return s.port }

// Export implements coltracepb.TraceServiceServer.
func (s *Sink) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		service := attrString(rs.GetResource().GetAttributes(), "service.name")
		for _, ss := range rs.GetScopeSpans() {
			scope := ss.GetScope().GetName()
			for _, sp := range ss.GetSpans() {
				s.spans = append(s.spans, flattenSpan(service, scope, sp))
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func flattenSpan(service, scope string, sp *tracepb.Span) Span {
	attrs := make(map[string]string, len(sp.GetAttributes()))
	for _, kv := range sp.GetAttributes() {
		attrs[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	links := make([]Link, 0, len(sp.GetLinks()))
	for _, l := range sp.GetLinks() {
		links = append(links, Link{
			TraceID:    hex.EncodeToString(l.GetTraceId()),
			SpanID:     hex.EncodeToString(l.GetSpanId()),
			TraceState: l.GetTraceState(),
		})
	}
	return Span{
		ServiceName:  service,
		Scope:        scope,
		Name:         sp.GetName(),
		TraceID:      hex.EncodeToString(sp.GetTraceId()),
		SpanID:       hex.EncodeToString(sp.GetSpanId()),
		ParentSpanID: hex.EncodeToString(sp.GetParentSpanId()),
		TraceState:   sp.GetTraceState(),
		Attributes:   attrs,
		Links:        links,
	}
}

func attrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return anyValueString(kv.GetValue())
		}
	}
	return ""
}

func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(val.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", v.GetValue())
	}
}

// Spans returns a copy of all spans received so far.
func (s *Sink) Spans() []Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Span, len(s.spans))
	copy(out, s.spans)
	return out
}

// ByRun returns all spans tagged with the given RunAttr ("chain.run") value.
func (s *Sink) ByRun(runID string) []Span {
	return SpansByAttr(s.Spans(), RunAttr, runID)
}

// WaitFor polls until pred is satisfied by the accumulated spans or timeout
// elapses, returning the final snapshot. It absorbs export/forwarding latency.
func (s *Sink) WaitFor(timeout time.Duration, pred func([]Span) bool) []Span {
	deadline := time.Now().Add(timeout)
	for {
		spans := s.Spans()
		if pred(spans) {
			return spans
		}
		if time.Now().After(deadline) {
			return spans
		}
		time.Sleep(50 * time.Millisecond)
	}
}
