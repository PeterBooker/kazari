package site

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// fakeGate records its calls; allow decides Allow.
type fakeGate struct {
	mu       sync.Mutex
	allow    bool
	allowed  []string // host of each Allow call
	reserved []string // method host path of each ServeReserved call
}

func (g *fakeGate) Allow(w http.ResponseWriter, _ *http.Request, p *artifact.Published, host string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowed = append(g.allowed, host)
	if !p.Visibility.Gated() {
		return true
	}
	if !g.allow {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("gate says no to " + p.Slug))
	}
	return g.allow
}

func (g *fakeGate) ServeReserved(w http.ResponseWriter, r *http.Request, p *artifact.Published, host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserved = append(g.reserved, r.Method+" "+host+" "+r.URL.Path)
	_, _ = w.Write([]byte("reserved for " + p.Slug))
}

// addMembers adds a members artifact "team" with two versions.
func (fx *fixture) addMembers(t *testing.T) {
	t.Helper()
	fx.add(t, "team", 30, 1, nil, []testFile{{"index.html", "text/html", "team v1"}})
	fx.add(t, "team", 31, 2, nil, []testFile{{"index.html", "text/html", "team v2"}, {"404.html", "text/html", "team 404"}})
	for _, p := range fx.res.versions["team"] {
		p.Visibility = artifact.Members
	}
}

// Without a Gate a gated artifact is never served, on any of its hosts.
func TestGatedWithoutGate(t *testing.T) {
	fx := newFixture(t)
	fx.addMembers(t)
	h := fx.handler(Options{})
	for _, host := range []string{"team.art.test", "team--v1.art.test", "team--v2.art.test"} {
		rec := do(t, h, http.MethodGet, host, "/", nil)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "team v") {
			t.Errorf("%s: status %d, body %q", host, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, h, http.MethodGet, "demo.art.test", "/.kazari/share", nil); rec.Code != http.StatusNotFound {
		t.Errorf("reserved path without a gate: status %d", rec.Code)
	}
}

func TestGatedAsksTheGate(t *testing.T) {
	fx := newFixture(t)
	fx.addMembers(t)
	g := &fakeGate{}
	h := fx.handler(Options{Gate: g})

	for _, tc := range []struct{ host, path string }{
		{"team.art.test", "/"},
		{"team--v1.art.test", "/"},
		{"team.art.test", "/missing"},
		{"team.art.test", "/.kazari"},
	} {
		rec := do(t, h, http.MethodGet, tc.host, tc.path, nil)
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != "gate says no to team" {
			t.Errorf("%s%s denied: status %d, body %q", tc.host, tc.path, rec.Code, rec.Body.String())
		}
	}
	if want := []string{"team", "team--v1", "team", "team"}; !slices.Equal(g.allowed, want) {
		t.Fatalf("Allow hosts = %v, want %v", g.allowed, want)
	}
	// Public artifacts pass the gate too, which counts them and lets them through.
	g.allowed = nil
	if rec := do(t, h, http.MethodGet, "demo.art.test", "/", nil); rec.Code != http.StatusOK || !slices.Equal(g.allowed, []string{"demo"}) {
		t.Fatalf("public artifact: status %d, Allow calls %v", rec.Code, g.allowed)
	}

	g.allow = true
	for _, tc := range []struct{ host, path, body, cache string }{
		{"team.art.test", "/", "team v2", "private, no-cache"},
		{"team--v1.art.test", "/", "team v1", "private, no-cache"},
		{"team.art.test", "/nope", "team 404", "private, no-cache"},
	} {
		rec := do(t, h, http.MethodGet, tc.host, tc.path, nil)
		if rec.Body.String() != tc.body {
			t.Errorf("%s%s: body %q, want %q", tc.host, tc.path, rec.Body.String(), tc.body)
		}
		checkHeaders(t, rec, map[string]string{"Cache-Control": tc.cache, "X-Robots-Tag": "noindex",
			"Cross-Origin-Resource-Policy": "same-origin"})
	}
	// Public pages stay embeddable from anywhere.
	rec := do(t, h, http.MethodGet, "demo.art.test", "/app.js", nil)
	checkHeaders(t, rec, map[string]string{"Cross-Origin-Resource-Policy": ""})
}

// A cross-site opener can see whether a window's browsing context changed,
// so a members host must answer with the same opener policy whatever the
// gate decides and wherever the request ends. The sign-in page has one, so
// a response without it would tell an opener that the visitor has access.
func TestGatedOpenerPolicy(t *testing.T) {
	const coop = "Cross-Origin-Opener-Policy"
	fx := newFixture(t)
	fx.addMembers(t)
	g := &fakeGate{}
	h := fx.handler(Options{Gate: g})

	for _, tc := range []struct {
		name  string
		allow bool
		host  string
		path  string
		code  int
	}{
		{"gate refuses", false, "team.art.test", "/", http.StatusUnauthorized},
		{"gate refuses a version host", false, "team--v1.art.test", "/", http.StatusUnauthorized},
		{"reserved path", false, "team.art.test", "/.kazari/signout", http.StatusOK},
		{"page", true, "team.art.test", "/", http.StatusOK},
		{"version host page", true, "team--v1.art.test", "/", http.StatusOK},
		{"artifact 404.html", true, "team.art.test", "/nope", http.StatusNotFound},
		{"instance 404 page", true, "team--v1.art.test", "/nope", http.StatusNotFound},
		{"path no file can have", true, "team--v1.art.test", "/a%5Cb", http.StatusNotFound},
	} {
		g.allow = tc.allow
		rec := do(t, h, http.MethodGet, tc.host, tc.path, nil)
		if rec.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.code)
		}
		if got := rec.Header().Get(coop); got != "same-origin" {
			t.Errorf("%s: %s = %q, want same-origin", tc.name, coop, got)
		}
	}

	// A page that opens another page may need the link between them, so
	// public artifacts, and hosts that do not exist, stay without one.
	for _, tc := range []struct{ host, path string }{
		{"demo.art.test", "/"},
		{"demo.art.test", "/nope"},
		{"demo.art.test", "/.kazari/signout"},
		{"nosuch.art.test", "/"},
	} {
		rec := do(t, h, http.MethodGet, tc.host, tc.path, nil)
		checkHeaders(t, rec, map[string]string{coop: ""})
	}
}

