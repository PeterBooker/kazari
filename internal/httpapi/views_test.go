package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/views"
)

// view counts one page view of slug's index.html, by access, from ip.
func (e *env) view(slug string, a views.Access, ip string) {
	e.t.Helper()
	p, err := e.art.ResolvePublished(e.ctx, slug)
	if err != nil {
		e.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+slug+".art.test/", nil)
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("User-Agent", "Firefox")
	e.views.Record(r, views.Hit{ArtifactID: p.ArtifactID, Path: "index.html", Access: a, IP: ip, UA: "Firefox"})
}

func TestAdminViews(t *testing.T) {
	e := newEnv(t, config{})
	e.member("bob", nil)
	e.create(e.alice, "demo")
	e.create(e.alice, "quiet")
	e.view("demo", views.Access{Via: views.ViaPublic}, "192.0.2.1")
	e.view("demo", views.Access{Via: views.ViaMember, Handle: "bob"}, "192.0.2.2")
	if err := e.views.Flush(e.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var sum api.ViewSummary
	e.call(http.MethodGet, e.a("/v1/views?days=2"), adminToken, nil, http.StatusOK, &sum)
	wantSum := api.ViewSummary{
		From: "2026-10-23", To: "2026-10-24",
		Days: []api.ViewDay{
			{Day: "2026-10-23"},
			{Day: "2026-10-24", Views: 2, Visitors: 2, MemberViews: 1},
		},
		Totals: api.ViewTotals{Views: 2, Visitors: 2, MemberViews: 1},
		Top:    []api.ArtifactViewTotals{{Slug: "demo", Views: 2, Visitors: 2, MemberViews: 1}},
	}
	if diff := cmp.Diff(wantSum, sum); diff != "" {
		t.Errorf("GET /v1/views?days=2 (-want +got):\n%s", diff)
	}

	var art api.ArtifactViews
	e.call(http.MethodGet, e.a("/v1/artifacts/demo/views?days=1"), adminToken, nil, http.StatusOK, &art)
	wantArt := api.ArtifactViews{
		Slug: "demo", From: "2026-10-24", To: "2026-10-24",
		Days:    []api.ViewDay{{Day: "2026-10-24", Views: 2, Visitors: 2, MemberViews: 1}},
		Totals:  api.ViewTotals{Views: 2, Visitors: 2, MemberViews: 1},
		Files:   []api.ViewFile{{Path: "index.html", Views: 2}},
		Readers: []api.ViewReader{{Handle: "bob", FirstAt: t0, LastAt: t0, Views: 1}},
	}
	if diff := cmp.Diff(wantArt, art); diff != "" {
		t.Errorf("GET /v1/artifacts/demo/views (-want +got):\n%s", diff)
	}

	e.call(http.MethodGet, e.a("/v1/artifacts/quiet/views"), adminToken, nil, http.StatusOK, &art)
	if len(art.Days) != httpapi.DefaultViewDays || art.Totals != (api.ViewTotals{}) || len(art.Files) != 0 || len(art.Readers) != 0 {
		t.Errorf("GET /v1/artifacts/quiet/views = %d days, totals %+v, files %v, readers %v; want 30 empty days",
			len(art.Days), art.Totals, art.Files, art.Readers)
	}

	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/artifacts/missing/views"), token: adminToken}), 404, api.CodeNotFound)
	for _, q := range []string{"?days=0", "?days=367", "?days=x"} {
		e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/views" + q), token: adminToken}), 422, api.CodeInvalidArgument)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/views"), token: e.alice}), 403, api.CodeForbidden)
}

func TestAdminViewsOff(t *testing.T) {
	e := newEnv(t, config{noViews: true})
	e.create(e.alice, "demo")
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/views"), token: adminToken}), 404, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/artifacts/demo/views"), token: adminToken}), 404, api.CodeNotFound)
}
