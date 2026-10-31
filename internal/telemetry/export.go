package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// outcome is what the last attempt of one export got back from the
// collector. Exporters only return an error, so the HTTP client and the gRPC
// interceptor record the status for error.type.
type outcome struct {
	answered bool
	status   int
	code     codes.Code
}

type outcomeKey struct{}

func withOutcome(ctx context.Context) (context.Context, *outcome) {
	o := &outcome{}
	return context.WithValue(ctx, outcomeKey{}, o), o
}

// errorType classifies a failed export: the status the collector answered
// with, timeout, rejected for a partial success, else _OTHER.
func (o *outcome) errorType(err error) string {
	var ne net.Error
	switch {
	case o.code == codes.DeadlineExceeded, errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case o.status >= 300:
		return strconv.Itoa(o.status)
	case o.code != codes.OK:
		return grpcCodeName(o.code)
	case o.answered:
		return "rejected"
	default:
		return instrument.Other
	}
}

// grpcCodeName spells a code as gRPC's documentation does,
// DEADLINE_EXCEEDED; a code outside the defined ones is _OTHER, since a
// collector chooses it.
func grpcCodeName(c codes.Code) string {
	if c > codes.Unauthenticated {
		return instrument.Other
	}
	s := c.String()
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(rune(s[i-1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

type recordStatus struct{ next http.RoundTripper }

func (t recordStatus) RoundTrip(r *http.Request) (*http.Response, error) {
	o, _ := r.Context().Value(outcomeKey{}).(*outcome)
	if o != nil {
		*o = outcome{}
	}
	resp, err := t.next.RoundTrip(r)
	if o != nil && err == nil {
		o.answered, o.status = true, resp.StatusCode
	}
	return resp, err
}

func recordCode(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	if o, ok := ctx.Value(outcomeKey{}).(*outcome); ok {
		*o = outcome{code: status.Code(err), answered: true}
	}
	return err
}

// exportStats counts the items one exporter handled, as the semantic
// conventions' otel.sdk.exporter.*.exported, and logs failures.
type exportStats struct {
	signal string
	add    func(ctx context.Context, n int64, set attribute.Set)
	base   []attribute.KeyValue
	ok     attribute.Set
	log    *limitedLog
}

func newExportStats(signal string, add func(context.Context, int64, attribute.Set), t *transport, typ attribute.KeyValue, log *limitedLog) *exportStats {
	base := append([]attribute.KeyValue{typ, semconv.OTelComponentName(typ.Value.AsString() + "/0")}, t.server...)
	return &exportStats{signal: signal, add: add, base: base, ok: attribute.NewSet(base...), log: log}
}

// record returns err marked as logged.
func (s *exportStats) record(ctx context.Context, n int64, err error, o *outcome) error {
	if err == nil {
		s.add(ctx, n, s.ok)
		return nil
	}
	typ := o.errorType(err)
	s.add(ctx, n, attribute.NewSet(append(s.base[:len(s.base):len(s.base)], semconv.ErrorTypeKey.String(typ))...))
	s.log.warn(s.signal, "telemetry: export failed", "signal", s.signal, "error_type", typ, "err", err)
	return &exportError{err: err}
}

// metricExporter counts data points. Its counter comes from the meter
// provider that the exporter's own reader feeds, so it is bound after.
type metricExporter struct {
	sdkmetric.Exporter
	stats atomic.Pointer[exportStats]
}

func (e *metricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	ctx, o := withOutcome(ctx)
	err := e.Exporter.Export(ctx, rm)
	if s := e.stats.Load(); s != nil {
		return s.record(ctx, dataPoints(rm), err, o)
	}
	return err
}

func dataPoints(rm *metricdata.ResourceMetrics) int64 {
	var n int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				n += len(d.DataPoints)
			case metricdata.Gauge[float64]:
				n += len(d.DataPoints)
			case metricdata.Sum[int64]:
				n += len(d.DataPoints)
			case metricdata.Sum[float64]:
				n += len(d.DataPoints)
			case metricdata.Histogram[int64]:
				n += len(d.DataPoints)
			case metricdata.Histogram[float64]:
				n += len(d.DataPoints)
			case metricdata.ExponentialHistogram[int64]:
				n += len(d.DataPoints)
			case metricdata.ExponentialHistogram[float64]:
				n += len(d.DataPoints)
			case metricdata.Summary:
				n += len(d.DataPoints)
			}
		}
	}
	return int64(n)
}
