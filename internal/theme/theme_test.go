package theme

import (
	"bytes"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// csp is the policy the pages that load the stylesheet are served under.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func sampleShell(prefix string) Shell {
	return Shell{
		Title: "Overview", Assets: URLs(prefix),
		Home:  "/",
		Host:  "art.example.com",
		Label: "Dashboard",
		Nav: []Link{
			{Label: "Overview", Href: "/", Current: true},
			{Label: "People", Href: "/people"},
			{Label: "Pages", Href: "/pages"},
			{Label: "Activity", Href: "/activity"},
			{Label: "System", Href: "/system"},
		},
		Person:  "Ana Sousa",
		Detail:  "ana · admin",
		SignOut: Form{Action: "/session/signout", Hidden: map[string]string{"form_token": "t0ken"}},
	}
}

// sampleServer serves the two sample pages as a host would: the assets
// under prefix, behind the no-store default of the control hosts.
func sampleServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	tmpl := template.Must(Templates().ParseGlob("testdata/*.html"))
	page := func(name string, data any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
				t.Errorf("render %s: %v", name, err)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", csp)
			_, _ = buf.WriteTo(w)
		}
	}
	mux := http.NewServeMux()
	mux.Handle(prefix, Handler(prefix))
	mux.Handle("GET /{$}", page("sample.html", sampleShell(prefix)))
	mux.Handle("GET /door", page("door.html", Page{Title: "Sign in", Assets: URLs(prefix)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, method, url string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(body)
}

var (
	classAttr    = regexp.MustCompile(`\sclass="([^"]*)"`)
	assetRef     = regexp.MustCompile(`<(?:link|script|img)\b[^>]*?\s(?:href|src)="([^"]*)"`)
	inlineScript = regexp.MustCompile(`(?s)<script\b[^>]*>(.*?)</script>`)
	cssURL       = regexp.MustCompile(`url\(\s*["']?([^"')]+)["']?\s*\)`)
	literal      = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|oklch|oklab)\(`)
)

func TestSamples(t *testing.T) {
	for _, prefix := range []string{"/assets/", "/oauth/assets/", "/account/assets/"} {
		t.Run(prefix, func(t *testing.T) {
			srv := sampleServer(t, prefix)
			_, sheet := fetch(t, http.MethodGet, srv.URL+URLs(prefix).CSS)
			for _, path := range []string{"/", "/door"} {
				res, html := fetch(t, http.MethodGet, srv.URL+path)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("%s: status %d", path, res.StatusCode)
				}
				if strings.Contains(html, "style=") || strings.Contains(html, "<style") {
					t.Errorf("%s: an inline style, which the policy refuses", path)
				}
				for _, m := range inlineScript.FindAllStringSubmatch(html, -1) {
					if strings.TrimSpace(m[1]) != "" {
						t.Errorf("%s: an inline script, which the policy refuses", path)
					}
				}
				refs := assetRef.FindAllStringSubmatch(html, -1)
				if len(refs) != 3 {
					t.Errorf("%s: %d asset references, want the icon, the stylesheet and the script", path, len(refs))
				}
				for _, m := range refs {
					if !strings.HasPrefix(m[1], prefix) {
						t.Errorf("%s: asset %q is not under %s", path, m[1], prefix)
						continue
					}
					asset, _ := fetch(t, http.MethodGet, srv.URL+m[1])
					if asset.StatusCode != http.StatusOK || asset.Header.Get("Cache-Control") != immutable ||
						asset.Header.Get("X-Content-Type-Options") != "nosniff" {
						t.Errorf("%s: asset %s: status %d, headers %v", path, m[1], asset.StatusCode, asset.Header)
					}
				}
				for _, m := range classAttr.FindAllStringSubmatch(html, -1) {
					for class := range strings.FieldsSeq(m[1]) {
						if !regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `(?:[^\w-]|$)`).MatchString(sheet) {
							t.Errorf("%s: class %q is not in the stylesheet", path, class)
						}
					}
				}
			}
		})
	}
}

func TestStylesheet(t *testing.T) {
	srv := sampleServer(t, "/assets/")
	res, sheet := fetch(t, http.MethodGet, srv.URL+URLs("/assets/").CSS)
	if got := res.Header.Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	urls := cssURL.FindAllStringSubmatch(sheet, -1)
	if len(urls) != 3 {
		t.Errorf("%d url() references, want the three fonts", len(urls))
	}
	for _, m := range urls {
		if strings.Contains(m[1], ":") || strings.Contains(m[1], "/") {
			t.Errorf("url(%s) leaves the stylesheet's directory", m[1])
			continue
		}
		font, _ := fetch(t, http.MethodGet, srv.URL+"/assets/"+m[1])
		if font.StatusCode != http.StatusOK || font.Header.Get("Content-Type") != "font/woff2" ||
			font.Header.Get("Cache-Control") != immutable {
			t.Errorf("font %s: status %d, headers %v", m[1], font.StatusCode, font.Header)
		}
	}
	for _, want := range []string{"--season: light-dark(#5b57a3, #a5a4f7)", "--season-mark: light-dark(#5b57a3, #a5a4f7)",
		"prefers-reduced-motion", ":focus-visible"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("stylesheet lacks %q", want)
		}
	}
}

// A component never names a colour: the tokens do, once, for both themes.
func TestNoLiteralColours(t *testing.T) {
	for _, part := range sheetParts {
		if part == "tokens" {
			continue
		}
		css := string(read("css/" + part + ".css"))
		if m := literal.FindString(css); m != "" {
			t.Errorf("css/%s.css: literal colour %q; use a token", part, m)
		}
	}
}

func TestHandler(t *testing.T) {
	srv := sampleServer(t, "/assets/")
	js := URLs("assets").JS
	if !strings.HasPrefix(js, "/assets/app.") {
		t.Fatalf("URLs(%q).JS = %q", "assets", js)
	}
	tests := []struct {
		method, path string
		status       int
		contentType  string
		cache        string
	}{
		{http.MethodGet, js, http.StatusOK, "text/javascript; charset=utf-8", immutable},
		{http.MethodHead, js, http.StatusOK, "text/javascript; charset=utf-8", immutable},
		{http.MethodGet, "/assets/OFL-manrope.txt", http.StatusOK, "text/plain; charset=utf-8", daily},
		{http.MethodPost, js, http.StatusMethodNotAllowed, "", "no-store"},
		{http.MethodGet, "/assets/", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/app.js", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/theme.css", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/manrope.woff2", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/japanese.txt", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/../theme.go", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/css/app.css", http.StatusNotFound, "", "no-store"},
	}
	for _, tt := range tests {
		res, body := fetch(t, tt.method, srv.URL+tt.path)
		if res.StatusCode != tt.status {
			t.Errorf("%s %s: status %d, want %d", tt.method, tt.path, res.StatusCode, tt.status)
			continue
		}
		if got := res.Header.Get("Cache-Control"); got != tt.cache {
			t.Errorf("%s %s: Cache-Control %q, want %q", tt.method, tt.path, got, tt.cache)
		}
		if tt.contentType != "" && res.Header.Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s: Content-Type %q, want %q", tt.method, tt.path, res.Header.Get("Content-Type"), tt.contentType)
		}
		if tt.method == http.MethodHead && body != "" {
			t.Errorf("HEAD %s: a body of %d bytes", tt.path, len(body))
		}
	}
	if a, b := URLs("/assets/"), URLs("/oauth/assets/"); strings.TrimPrefix(a.CSS, "/assets/") != strings.TrimPrefix(b.CSS, "/oauth/assets/") {
		t.Errorf("the stylesheet's name differs by prefix: %s, %s", a.CSS, b.CSS)
	}
}

// The gate's page inlines the block, so it must stand alone: no file to
// fetch, and nothing html/template would escape.
func TestInline(t *testing.T) {
	css := string(Inline())
	for _, want := range []string{"--bg: light-dark(#edefea, #191c1a)", ".door-card", ".btn-primary", ".title"} {
		if !strings.Contains(css, want) {
			t.Errorf("Inline() lacks %q", want)
		}
	}
	for _, bad := range []string{"url(", "@font-face", "@import", "</", "{{", "/*"} {
		if strings.Contains(css, bad) {
			t.Errorf("Inline() contains %q", bad)
		}
	}
	page := template.Must(Templates().New("page").Parse(
		`<head>{{template "theme/inline"}}</head>{{template "theme/door-open"}}<p>{{.}}</p>{{template "theme/door-close" ""}}`))
	var buf bytes.Buffer
	if err := page.Execute(&buf, "<b>"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "<style>"+css+"</style>") {
		t.Error("theme/inline does not hold Inline() as it is")
	}
	if !strings.Contains(out, "<p>&lt;b&gt;</p>") || strings.Contains(out, `class="door-foot`) {
		t.Errorf("the page around it: %s", out[strings.Index(out, "<body"):])
	}
}

func TestTemplatesAreCopies(t *testing.T) {
	a, b := Templates(), Templates()
	template.Must(a.New("mine").Parse("x"))
	if b.Lookup("mine") != nil {
		t.Error("a definition added to one set appears in another")
	}
	for _, name := range []string{"theme/head", "theme/mark", "theme/brand", "theme/door", "theme/door-open", "theme/door-close",
		"theme/shell", "theme/shell-close", "theme/inline", "icon/alert", "icon/check", "icon/copy", "icon/arrow", "icon/external",
		"icon/search", "icon/key", "icon/link", "icon/clock", "icon/download", "icon/plus", "icon/bin", "icon/lock"} {
		if b.Lookup(name) == nil {
			t.Errorf("no template %q", name)
		}
	}
}

func TestShellEscapes(t *testing.T) {
	sh := sampleShell("/assets/")
	sh.Person, sh.Host = `<script>alert(1)</script>`, `"><img src=x>`
	sh.Nav = []Link{{Label: "<b>", Href: "javascript:alert(1)"}}
	page := template.Must(Templates().New("page").Parse(`{{template "theme/shell" .}}{{template "theme/shell-close"}}`))
	var buf bytes.Buffer
	if err := page.Execute(&buf, sh); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, bad := range []string{"<script>alert", "<img src=x>", `href="javascript:`, "<b>"} {
		if strings.Contains(out, bad) {
			t.Errorf("the shell renders %q unescaped", bad)
		}
	}
	if !strings.Contains(out, `name="form_token" value="t0ken"`) || !strings.Contains(out, `action="/session/signout"`) {
		t.Error("the sign-out form lacks its action or its hidden field")
	}
}

// The font holds only the Japanese that site/subset-fonts.sh found; a label
// added since would silently fall back to a system font.
func TestJapaneseIsInTheFont(t *testing.T) {
	have, err := os.ReadFile("fonts/japanese.txt")
	if err != nil {
		t.Fatal(err)
	}
	missing := map[rune]string{}
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".html", ".css", ".js":
		case ".go":
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
		default:
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, r := range string(body) {
			if (r >= 0x3000 && r <= 0x30FF || r >= 0x4E00 && r <= 0x9FFF) && !bytes.ContainsRune(have, r) {
				missing[r] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for r, path := range missing {
		t.Errorf("%q in %s is not in the font: run site/subset-fonts.sh app", r, path)
	}
}
