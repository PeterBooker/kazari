package dashboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/user"
	"github.com/PeterBooker/kazari/internal/views"
)

// view counts n page views of slug's index.html, each by another visitor.
func (e *env) view(slug string, a views.Access, n int) {
	e.t.Helper()
	p, err := e.artifacts.ResolvePublished(context.Background(), slug)
	if err != nil {
		e.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+slug+".art.test/", nil)
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("User-Agent", "Firefox")
	for i := range n {
		e.views.Record(r, views.Hit{ArtifactID: p.ArtifactID, Path: "index.html", Access: a, IP: "192.0.2." + strconv.Itoa(i), UA: "Firefox"})
	}
}

func (e *env) flushViews() {
	e.t.Helper()
	if err := e.views.Flush(context.Background()); err != nil {
		e.t.Fatalf("Flush: %v", err)
	}
}

func wantAll(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s lacks %q", what, w)
		}
	}
}

func TestViewsOnTheDashboard(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.publish("ana", "report")
	e.publish("ana", "team")
	if _, _, err := e.artifacts.Update(context.Background(), artifact.Actor{Handle: "ana"},
		artifact.UpdateRequest{Slug: "team", Visibility: new(artifact.Members)}); err != nil {
		t.Fatal(err)
	}
	e.view("report", views.Access{Via: views.ViaPublic}, 2)
	e.clock.Advance(8 * 24 * time.Hour)
	e.view("report", views.Access{Via: views.ViaPublic}, 3)
	e.view("team", views.Access{Via: views.ViaMember, Handle: "ana"}, 1)
	e.flushViews()
	b := e.signedIn("boss")

	r := b.get("/")
	strictMarkup(t, "the overview", r.body)
	wantAll(t, "the overview", r.body,
		`aria-labelledby="views"`, "4 views, up 100% on the week before", "6 views in all",
		`<svg class="chart-plot" viewBox="0 0 360 80" role="img"`, `<path class="chart-bar"`,
		"The numbers by day", `href="/pages/report">report</a></td><td class="num" data-label="Views">5</td>`)

	r = b.get("/pages")
	strictMarkup(t, "the pages list", r.body)
	wantAll(t, "the pages list", r.body, "Views, 30 days", `data-label="Views, 30 days">5</td>`, `data-label="Views, 30 days">1</td>`)

	r = b.get("/pages/team")
	strictMarkup(t, "a page", r.body)
	wantAll(t, "a members page", r.body, "1 view and 1 daily visitor in the last 30 days: 1 by members",
		"Members who opened it", `href="/people/ana">ana</a>`, "Most viewed files", "index.html")

	r = b.get("/pages/report")
	wantAll(t, "a public page", r.body, "Only members-only pages record who opened them")
}

func TestViewsQuietAndOff(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.publish("boss", "report")
	b := e.signedIn("boss")
	wantAll(t, "a quiet overview", b.get("/").body, "No page has been viewed in the last 30 days")
	wantAll(t, "a quiet page", b.get("/pages/report").body, "Nobody has opened this page in the last 30 days")

	off := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Views = nil })
	off.person("boss", user.RoleAdmin)
	off.publish("boss", "report")
	ob := off.signedIn("boss")
	for _, path := range []string{"/", "/pages/report"} {
		r := ob.get(path)
		strictMarkup(t, path+" with analytics off", r.body)
		wantAll(t, path+" with analytics off", r.body, "Analytics is off on this instance")
	}
	if r := ob.get("/pages"); strings.Contains(r.body, "Views, 30 days") {
		t.Error("the pages list has a views column with analytics off")
	}
}
