package artifact_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

const checkPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " +
	"style-src 'self' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: https:"

func TestCheckPage(t *testing.T) {
	tests := []struct {
		name  string
		page  string
		data  string
		paths []string
		want  []string
	}{
		{"allowed and present", "index.html",
			`<script src="https://cdn.jsdelivr.net/npm/x.js"></script><img src="logo.png"><img src="https://i.test/a.png">` +
				`<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter"><a href="other.html">`,
			[]string{"logo.png"}, nil},
		{"blocked script", "index.html", `<script src="https://cdn.tailwindcss.com"></script><script src="https://cdn.tailwindcss.com/x"></script>`, nil,
			[]string{"index.html loads https://cdn.tailwindcss.com, which the page's Content-Security-Policy (script-src) blocks: " +
				"ship the file in the artifact instead, or add https://cdn.tailwindcss.com to script-src with kazari.csp metadata"}},
		{"blocked frame", "index.html", `<iframe src="//www.youtube.com/embed/x"></iframe>`, nil,
			[]string{"index.html loads https://www.youtube.com/embed/x, which the page's Content-Security-Policy (frame-src) blocks: " +
				"ship the file in the artifact instead, or add https://www.youtube.com to frame-src with kazari.csp metadata"}},
		{"missing files", "docs/index.html", `<img src="/logo.png"><script src="app.js"></script><link rel=icon href="../favicon.ico">`,
			[]string{"docs/app.js"},
			[]string{"docs/index.html refers to logo.png, which is not in this version",
				"docs/index.html refers to favicon.ico, which is not in this version"}},
		{"stylesheet", "css/site.css",
			`@import url("https://fonts.bunny.net/css?family=x"); @font-face{src:url(../fonts/a.woff2)} .x{background:url("data:image/png;base64,AA")}`,
			[]string{"fonts/a.woff2"},
			[]string{"css/site.css loads https://fonts.bunny.net/css?family=x, which the page's Content-Security-Policy (style-src) blocks: " +
				"ship the file in the artifact instead, or add https://fonts.bunny.net to style-src with kazari.csp metadata"}},
		{"inline style", "index.html", `<style>.hero{background:url(hero.jpg)}</style>`, nil,
			[]string{"index.html refers to hero.jpg, which is not in this version"}},
	}
	for _, tt := range tests {
		got := artifact.CheckPage(checkPolicy, true, tt.page, []byte(tt.data), tt.paths...)
		if diff := cmp.Diff(tt.want, got); diff != "" {
			t.Errorf("%s: CheckPage (-want +got):\n%s", tt.name, diff)
		}
	}
	if got := artifact.CheckPage(checkPolicy, false, "index.html", []byte(`<script src="https://x.test/a.js"></script>`)); len(got) != 1 || strings.Contains(got[0], "kazari.csp") {
		t.Errorf("CheckPage without overrides = %q, want one warning that does not suggest kazari.csp", got)
	}
}

func TestPublishWarnings(t *testing.T) {
	e := newEnv(t, artifact.Options{CSP: checkPolicy})
	alice := e.user(t, "alice")
	_, v, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "nested", Files: files("site/index.html", "<title>x</title>", "site/a.css", "")})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"every path starts with site/, so the page's address answers 404: publish the contents of site, not the directory itself"}
	if diff := cmp.Diff(want, v.Warnings); diff != "" {
		t.Errorf("Create of a nested site: warnings (-want +got):\n%s", diff)
	}
	_, v, err = e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "report", Files: files("report.html", "<p>hi</p>")})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"there is no index.html at the root, so the page's address answers 404: name the main page index.html"}
	if diff := cmp.Diff(want, v.Warnings); diff != "" {
		t.Errorf("Create without index.html: warnings (-want +got):\n%s", diff)
	}
	_, v, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "report", Files: files("index.html", `<img src="logo.png">`)})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"index.html refers to logo.png, which is not in this version"}
	if diff := cmp.Diff(want, v.Warnings); diff != "" {
		t.Errorf("Update: warnings (-want +got):\n%s", diff)
	}
}

func TestCreateNamesFromTitle(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	a, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "notes", Files: files("index.html", "<title>\n  Team   notes </title><h1>x</h1>")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Team notes" {
		t.Errorf("Create without a name: name = %q, want the title %q", a.Name, "Team notes")
	}
	a, _, err = e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "named", Name: "Mine", Files: files("index.html", "<title>Other</title>")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Mine" {
		t.Errorf("Create with a name: name = %q, want %q", a.Name, "Mine")
	}
}
