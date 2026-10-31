// Package telemetry builds the OpenTelemetry SDK from Options: the resource,
// the meter provider with its Prometheus and OTLP readers, the Go runtime
// metrics and the counts of every OTLP export. Every setting the SDK would
// otherwise read from an OTEL_* variable is passed explicitly, so the
// environment never changes what Kazari exports.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// OTLP protocols.
const (
	ProtocolHTTP = "http/protobuf"
	ProtocolGRPC = "grpc"
)

// exportTimeout bounds one export with its retries, for every signal.
const exportTimeout = 30 * time.Second

// Options configure the SDK.
type Options struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	HostName       string

	OTLP    OTLP
	Metrics Metrics

	Logger *slog.Logger     // nil means slog.Default()
	Now    func() time.Time // nil means time.Now

	// MetricReader replaces the OTLP metric reader whatever the endpoint,
	// for tests.
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

// Metrics selects the metric readers.
type Metrics struct {
	Prometheus bool
	OTLP       bool
	Interval   time.Duration
}

// Telemetry owns the SDK providers.
type Telemetry struct {
	providers instrument.Providers
	metrics   http.Handler
	transport *transport
	log       *limitedLog
	shutdown  []func(context.Context) error
}

// New builds the providers. Without an endpoint and with Prometheus off it
// returns no-op providers.
func New(ctx context.Context, o Options) (*Telemetry, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	t := &Telemetry{log: newLimitedLog(o.Logger, o.Now)}
	installErrorHandler(t.log)

	if o.OTLP.Endpoint != "" && o.Metrics.OTLP {
		tr, err := newTransport(o.OTLP, o.Logger)
		if err != nil {
			return nil, err
		}
		t.transport = tr
		t.shutdown = append(t.shutdown, func(context.Context) error { return tr.close() })
	}

	if err := t.buildMetrics(ctx, o, newResource(o)); err != nil {
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

func (t *Telemetry) noopMetrics() {
	t.providers.MeterProvider = metricnoop.NewMeterProvider()
}
