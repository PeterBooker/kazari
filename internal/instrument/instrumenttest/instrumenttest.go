// Package instrumenttest records what a package sends to OpenTelemetry, for
// its tests: the SDK with a manual reader and a span recorder behind the
// instrument.Providers a package's Options take.
package instrumenttest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// Recorder holds what was recorded since New.
type Recorder struct {
	// Providers go into the Options of the package under test.
	Providers instrument.Providers

	t      testing.TB
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
	tracer trace.Tracer
}

// New returns a Recorder that shuts its providers down with the test.
func New(t testing.TB) *Recorder {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
	})
	return &Recorder{
		Providers: instrument.Providers{TracerProvider: tp, MeterProvider: mp},
		t:         t, reader: reader, spans: spans, tracer: tp.Tracer("instrumenttest"),
	}
}

// Start returns a context under a recording span, as a traced request gives
// a package, and the function that ends it. Kazari's packages start spans
// only under a recording parent.
func (r *Recorder) Start(name string) (context.Context, func()) {
	ctx, span := r.tracer.Start(context.Background(), name)
	return ctx, func() { span.End() }
}

// Span returns the ended span called name, and whether there is one.
func (r *Recorder) Span(name string) (sdktrace.ReadOnlySpan, bool) {
	for _, s := range r.spans.Ended() {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

// SpanAttr returns the value of an attribute of the ended span called name,
// as a string, or "" when the span or the attribute is missing.
func (r *Recorder) SpanAttr(name string, key attribute.Key) string {
	s, ok := r.Span(name)
	if !ok {
		return ""
	}
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value.String()
		}
	}
	return ""
}

// Sum adds up the data points of the metric called name whose attributes
// include every one of want: a counter's total, a gauge's value, or how
// many values a histogram recorded. ok is false when no point matches.
func (r *Recorder) Sum(name string, want ...attribute.KeyValue) (total float64, ok bool) {
	r.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		r.t.Fatal(err)
	}
	add := func(set attribute.Set, v float64) {
		for _, kv := range want {
			if got, has := set.Value(kv.Key); !has || got != kv.Value {
				return
			}
		}
		total, ok = total+v, true
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					add(p.Attributes, float64(p.Value))
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					add(p.Attributes, p.Value)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					add(p.Attributes, float64(p.Value))
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					add(p.Attributes, p.Value)
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					add(p.Attributes, float64(p.Count))
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					add(p.Attributes, float64(p.Count))
				}
			default:
				r.t.Fatalf("instrumenttest: metric %s has data of type %T", name, m.Data)
			}
		}
	}
	return total, ok
}

// Want fails the test unless Sum(name, attrs...) is want.
func (r *Recorder) Want(name string, want float64, attrs ...attribute.KeyValue) {
	r.t.Helper()
	if got, _ := r.Sum(name, attrs...); got != want {
		r.t.Errorf("%s%v = %v, want %v", name, attrs, got, want)
	}
}
