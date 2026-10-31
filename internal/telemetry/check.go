package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// CheckResult is how one signal's test export went.
type CheckResult struct {
	Signal   string // traces, metrics or logs
	Protocol string
	Endpoint string
	Err      error
	// Problem says where to look when Err is set.
	Problem string
}

// Check sends one record of each enabled signal (a span "kazari check", a
// gauge kazari.check and a log record "kazari check") to its endpoint, once
// and without retrying, which exercises the protocol, path, TLS and
// headers. Signals without an endpoint are left out. The records stay in the
// backend.
func Check(ctx context.Context, o Options) []CheckResult {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	res := newResource(o)
	var out []CheckResult
	for _, s := range []struct {
		on           bool
		name, signal string
		path         string
		send         func(context.Context, *endpoint, *resource.Resource) error
	}{
		{o.Traces.OTLP, "traces", signalTraces, "/v1/traces", checkSpan},
		{o.Metrics.OTLP, "metrics", signalMetrics, "/v1/metrics", checkMetric},
		{o.Logs.OTLP, "logs", signalLogs, "/v1/logs", checkLog},
	} {
		if !s.on {
			continue
		}
		e, err := newEndpoint(o.OTLP, s.signal, s.path, o.Logger)
		if err != nil {
			out = append(out, CheckResult{Signal: s.name, Err: err, Problem: "fix the endpoint settings"})
			continue
		}
		if e == nil {
			continue
		}
		e.noRetry = true
		r := CheckResult{Signal: s.name, Protocol: e.protocol, Endpoint: e.url.Redacted()}
		ctx, outcome := withOutcome(ctx)
		if r.Err = s.send(ctx, e, res); r.Err != nil {
			r.Problem = problem(outcome.errorType(r.Err))
		}
		_ = e.close()
		out = append(out, r)
	}
	return out
}

func problem(errType string) string {
	switch errType {
	case "401", "403":
		return "the collector refused the credentials; check telemetry.otlp.headers"
	case "404", "405":
		return "no OTLP receiver answers at this address; check telemetry.otlp.endpoint and protocol"
	case "timeout":
		return "the endpoint did not answer in time; check telemetry.otlp.endpoint and the network"
	case "rejected":
		return "the collector accepted the export but rejected the record"
	case instrument.Other:
		return "could not reach the endpoint or verify its certificate; check telemetry.otlp.endpoint and ca_file"
	default:
		return "the collector answered " + errType
	}
}

func checkSpan(ctx context.Context, e *endpoint, res *resource.Resource) error {
	exp, err := e.spanExporter(ctx)
	if err != nil {
		return err
	}
	defer exp.Shutdown(context.WithoutCancel(ctx))
	keep := &lastSpan{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(keep))
	defer tp.Shutdown(context.WithoutCancel(ctx))
	_, span := tp.Tracer(scope).Start(ctx, "kazari check")
	span.End()
	return exp.ExportSpans(ctx, []sdktrace.ReadOnlySpan{keep.span})
}

// lastSpan keeps the span that ended last, for an export Check makes itself
// so it gets the error back.
type lastSpan struct{ span sdktrace.ReadOnlySpan }

func (l *lastSpan) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (l *lastSpan) OnEnd(s sdktrace.ReadOnlySpan)                   { l.span = s }
func (l *lastSpan) Shutdown(context.Context) error                  { return nil }
func (l *lastSpan) ForceFlush(context.Context) error                { return nil }

func checkMetric(ctx context.Context, e *endpoint, res *resource.Resource) error {
	exp, err := e.metricExporter(ctx)
	if err != nil {
		return err
	}
	defer exp.Shutdown(context.WithoutCancel(ctx))
	now := time.Now()
	return exp.Export(ctx, &metricdata.ResourceMetrics{Resource: res, ScopeMetrics: []metricdata.ScopeMetrics{{
		Scope: instrumentation.Scope{Name: scope},
		Metrics: []metricdata.Metrics{{
			Name: "kazari.check", Description: "Sent once by kazari check.", Unit: "1",
			Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{{Time: now, Value: 1}}},
		}},
	}}})
}

func checkLog(ctx context.Context, e *endpoint, res *resource.Resource) error {
	exp, err := e.logExporter(ctx)
	if err != nil {
		return err
	}
	defer exp.Shutdown(context.WithoutCancel(ctx))
	keep := &lastRecord{}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(keep))
	defer lp.Shutdown(context.WithoutCancel(ctx))
	var r otellog.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(otellog.SeverityInfo)
	r.SetBody(attribute.StringValue("kazari check"))
	lp.Logger(scope).Emit(ctx, r)
	return exp.Export(ctx, []sdklog.Record{keep.record})
}

// lastRecord keeps the record emitted last, as lastSpan does for spans.
type lastRecord struct{ record sdklog.Record }

func (l *lastRecord) OnEmit(_ context.Context, r *sdklog.Record) error {
	l.record = r.Clone()
	return nil
}
func (l *lastRecord) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (l *lastRecord) Shutdown(context.Context) error                         { return nil }
func (l *lastRecord) ForceFlush(context.Context) error                       { return nil }
