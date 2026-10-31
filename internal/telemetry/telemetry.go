// Package telemetry builds the OpenTelemetry SDK from Options: the resource,
// the tracer and meter providers with their OTLP and Prometheus exporters,
// the Go runtime metrics and the counts of every OTLP export. An option
// Kazari sets wins; one it leaves unset is left to the SDK, which reads the
// standard OTEL_* variables, so Kazari runs like any other OpenTelemetry
// service under an operator's environment.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// OTLP protocols.
const (
	ProtocolHTTP = "http/protobuf"
	ProtocolGRPC = "grpc"
)

// Options configure the SDK.
type Options struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	HostName       string

	OTLP    OTLP
	Traces  Traces
	Metrics Metrics
	Logs    Logs

	Logger *slog.Logger     // nil means slog.Default()
	Now    func() time.Time // nil means time.Now

	// SpanExporter and MetricReader replace their signal's OTLP exporter
	// whatever the endpoint, for tests.
	SpanExporter sdktrace.SpanExporter
	MetricReader sdkmetric.Reader
}

// OTLP is the destination of every exported signal.
type OTLP struct {
	Endpoint    string
	Protocol    string
	Headers     map[string]string
	Compression string
	Timeout     time.Duration
	CAFile      string
	CertFile    string
	KeyFile     string
}

// Traces configures span export and sampling. With both ratios nil,
// OTEL_TRACES_SAMPLER decides when it is set.
type Traces struct {
	OTLP            bool
	SampleRatio     *float64
	SiteSampleRatio *float64
}

// Metrics selects the metric readers. A zero Interval leaves it to
// OTEL_METRIC_EXPORT_INTERVAL.
type Metrics struct {
	Prometheus bool
	OTLP       bool
	Interval   time.Duration
}

// Logs selects the export of log records.
type Logs struct {
	OTLP bool
}

// Telemetry owns the SDK providers.
type Telemetry struct {
	providers instrument.Providers
	metrics   http.Handler
	log       *limitedLog
	shutdown  []func(context.Context) error
}

// New builds the providers. A signal that goes nowhere has a nil provider:
// no endpoint means no tracing, and Prometheus off without a push means no
// metrics. An endpoint is telemetry.otlp.endpoint or the OTLP exporter
// variables.
func New(ctx context.Context, o Options) (*Telemetry, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	t := &Telemetry{log: newLimitedLog(o.Logger, o.Now)}
	installErrorHandler(t.log)
	for _, w := range Warnings(o) {
		o.Logger.Warn("telemetry: " + w)
	}

	// Metrics first: the span exporter counts into the meter provider, and
	// shutting down in reverse flushes spans before the metrics that count
	// their export.
	res := newResource(o)
	if err := t.buildMetrics(ctx, o, res); err != nil {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}
	if err := t.buildTraces(ctx, o, res); err != nil {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}
	return t, nil
}

// Providers returns the providers for every package's Options.
func (t *Telemetry) Providers() instrument.Providers { return t.providers }

// MetricsHandler serves the Prometheus registry, or is nil with Prometheus
// off.
func (t *Telemetry) MetricsHandler() http.Handler { return t.metrics }

// Shutdown flushes and stops every provider, then closes the OTLP
// connection; it is not safe to record afterwards.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for i := len(t.shutdown) - 1; i >= 0; i-- {
		if err := t.shutdown[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	t.shutdown = nil
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("telemetry: shutdown: %w", err)
	}
	return nil
}

// endpoint builds the signal's endpoint when the signal is on, closing it
// at Shutdown; nil means the signal exports nothing over OTLP.
func (t *Telemetry) endpoint(on bool, o Options, signal, path string) (*endpoint, error) {
	if !on {
		return nil, nil
	}
	e, err := newEndpoint(o.OTLP, signal, path, o.Logger)
	if e != nil {
		t.shutdown = append(t.shutdown, func(context.Context) error { return e.close() })
	}
	return e, err
}
