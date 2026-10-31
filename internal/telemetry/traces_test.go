package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

func ratio(r float64) *float64 { return &r }

func TestTracesOffWithoutEndpoint(t *testing.T) {
	tel := newTelemetry(t, Options{Traces: Traces{OTLP: true}, Metrics: Metrics{Prometheus: true}})
	if tel.Providers().Tracing() {
		t.Error("tracing is on without an endpoint")
	}
}

func TestTracesOTLPHTTP(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	tel := newTelemetry(t, Options{
		OTLP:   OTLP{Endpoint: srv.URL + "/otlp", Protocol: ProtocolHTTP, Headers: map[string]string{"X-Team": "ops"}, Compression: "gzip"},
		Traces: Traces{OTLP: true},
	})
	_, span := tel.Providers().Tracer("test").Start(context.Background(), "kazari.start")
	span.End()
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := recv.requests()
	if len(got) != 1 {
		t.Fatalf("exports = %d, want only the spans", len(got))
	}
	if got[0].path != "/otlp/v1/traces" || got[0].header.Get("X-Team") != "ops" ||
		got[0].header.Get("Content-Type") != "application/x-protobuf" || got[0].encoding != "gzip" {
		t.Fatalf("path %q, headers %v, encoding %q", got[0].path, got[0].header, got[0].encoding)
	}
	req := decodeTraces(t, got[0].body)
	if n := len(req.ResourceSpans[0].ScopeSpans[0].Spans); n != 1 || req.ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "kazari.start" {
		t.Errorf("spans = %v", req.ResourceSpans)
	}
}

func TestTracesOTLPGRPC(t *testing.T) {
	recv := &grpcReceiver{}
	addr := startGRPC(t, recv)
	tel := newTelemetry(t, Options{
		OTLP:   OTLP{Endpoint: "http://" + addr, Protocol: ProtocolGRPC, Compression: "gzip"},
		Traces: Traces{OTLP: true},
	})
	_, span := tel.Providers().Tracer("test").Start(context.Background(), "job sweep")
	span.End()
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.traces) != 1 || recv.traces[0].ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "job sweep" {
		t.Errorf("traces = %v", recv.traces)
	}
}

func TestSampler(t *testing.T) {
	sampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true})
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, Remote: true})
	tests := []struct {
		name          string
		surface       string
		parent        trace.SpanContext
		control, site float64
		want          bool
	}{
		{"site at 0", instrument.SurfaceSite, trace.SpanContext{}, 1, 0, false},
		{"apex at 0", instrument.SurfaceApex, trace.SpanContext{}, 1, 0, false},
		{"site at 1", instrument.SurfaceSite, trace.SpanContext{}, 0, 1, true},
		{"api at 1", instrument.SurfaceAPI, trace.SpanContext{}, 1, 0, true},
		{"mcp at 0", instrument.SurfaceMCP, trace.SpanContext{}, 0, 1, false},
		{"admin at 0", instrument.SurfaceAdmin, trace.SpanContext{}, 0, 1, false},
		{"no surface always", "", trace.SpanContext{}, 0, 0, true},
		{"sampled parent wins", instrument.SurfaceAPI, sampled, 0, 0, true},
		{"unsampled parent wins", instrument.SurfaceAPI, unsampled, 1, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.parent.IsValid() {
				ctx = trace.ContextWithRemoteSpanContext(ctx, tt.parent)
			}
			var attrs []attribute.KeyValue
			if tt.surface != "" {
				attrs = append(attrs, instrument.Surface.String(tt.surface))
			}
			res := newSampler(tt.control, tt.site).ShouldSample(sdktrace.SamplingParameters{
				ParentContext: ctx, TraceID: trace.TraceID{0xff}, Name: "GET", Attributes: attrs,
			})
			if got := res.Decision == sdktrace.RecordAndSample; got != tt.want {
				t.Errorf("sampled = %v, want %v", got, tt.want)
			}
		})
	}
}

// spanSink keeps what it exported after Shutdown, which the in-memory
// exporter of tracetest forgets.
type spanSink struct{ *tracetest.InMemoryExporter }

func (spanSink) Shutdown(context.Context) error { return nil }

func newSink() spanSink { return spanSink{tracetest.NewInMemoryExporter()} }

func TestSpanLimits(t *testing.T) {
	exp := newSink()
	tel := newTelemetry(t, Options{SpanExporter: exp})
	_, span := tel.Providers().Tracer("test").Start(context.Background(), "x")
	for i := range 140 {
		span.SetAttributes(attribute.Int(fmt.Sprintf("a%d", i), i))
	}
	span.SetAttributes(attribute.String("long", strings.Repeat("x", 2000)))
	span.RecordError(errors.New("boom"))
	span.End()
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := exp.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans = %d", len(got))
	}
	s := got[0]
	if len(s.Attributes) != 128 || s.DroppedAttributes != 13 {
		t.Errorf("attributes = %d, dropped %d; want the SDK's 128 and 13", len(s.Attributes), s.DroppedAttributes)
	}
	for _, kv := range s.Attributes {
		if kv.Key == "long" && len(kv.Value.AsString()) != maxAttributeValueLength {
			t.Errorf("long value kept %d bytes, want %d", len(kv.Value.AsString()), maxAttributeValueLength)
		}
	}
	if len(s.Events) != 1 || len(s.Events[0].Attributes) == 0 {
		t.Errorf("exception event = %+v, want its attributes kept", s.Events)
	}
}
