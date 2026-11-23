package app_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/apiclient"
	"github.com/PeterBooker/kazari/internal/app"
)

type discardSpans struct{}

func (discardSpans) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardSpans) Shutdown(context.Context) error                             { return nil }

// BenchmarkSiteServe serves a 5 KiB file through the whole public chain:
// host router, middleware and the site handler, with telemetry off (the
// Prometheus metrics alone) and with tracing at two site sample ratios, and
// one file of a version with many, telemetry off.
func BenchmarkSiteServe(b *testing.B) {
	for _, bc := range []struct {
		name  string
		ratio *float64
	}{
		{"off", nil},
		{"traced-1pct", new(0.01)},
		{"traced-100pct", new(1.0)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			cfg := testConfig(b)
			opts := app.Options{}
			if bc.ratio != nil {
				cfg.Telemetry.Traces.SiteSampleRatio = bc.ratio
				opts.SpanExporter = discardSpans{}
			}
			benchSite(b, startWith(b, cfg, opts), 1)
		})
	}
	for _, n := range []int{200, 2000} {
		b.Run("files-"+strconv.Itoa(n), func(b *testing.B) {
			benchSite(b, startWith(b, testConfig(b), app.Options{}), n)
		})
	}
}

// benchSite publishes index.html and files-1 more stylesheets, and requests
// the last stylesheet when there are any.
func benchSite(b *testing.B, in *instance, files int) {
	b.Helper()
	hc := client(in.app.Addrs().HTTP)
	ctx := context.Background()
	admin := apiclient.New("http://admin.localhost", adminToken, apiclient.Options{HTTP: hc})
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter"}); err != nil {
		b.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{})
	if err != nil {
		b.Fatal(err)
	}
	page := "<!doctype html><title>Bench</title>" + strings.Repeat("x", 5<<10)
	user := apiclient.New("http://api.localhost", issued.Key, apiclient.Options{HTTP: hc})
	content := map[string]string{"index.html": page}
	target := "index.html"
	for i := 1; i < files; i++ {
		target = fmt.Sprintf("css/%04d.css", i)
		content[target] = fmt.Sprintf("body{order:%d}", i)
	}
	if _, err := user.CreateBundle(ctx, api.CreateManifest{Slug: "bench"}, bundle(b, content)); err != nil {
		b.Fatal(err)
	}

	h := in.app.Handler()
	req := httptest.NewRequest(http.MethodGet, "http://bench.localhost/"+target, nil)
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}
