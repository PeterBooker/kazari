package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/cli"
)

// BenchmarkSiteServe serves a 5 KiB file through the whole public chain:
// host router, middleware and the site handler.
func BenchmarkSiteServe(b *testing.B) {
	in := start(b, testConfig(b))
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
