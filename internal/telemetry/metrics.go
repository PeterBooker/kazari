package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/otelconv"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/telemetry"

// cardinalityLimit is a safety net, not a budget: every attribute is
// bounded, so an otel_metric_overflow series means one escaped its bound.
const cardinalityLimit = 2000

func (t *Telemetry) buildMetrics(ctx context.Context, o Options, res *resource.Resource) error {
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithExemplarFilter(exemplar.TraceBasedFilter),
		sdkmetric.WithCardinalityLimit(cardinalityLimit),
		// The runtime instrumentation's pre-semconv names, which
		// OTEL_GO_X_DEPRECATED_RUNTIME_METRICS would turn on.
		sdkmetric.WithView(
			dropView("process.runtime.go.*"),
			dropView("runtime.uptime"),
		),
	}
	readers := 0
	if o.Metrics.Prometheus {
		reader, handler, err := newPrometheus()
		if err != nil {
			return err
		}
		opts = append(opts, sdkmetric.WithReader(reader))
		readers++
		t.metrics = handler
	}

	var exp *metricExporter
	reader := o.MetricReader
	if reader == nil && t.transport != nil {
		e, err := t.transport.metricExporter(ctx)
		if err != nil {
			return err
		}
		exp = &metricExporter{Exporter: e}
		reader = sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(o.Metrics.Interval),
			sdkmetric.WithTimeout(exportTimeout),
			sdkmetric.WithProducer(runtime.NewProducer()),
		)
	}
	if reader != nil {
		opts = append(opts, sdkmetric.WithReader(reader))
		readers++
	}
	if readers == 0 {
		t.noopMetrics()
		return nil
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	t.providers.MeterProvider = mp
	t.shutdown = append(t.shutdown, mp.Shutdown)
	if exp != nil {
		count := instrument.Handle(otelconv.NewSDKExporterMetricDataPointExported(mp.Meter(scope)))
		exp.stats.Store(newExportStats("metrics", count.AddSet, t.transport,
			t.transport.componentType(semconv.OTelComponentTypeOtlpHTTPMetricExporter, semconv.OTelComponentTypeOtlpGRPCMetricExporter), t.log))
	}
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return fmt.Errorf("telemetry: runtime metrics: %w", err)
	}
	return nil
}

func dropView(name string) sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: name, Scope: instrumentation.Scope{Name: runtime.ScopeName}},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
	)
}

// newPrometheus serves OpenTelemetry names translated the Prometheus way
// (dots to underscores, unit and _total suffixes) beside client_golang's
// process collector, the one family no Go instrumentation covers.
func newPrometheus() (sdkmetric.Reader, http.Handler, error) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, nil, fmt.Errorf("telemetry: %w", err)
	}
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		// Pinned: the exporter's default follows prometheus/common's
		// validation scheme, which keeps the dots.
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		otelprom.WithoutScopeInfo(),
		otelprom.WithProducer(runtime.NewProducer()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: prometheus: %w", err)
	}
	return exp, promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}), nil
}
