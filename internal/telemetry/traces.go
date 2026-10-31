package telemetry

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/otelconv"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// Kazari's sampling when neither its keys nor OTEL_TRACES_SAMPLER decide:
// every control-plane trace, and one page view in a hundred.
const (
	defaultSampleRatio     = 1.0
	defaultSiteSampleRatio = 0.01
)

// maxAttributeValueLength caps attribute values unless the environment sets
// a limit. The SDK's default is none, and values such as url.path come from
// requests: unbounded, one request could fill the export queue.
const maxAttributeValueLength = 1024

func (t *Telemetry) buildTraces(ctx context.Context, o Options, res *resource.Resource) error {
	exp := o.SpanExporter
	if exp == nil {
		ep, err := t.endpoint(o.Traces.OTLP, o, signalTraces, "/v1/traces")
		if err != nil || ep == nil {
			return err
		}
		e, err := ep.spanExporter(ctx)
		if err != nil {
			return err
		}
		count := instrument.Handle(otelconv.NewSDKExporterSpanExported(t.providers.Meter(scope)))
		exp = &spanExporter{SpanExporter: e, stats: newExportStats("traces", count.AddSet, ep,
			ep.componentType(semconv.OTelComponentTypeOtlpHTTPSpanExporter, semconv.OTelComponentTypeOtlpGRPCSpanExporter), t.log)}
	}
	limits := sdktrace.NewSpanLimits()
	if !envSet("OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT", "OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT") {
		limits.AttributeValueLengthLimit = maxAttributeValueLength
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithRawSpanLimits(limits),
		sdktrace.WithBatcher(exp),
	}
	if s := sampler(o.Traces); s != nil {
		opts = append(opts, sdktrace.WithSampler(s))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	t.providers.TracerProvider = tp
	t.shutdown = append(t.shutdown, tp.Shutdown)
	return nil
}

func envSet(names ...string) bool {
	for _, n := range names {
		if _, ok := os.LookupEnv(n); ok {
			return true
		}
	}
	return false
}

// sampler is Kazari's per-surface sampler, or nil to leave sampling to
// OTEL_TRACES_SAMPLER when Kazari sets no ratio and the variable is set.
func sampler(t Traces) sdktrace.Sampler {
	if t.SampleRatio == nil && t.SiteSampleRatio == nil && envSet("OTEL_TRACES_SAMPLER") {
		return nil
	}
	control, site := defaultSampleRatio, defaultSiteSampleRatio
	if t.SampleRatio != nil {
		control = *t.SampleRatio
	}
	if t.SiteSampleRatio != nil {
		site = *t.SiteSampleRatio
	}
	return newSampler(control, site)
}

// newSampler decides new traces by surface: artifact hosts and the apex at
// site, the control plane at control, and work with no surface, the
// scheduler's and startup's, always. A parent's decision stands, so a trace
// is kept or dropped whole.
func newSampler(control, site float64) sdktrace.Sampler {
	return sdktrace.ParentBased(surfaceSampler{
		control: sdktrace.TraceIDRatioBased(control),
		site:    sdktrace.TraceIDRatioBased(site),
	})
}

type surfaceSampler struct{ control, site sdktrace.Sampler }

func (s surfaceSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	for _, kv := range p.Attributes {
		if kv.Key != instrument.Surface {
			continue
		}
		switch kv.Value.AsString() {
		case instrument.SurfaceSite, instrument.SurfaceApex:
			return s.site.ShouldSample(p)
		default:
			return s.control.ShouldSample(p)
		}
	}
	return sdktrace.AlwaysSample().ShouldSample(p)
}

func (s surfaceSampler) Description() string {
	return fmt.Sprintf("KazariSurface{control:%s,site:%s}", s.control.Description(), s.site.Description())
}

type spanExporter struct {
	sdktrace.SpanExporter
	stats *exportStats
}

func (e *spanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	ctx, o := withOutcome(ctx)
	return e.stats.record(ctx, int64(len(spans)), e.SpanExporter.ExportSpans(ctx, spans), o)
}

func (e *endpoint) spanExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	var (
		exp sdktrace.SpanExporter
		err error
	)
	if e.grpc() {
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithGRPCConn(e.conn),
			otlptracegrpc.WithTimeout(e.timeout),
			otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
				Enabled: !e.noRetry, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
		}
		if e.headers != nil {
			opts = append(opts, otlptracegrpc.WithHeaders(e.headers))
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	} else {
		compression, encoding := otlptracehttp.NoCompression, otlptracehttp.EncodingProtobuf
		if e.gzip {
			compression = otlptracehttp.GzipCompression
		}
		if e.protocol == protocolJSON {
			encoding = otlptracehttp.EncodingJSON
		}
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(e.url.String()),
			otlptracehttp.WithHTTPClient(e.http),
			otlptracehttp.WithTLSClientConfig(e.tls),
			otlptracehttp.WithCompression(compression),
			otlptracehttp.WithEncoding(encoding),
			otlptracehttp.WithTimeout(e.timeout),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
				Enabled: !e.noRetry, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
		}
		if e.headers != nil {
			opts = append(opts, otlptracehttp.WithHeaders(e.headers))
		}
		exp, err = otlptracehttp.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("telemetry: span exporter: %w", err)
	}
	return exp, nil
}