func TestReservedPaths(t *testing.T) {
	fx := newFixture(t)
	fx.addMembers(t)
	g := &fakeGate{}
	h := fx.handler(Options{Gate: g})
	for _, tc := range []struct{ method, host, path string }{
		{http.MethodGet, "team.art.test", "/.kazari/callback"},
		{http.MethodGet, "team--v2.art.test", "/.kazari/share"},
		{http.MethodPost, "demo.art.test", "/.kazari/signout"},
	} {
		rec := do(t, h, tc.method, tc.host, tc.path, nil)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "reserved for ") {
			t.Errorf("%s %s%s: status %d, body %q", tc.method, tc.host, tc.path, rec.Code, rec.Body.String())
		}
	}
	want := []string{"GET team /.kazari/callback", "GET team--v2 /.kazari/share", "POST demo /.kazari/signout"}
	if !slices.Equal(g.reserved, want) {
		t.Fatalf("ServeReserved calls = %v, want %v", g.reserved, want)
	}
	if len(g.allowed) != 0 {
		t.Fatalf("reserved paths asked Allow: %v", g.allowed)
	}
	// Unknown and offline artifacts have no reserved paths, and other
	// paths still take only GET and HEAD.
	for _, tc := range []struct {
		method, host, path string
		status             int
	}{
		{http.MethodGet, "nosuch.art.test", "/.kazari/callback", http.StatusNotFound},
		{http.MethodGet, "unpub.art.test", "/.kazari/share", http.StatusNotFound},
		{http.MethodPost, "demo.art.test", "/", http.StatusMethodNotAllowed},
	} {
		if rec := do(t, h, tc.method, tc.host, tc.path, nil); rec.Code != tc.status {
			t.Errorf("%s %s%s: status %d, want %d", tc.method, tc.host, tc.path, rec.Code, tc.status)
		}
	}
}

// A browser holding a copy of a gated version must ask again, and its
// conditional request is checked like any other: a sign-out or a revoked
// share link reaches pages it has already seen.
func TestGatedVersionRevalidates(t *testing.T) {
	fx := newFixture(t)
	fx.addMembers(t)
	g := &fakeGate{allow: true}
	h := fx.handler(Options{Gate: g})
	etag := `"` + fx.hash("team", 1, "index.html") + `"`
	rec := do(t, h, http.MethodGet, "team--v1.art.test", "/", http.Header{"If-None-Match": {etag}})
	if rec.Code != http.StatusNotModified || rec.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("allowed: status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	g.allow = false
	if rec := do(t, h, http.MethodGet, "team--v1.art.test", "/", http.Header{"If-None-Match": {etag}}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("denied: status %d, want the gate's answer", rec.Code)
	}
}
