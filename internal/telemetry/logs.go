package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/otelconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// identityKeys are dropped from exported log records, which often go to a
// hosted vendor; stderr keeps them for the operator. A log call that adds a
// key naming a person or a credential adds it here.
var identityKeys = []string{"ip", "email", "subject", "handle", "actor", "family"}

func (t *Telemetry) buildLogs(ctx context.Context, o Options, res *resource.Resource) error {
	exp := o.LogExporter
	if exp == nil {
		ep, err := t.endpoint(o.Logs.OTLP, o, signalLogs, "/v1/logs")
		if err != nil || ep == nil {
			return err
		}
		e, err := ep.logExporter(ctx)
		if err != nil {
			return err
		}
		count := instrument.Handle(otelconv.NewSDKExporterLogExported(t.providers.Meter(scope)))
		exp = &logExporter{Exporter: e, stats: newExportStats("logs", count.AddSet, ep,
			ep.componentType(semconv.OTelComponentTypeOtlpHTTPLogExporter, semconv.OTelComponentTypeOtlpGRPCLogExporter), t.log)}
	}
	opts := []sdklog.LoggerProviderOption{
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	}
	if !envSet("OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT") {
		opts = append(opts, sdklog.WithAttributeValueLengthLimit(maxAttributeValueLength))
	}
	lp := sdklog.NewLoggerProvider(opts...)
	t.logs = otelslog.NewHandler(scope, otelslog.WithLoggerProvider(lp))
	t.logLevel = o.Logs.Level
	t.shutdown = append(t.shutdown, lp.Shutdown)
	return nil
}

// Logger returns l with trace_id and span_id on every record written with a
// sampled span's context, the keys of the W3C convention for logs outside
// OTLP, and, when log export is on, a copy of each record at Logs.Level or
// above for the collector. With neither, it is l itself.
func (t *Telemetry) Logger(l *slog.Logger) *slog.Logger {
	h := l.Handler()
	if t.providers.Tracing() {
		h = correlate{h}
	}
	if t.logs != nil {
		h = slog.NewMultiHandler(h, instrument.MinLevel(withoutIdentity{t.logs}, t.logLevel))
	}
	if h == l.Handler() {
		return l
	}
	return slog.New(h)
}

// correlate leaves trace_flags out: on a sampled span it is always 01.
type correlate struct{ slog.Handler }

func (h correlate) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsSampled() {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h correlate) WithAttrs(attrs []slog.Attr) slog.Handler {
	return correlate{h.Handler.WithAttrs(attrs)}
}

func (h correlate) WithGroup(name string) slog.Handler { return correlate{h.Handler.WithGroup(name)} }

type withoutIdentity struct{ slog.Handler }

func identity(a slog.Attr) bool { return slices.Contains(identityKeys, a.Key) }

func (h withoutIdentity) Handle(ctx context.Context, r slog.Record) error {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = identity(a)
		return !found
	})
	if !found {
		return h.Handler.Handle(ctx, r)
	}
	kept := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if !identity(a) {
			kept.AddAttrs(a)
		}
		return true
	})
	return h.Handler.Handle(ctx, kept)
}

func (h withoutIdentity) WithAttrs(attrs []slog.Attr) slog.Handler {
	return withoutIdentity{h.Handler.WithAttrs(slices.DeleteFunc(slices.Clone(attrs), identity))}
}

func (h withoutIdentity) WithGroup(name string) slog.Handler {
	return withoutIdentity{h.Handler.WithGroup(name)}
}

type logExporter struct {
	sdklog.Exporter
	stats *exportStats
}

func (e *logExporter) Export(ctx context.Context, records []sdklog.Record) error {
	ctx, o := withOutcome(ctx)
	return e.stats.record(ctx, int64(len(records)), e.Exporter.Export(ctx, records), o)
}

func (e *endpoint) logExporter(ctx context.Context) (sdklog.Exporter, error) {
	var (
		exp sdklog.Exporter
		err error
	)
	if e.grpc() {
		opts := []otlploggrpc.Option{
			otlploggrpc.WithGRPCConn(e.conn),
			otlploggrpc.WithTimeout(e.timeout),
			otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
				Enabled: !e.noRetry, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
		}
		if e.headers != nil {
			opts = append(opts, otlploggrpc.WithHeaders(e.headers))
		}
		exp, err = otlploggrpc.New(ctx, opts...)
	} else {
		compression := otlploghttp.NoCompression
		if e.gzip {
			compression = otlploghttp.GzipCompression
		}
		opts := []otlploghttp.Option{
			otlploghttp.WithEndpointURL(e.url.String()),
			otlploghttp.WithHTTPClient(e.http),
			otlploghttp.WithTLSClientConfig(e.tls),
			otlploghttp.WithCompression(compression),
			otlploghttp.WithTimeout(e.timeout),
			otlploghttp.WithRetry(otlploghttp.RetryConfig{
				Enabled: !e.noRetry, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
		}
		if e.headers != nil {
			opts = append(opts, otlploghttp.WithHeaders(e.headers))
		}
		exp, err = otlploghttp.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("telemetry: log exporter: %w", err)
	}
	return exp, nil
}
