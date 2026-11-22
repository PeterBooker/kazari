package site

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/views"
)

// fakeViews keeps every hit it is given.
type fakeViews struct {
	mu   sync.Mutex
	hits []views.Hit
}

func (v *fakeViews) Record(_ *http.Request, h views.Hit) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.hits = append(v.hits, h)
}

func (v *fakeViews) take() []views.Hit {
	v.mu.Lock()
	defer v.mu.Unlock()
	h := v.hits
	v.hits = nil
	return h
}

// The handler offers every HTML file it serves, and nothing else; the
// recorder decides which offers are views.
func TestViewsOffered(t *testing.T) {
	fx := newFixture(t)
	fx.addMembers(t)
	v := &fakeViews{}
	h := fx.handler(Options{Views: v, Gate: &fakeGate{allow: true}})
	header := http.Header{"User-Agent": {"Firefox"}, "X-Forwarded-For": {"198.51.100.7"}}
	public := views.Access{Via: views.ViaPublic}
	demo, spa, team := int64(len("demo")), int64(len("spa")), int64(len("team"))

	tests := []struct {
		name, host, path string
		want             []views.Hit
	}{
		{"index", "demo.art.test", "/", []views.Hit{{ArtifactID: demo, Path: "index.html", Access: public}}},
		{"page", "demo.art.test", "/docs/page.html", []views.Hit{{ArtifactID: demo, Path: "docs/page.html", Access: public}}},
		{"pinned version", "demo--v1.art.test", "/", []views.Hit{{ArtifactID: demo, Path: "index.html", Access: public}}},
		{"spa route", "spa.art.test", "/reports/q3", []views.Hit{{ArtifactID: spa, Path: "index.html", Access: public}}},
		{"members page", "team.art.test", "/", []views.Hit{{ArtifactID: team, Path: "index.html", Access: views.Access{Via: views.ViaMember, Handle: "alice"}}}},
		{"script", "demo.art.test", "/app.js", nil},
		{"directory redirect", "demo.art.test", "/docs", nil},
		{"own 404 page", "custom.art.test", "/missing", nil},
		{"missing artifact", "nope.art.test", "/", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			do(t, h, http.MethodGet, tt.host, tt.path, header)
			got := v.take()
			for i := range got {
				if got[i].UA != "Firefox" || got[i].IP == "" {
					t.Errorf("GET %s%s: hit %d has UA %q and IP %q, want the request's", tt.host, tt.path, i, got[i].UA, got[i].IP)
				}
				got[i].UA, got[i].IP = "", ""
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GET %s%s offered (-want +got):\n%s", tt.host, tt.path, diff)
			}
		})
	}
}

func TestHTML(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html":                 true,
		"text/html; charset=utf-8":  true,
		"TEXT/HTML ; charset=utf-8": true,
		"text/plain":                false,
		"application/xhtml+xml":     false,
		"":                          false,
	} {
		if got := html(ct); got != want {
			t.Errorf("html(%q) = %v, want %v", ct, got, want)
		}
	}
}
