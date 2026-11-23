package app_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/apiclient"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/views"
)

// open loads url the way a browser opens a page in a tab, with edit
// changing the request first.
func open(t *testing.T, addr, url string, edit func(r *http.Request)) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("User-Agent", "Mozilla/5.0 Firefox/140.0")
	if edit != nil {
		edit(req)
	}
	resp, err := client(addr).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
}

func publishPage(t *testing.T, ctx context.Context, addr, slug string) *apiclient.Client {
	t.Helper()
	admin := apiclient.New("http://admin.localhost", adminToken, apiclient.Options{HTTP: client(addr)})
	alice := member(t, ctx, admin, addr, "alice")
	if _, err := alice.Create(ctx, api.CreateArtifactRequest{Slug: slug, Files: []api.FileInput{
		{Path: "index.html", Content: "<h1>page</h1>"},
		{Path: "app.js", Content: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	return admin
}

// Pages opened in a browser count; their scripts, prefetches and bots do
// not, and an admin reads the count once the flush job has run.
func TestViewsCounted(t *testing.T) {
	in := start(t, testConfig(t))
	ctx := t.Context()
	addr := in.app.Addrs().HTTP
	admin := publishPage(t, ctx, addr, "demo")

	open(t, addr, "http://demo.localhost/", nil)
	open(t, addr, "http://demo--v1.localhost/", nil)
	open(t, addr, "http://demo.localhost/app.js", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "script") })
	open(t, addr, "http://demo.localhost/", func(r *http.Request) { r.Header.Set("Sec-Purpose", "prefetch") })
	open(t, addr, "http://demo.localhost/", func(r *http.Request) { r.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0") })
	if err := in.app.Scheduler().Trigger(ctx, "views-flush"); err != nil {
		t.Fatalf("trigger views-flush: %v", err)
	}

	got, err := admin.ArtifactViews(ctx, "demo", 1)
	if err != nil {
		t.Fatalf("ArtifactViews(demo): %v", err)
	}
	if want := (api.ViewTotals{Views: 2, Visitors: 1}); got.Totals != want {
		t.Errorf("ArtifactViews(demo).Totals = %+v, want %+v", got.Totals, want)
	}
}

// The views counted since the last flush are written when the server stops.
func TestViewsFlushedAtShutdown(t *testing.T) {
	cfg := testConfig(t)
	in := start(t, cfg)
	ctx := t.Context()
	addr := in.app.Addrs().HTTP
	publishPage(t, ctx, addr, "demo")
	open(t, addr, "http://demo.localhost/", nil)

	in.stop()
	select {
	case err := <-in.done:
		in.done <- err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	db, err := sqlite.Open(ctx, cfg.DBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rep, err := views.New(sqlite.NewViewStore(db), views.Options{Now: in.clock.Now}).Report(ctx, "demo", 1)
	if err != nil {
		t.Fatalf("Report(demo): %v", err)
	}
	if rep.Totals.Views != 1 {
		t.Errorf("views of demo after shutdown = %d, want 1", rep.Totals.Views)
	}
}

func TestViewsOff(t *testing.T) {
	cfg := testConfig(t)
	cfg.Analytics.Enabled = false
	in := start(t, cfg)
	ctx := t.Context()
	addr := in.app.Addrs().HTTP
	admin := publishPage(t, ctx, addr, "demo")
	open(t, addr, "http://demo.localhost/", nil)
	if err := in.app.Scheduler().Trigger(ctx, "views-flush"); err == nil {
		t.Error("views-flush ran with analytics off")
	}
	if _, err := admin.ViewSummary(ctx, 1); err == nil {
		t.Error("ViewSummary with analytics off succeeded")
	}
}
