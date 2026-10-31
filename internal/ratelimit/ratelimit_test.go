package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/instrument"
)

func TestLimiterBounded(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1, Options{Now: func() time.Time { return now }})
	if d := l.Reserve(context.Background(), "victim"); d != 0 {
		t.Fatalf("first request delayed %v", d)
	}
	for i := range MaxEntries + 500 {
		l.Reserve(context.Background(), fmt.Sprintf("k%d", i))
	}
	if n := l.Len(); n > MaxEntries {
		t.Fatalf("entries = %d, want at most %d", n, MaxEntries)
	}
	now = now.Add(idle + sweepEvery + time.Second)
	l.Reserve(context.Background(), "fresh")
	if n := l.Len(); n != 1 {
		t.Errorf("entries after idle sweep = %d, want 1", n)
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter = New(0, Options{})
	if l != nil {
		t.Fatal("New(0) is not nil")
	}
	if d := l.Reserve(context.Background(), "k"); d != 0 {
		t.Errorf("nil limiter delayed %v", d)
	}
}

func TestMiddleware(t *testing.T) {
	now := time.Unix(0, 0)
	h := Middleware(New(2, Options{Now: func() time.Time { return now }}), IPKey, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	var got []int
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		got = append(got, rec.Code)
		if rec.Code == http.StatusTooManyRequests {
			if ra := rec.Header().Get("Retry-After"); ra != "30" {
				t.Errorf("Retry-After = %q, want 30", ra)
			}
			if body := rec.Body.String(); body != `{"error":{"code":"rate_limited","message":"too many requests"}}`+"\n" {
				t.Errorf("body = %q", body)
			}
		}
	}
	if want := []int{204, 204, 429}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

func TestIPKey(t *testing.T) {
	tests := []struct {
		remote, want string
	}{
		{"192.0.2.7:1234", "192.0.2.7"},
		{"[2001:db8:1:2:aaaa::1]:443", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:bbbb::9]:443", "2001:db8:1:2::/64"},
		{"[::ffff:192.0.2.7]:80", "192.0.2.7"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		if got := IPKey(r); got != tt.want {
			t.Errorf("IPKey(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

func TestRejectionsCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())

	now := time.Unix(0, 0)
	l := New(1, Options{Now: func() time.Time { return now }, Scope: ScopeCredential, Telemetry: instrument.Providers{MeterProvider: mp}})
	ctx, span := tp.Tracer("test").Start(context.Background(), "POST /v1/artifacts")
	for range 3 {
		l.Reserve(ctx, "k")
	}
	span.End()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kazari.rate_limit.rejected_requests" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if v, _ := dp.Attributes.Value(instrument.RateLimitScope); v.AsString() != ScopeCredential {
					t.Errorf("scope = %q", v.AsString())
				}
				total += dp.Value
			}
		}
	}
	if total != 2 {
		t.Errorf("rejections = %d, want 2", total)
	}
	if ev := rec.Ended()[0].Events(); len(ev) != 2 || ev[0].Name != "kazari.rate_limited" {
		t.Errorf("events = %v, want two kazari.rate_limited", ev)
	}
}
