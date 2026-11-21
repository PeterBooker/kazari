package site

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
)

var workerHeader = http.Header{"Service-Worker": {"script"}}

const workerBody = "self.onfetch = spy"

// addWorker adds a public artifact "pwa" with a worker script, and a pinned
// host for it.
func (fx *fixture) addWorker(t *testing.T) {
	t.Helper()
	fx.add(t, "pwa", 40, 1, nil, []testFile{
		{"index.html", "text/html", "pwa index"},
		{"sw.js", "text/javascript; charset=utf-8", workerBody},
		{"notes.txt", "text/plain", "plain notes"},
	})
}

// wantEviction requires the script that removes a worker. clear says whether
// the origin's storage goes with it.
func wantEviction(t *testing.T, rec *httptest.ResponseRecorder, clear bool, what string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d, want 200; body %q", what, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "registration.unregister()") || strings.Contains(rec.Body.String(), workerBody) {
		t.Fatalf("%s: body %q is not the eviction script", what, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("%s: Content-Type %q: a browser refuses anything but a script", what, ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s: Cache-Control %q", what, cc)
	}
	want := ""
	if clear {
		want = `"storage"`
	}
	if got := rec.Header().Get("Clear-Site-Data"); got != want {
		t.Errorf("%s: Clear-Site-Data %q, want %q", what, got, want)
	}
}

// A browser keeps a worker whose update check fails, so every answer other
// than a script must be replaced by one that removes it: what a miss would
// otherwise become is checked here, each ahead of its normal answer.
func TestWorkerScriptMissEvicts(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{ReservedSlugs: []string{"www"}})
	for _, tc := range []struct {
		name, host, path string
		clear            bool
	}{
		{"no such artifact", "nothing.art.test", "/sw.js", false},
		{"reserved slug", "www.art.test", "/sw.js", false},
		{"no such version", "demo--v9.art.test", "/sw.js", false},
		{"purged", "purged.art.test", "/sw.js", true},
		{"purged pinned host", "purged--v1.art.test", "/sw.js", true},
		{"unpublished", "unpub.art.test", "/sw.js", false},
		{"soft-deleted", "deleted.art.test", "/sw.js", false},
		{"never published", "empty.art.test", "/sw.js", false},
		{"missing file", "demo.art.test", "/sw.js", false},
		{"missing file on a pinned host", "demo--v1.art.test", "/app.js", false},
		{"ahead of the directory redirect", "demo.art.test", "/docs", false},
		{"ahead of the SPA fallback", "spa-bare.art.test", "/sw", false},
		{"ahead of 404.html", "custom.art.test", "/sw.js", false},
		{"not a script: html", "demo.art.test", "/index.html", false},
		{"not a script: the root", "demo.art.test", "/", false},
	} {
		wantEviction(t, do(t, h, http.MethodGet, tc.host, tc.path, workerHeader), tc.clear, tc.name)
	}
}

func TestWorkerScriptFollowsControl(t *testing.T) {
	fx := newFixture(t)
	fx.addWorker(t)
	h := fx.handler(Options{})

	rec := do(t, h, http.MethodGet, "pwa.art.test", "/sw.js", workerHeader)
	if rec.Code != http.StatusOK || rec.Body.String() != workerBody {
		t.Fatalf("a controlled worker: status %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Clear-Site-Data") != "" {
		t.Error("a controlled worker clears the origin's storage")
	}

	fx.res.lost = map[string]bool{"sw.js": true}
	for _, host := range []string{"pwa.art.test", "pwa--v1.art.test"} {
		wantEviction(t, do(t, h, http.MethodGet, host, "/sw.js", workerHeader), false, "control lost on "+host)
		// The same file stays an ordinary file for everything else.
		if rec := do(t, h, http.MethodGet, host, "/sw.js", nil); rec.Body.String() != workerBody {
			t.Errorf("%s: plain request: status %d, body %q", host, rec.Code, rec.Body.String())
		}
	}
}

// A failed check says nothing about control: the browser keeps the worker
// and tries again.
func TestWorkerScriptCheckFails(t *testing.T) {
	fx := newFixture(t)
	fx.addWorker(t)
	fx.res.workerErr = errors.New("database is down")
	rec := do(t, fx.handler(Options{}), http.MethodGet, "pwa.art.test", "/sw.js", workerHeader)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
}

// A gated artifact's check runs ahead of the gate's answer: a 401 would leave
// the worker of an earlier controller in the browser of a visitor about to
// sign in. The answer is the same for every path, so it does not tell an
// outsider which files exist.
func TestWorkerScriptGated(t *testing.T) {
	fx := newFixture(t)
	fx.addWorker(t)
	fx.res.latest["pwa"].Visibility = artifact.Members
	fx.res.versions["pwa"][1].Visibility = artifact.Members

	g := &fakeGate{}
	h := fx.handler(Options{Gate: g})
	for _, target := range []string{"/sw.js", "/missing.js"} {
		wantEviction(t, do(t, h, http.MethodGet, "pwa.art.test", target, workerHeader), false, "no access to "+target)
	}
	if len(g.allowed) != 0 {
		t.Errorf("Allow called for %v", g.allowed)
	}
	if rec := do(t, h, http.MethodGet, "pwa.art.test", "/sw.js", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("plain request without access: status %d", rec.Code)
	}

	g.allow = true
	rec := do(t, h, http.MethodGet, "pwa.art.test", "/sw.js", workerHeader)
	if rec.Code != http.StatusOK || rec.Body.String() != workerBody {
		t.Fatalf("with access: status %d, body %q", rec.Code, rec.Body.String())
	}
	fx.res.lost = map[string]bool{"sw.js": true}
	wantEviction(t, do(t, h, http.MethodGet, "pwa.art.test", "/sw.js", workerHeader), false, "with access, control lost")

	g.gateErr = errors.New("database is down")
	if rec := do(t, h, http.MethodGet, "pwa.art.test", "/sw.js", workerHeader); rec.Code != http.StatusInternalServerError {
		t.Errorf("access check fails: status %d", rec.Code)
	}

	// The reserved paths stay the gate's.
	g.gateErr = nil
	if rec := do(t, h, http.MethodGet, "pwa.art.test", "/.kazari/sw.js", workerHeader); rec.Body.String() != "reserved for pwa" {
		t.Errorf("reserved path: status %d, body %q", rec.Code, rec.Body.String())
	}
}
