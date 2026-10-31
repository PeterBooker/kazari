package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/cli"
)

type discardSpans struct{}

func (discardSpans) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardSpans) Shutdown(context.Context) error                             { return nil }

// BenchmarkSiteServe serves a 5 KiB file through the whole public chain:
// host router, middleware and the site handler, with telemetry off (the
// Prometheus metrics alone) and with tracing at two site sample ratios.
func BenchmarkSiteServe(b *testing.B) {
	for _, bc := range []struct {
		name  string
		ratio *float64
	}{
		{"off", nil},
		{"traced-1pct", ptr(0.01)},
		{"traced-100pct", ptr(1.0)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			cfg := testConfig(b)
			opts := app.Options{}
			if bc.ratio != nil {
				cfg.Telemetry.Traces.SiteSampleRatio = bc.ratio
				opts.SpanExporter = discardSpans{}
			}
			benchSite(b, startWith(b, cfg, opts))
		})
	}
}

func ptr[T any](v T) *T { return &v }

func benchSite(b *testing.B, in *instance) {
	hc := client(in.app.Addrs().HTTP)
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter"}); err != nil {
		b.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{})
	if err != nil {
		b.Fatal(err)
	}
	page := "<!doctype html><title>Bench</title>" + strings.Repeat("x", 5<<10)
	user := cli.NewClient("http://api.localhost", issued.Key, hc)
	if _, err := user.CreateBundle(ctx, api.CreateManifest{Slug: "bench"}, bundle(b, map[string]string{"index.html": page})); err != nil {
		b.Fatal(err)
	}

	h := in.app.Handler()
	req := httptest.NewRequest(http.MethodGet, "http://bench.localhost/index.html", nil)
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}
