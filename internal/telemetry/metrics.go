package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/otelconv"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/telemetry"

func (t *Telemetry) buildMetrics(ctx context.Context, o Options, res *resource.Resource) error {
	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	readers := 0
	if o.Metrics.Prometheus {
		reader, handler, err := newPrometheus(t.log)
		if err != nil {
			return err
		}
		opts = append(opts, sdkmetric.WithReader(reader))
		readers++
		t.metrics = handler
	}

	var (
		exp *metricExporter
		ep  *endpoint
	)
	reader := o.MetricReader
	if reader == nil {
		var err error
		if ep, err = t.endpoint(o.Metrics.OTLP, o, signalMetrics, "/v1/metrics"); err != nil {
			return err
		}
	}
	if ep != nil {
		e, err := ep.metricExporter(ctx)
		if err != nil {
			return err
		}
		exp = &metricExporter{Exporter: e}
		readerOpts := []sdkmetric.PeriodicReaderOption{sdkmetric.WithProducer(runtime.NewProducer())}
		if o.Metrics.Interval > 0 {
			readerOpts = append(readerOpts, sdkmetric.WithInterval(o.Metrics.Interval))
		}
		reader = sdkmetric.NewPeriodicReader(exp, readerOpts...)
	}
	if reader != nil {
		opts = append(opts, sdkmetric.WithReader(reader))
		readers++
	}
	if readers == 0 {
		return nil
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	t.providers.MeterProvider = mp
	t.shutdown = append(t.shutdown, mp.Shutdown)
	if exp != nil {
		count := instrument.Handle(otelconv.NewSDKExporterMetricDataPointExported(mp.Meter(scope)))
		exp.stats.Store(newExportStats("metrics", count.AddSet, ep,
			ep.componentType(semconv.OTelComponentTypeOtlpHTTPMetricExporter, semconv.OTelComponentTypeOtlpGRPCMetricExporter), t.log))
	}
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return fmt.Errorf("telemetry: runtime metrics: %w", err)
	}
	return nil
}

// newPrometheus serves OpenTelemetry names translated the Prometheus way
// (dots to underscores, unit and _total suffixes) beside client_golang's
// process collector, the one family no Go instrumentation covers.
func newPrometheus(log *limitedLog) (sdkmetric.Reader, http.Handler, error) {
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
	return exp, promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true, ErrorLog: scrapeLog{log}}), nil
}

// scrapeLog reports a failed scrape, which the scraper sees only as a 500.
type scrapeLog struct{ log *limitedLog }

func (l scrapeLog) Println(v ...any) {
	l.log.warn("scrape", "telemetry: metrics scrape failed", "err", strings.TrimSpace(fmt.Sprintln(v...)))
}
