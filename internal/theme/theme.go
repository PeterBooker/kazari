// Package theme is the look of the pages Kazari renders itself: the
// dashboard, the sign-in and consent pages, the account pages and the gate.
// It holds the "Ma" tokens of the website with the accent fixed at gentian,
// one stylesheet, one small script, the subset fonts and the template pieces
// those pages share. Each host serves the files from its own origin with
// Handler, so the pages run under a policy that allows 'self' and nothing
// else.
//
// testdata/sample.html and testdata/door.html show every component; a page
// uses the classes as they do.
//
// Japanese appears in three places only, always beside English that says the
// same: the faint 飾り behind a front-door page, a label beside each page
// title (概要 Overview, 利用者 People, ページ Pages, 履歴 Activity, 運用
// System, アカウント Account), and the traditional names of the fixed
// colours (藍 松 藤 朽葉 紅). The font holds only the characters found in the
// templates, stylesheets and Go files under internal/: after adding one, run
// site/subset-fonts.sh app.
package theme

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
)

//go:embed css/*.css app.js icon.svg templates.html fonts/*.woff2 fonts/OFL-*.txt
var files embed.FS

// The stylesheet's parts, in order. inlineParts are those a page that cannot
// load the stylesheet inlines instead.
var (
	sheetParts  = []string{"fonts", "tokens", "core", "door", "app"}
	inlineParts = []string{"tokens", "core", "door"}
)

const (
	immutable = "public, max-age=31536000, immutable"
	// A licence keeps its name, so it cannot be cached for ever.
	daily = "public, max-age=86400"
)

type asset struct {
	body         []byte
	contentType  string
	cacheControl string
}

// The embedded files, read once. A missing one is a build fault, so it
// panics at start.
var (
	assets = map[string]asset{}
	// Named by content, so a new build never meets a cached old file.
	cssName, jsName, iconName string
	inline                    template.CSS
	shared                    *template.Template
)

func init() {
	fonts, err := fs.ReadDir(files, "fonts")
	if err != nil {
		panic(err)
	}
	hashed := map[string]string{}
	for _, f := range fonts {
		body := read("fonts/" + f.Name())
		if path.Ext(f.Name()) != ".woff2" {
			assets[f.Name()] = asset{body, "text/plain; charset=utf-8", daily}
			continue
		}
		hashed[f.Name()] = add(f.Name(), body, "font/woff2")
	}

	var sheet bytes.Buffer
	for _, part := range sheetParts {
		sheet.Write(read("css/" + part + ".css"))
	}
	css := sheet.String()
	for name, to := range hashed {
		css = strings.ReplaceAll(css, `url("`+name+`")`, `url("`+to+`")`)
	}
	cssName = add("theme.css", []byte(css), "text/css; charset=utf-8")
	jsName = add("app.js", read("app.js"), "text/javascript; charset=utf-8")
	iconName = add("icon.svg", read("icon.svg"), "image/svg+xml")

	var in bytes.Buffer
	for _, part := range inlineParts {
		in.Write(read("css/" + part + ".css"))
	}
	// Every page of the gate carries the block, so it travels without its
	// comments.
	bare := strings.TrimSpace(cssComment.ReplaceAllString(in.String(), ""))
	inline = template.CSS(bare)

	shared = template.Must(template.New("theme").Parse(string(read("templates.html"))))
	template.Must(shared.New("theme/inline").Parse("<style>" + bare + "</style>"))
}

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/[ \t]*\n?`)

func read(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// add registers body under name with a content hash before the extension and
// returns that name.
func add(name string, body []byte, contentType string) string {
	sum := sha256.Sum256(body)
	ext := path.Ext(name)
	hashed := strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:6]) + ext
	assets[hashed] = asset{body, contentType, immutable}
	return hashed
}

// Assets are the addresses of the stylesheet and the script under one
// prefix. The stylesheet finds the fonts beside it.
type Assets struct {
	CSS, JS string
	// Icon is the tab's icon, the mark as on kazari.io. Without it a
	// browser asks every host for /favicon.ico.
	Icon string
}

// URLs returns the addresses Handler(prefix) serves the stylesheet and the
// script at, such as /assets/theme.3f2a1b9c0d4e.css.
func URLs(prefix string) Assets {
	prefix = slashed(prefix)
	return Assets{CSS: prefix + cssName, JS: prefix + jsName, Icon: prefix + iconName}
}

// Handler serves the stylesheet, the script, the fonts and the fonts'
// licences under prefix, such as "/assets/", and answers 404 for anything
// else. The hashed files are cached for ever: this overrides the no-store
// default of the control hosts.
func Handler(prefix string) http.Handler {
	prefix = slashed(prefix)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, prefix)
		a, found := assets[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		h.Set("Content-Type", a.contentType)
		h.Set("Content-Length", strconv.Itoa(len(a.body)))
		h.Set("Cache-Control", a.cacheControl)
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(a.body)
	})
}

func slashed(prefix string) string {
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

// Inline returns the tokens and the front-door rules as one block of CSS,
// for a page that cannot load the stylesheet: an artifact host serves only
// the artifact's own files. It names no font file, so such a page is set in
// the system fonts. The template "theme/inline" is the same block in a style
// element.
func Inline() template.CSS { return inline }

// Page is what "theme/head" and "theme/door" read.
type Page struct {
	Title  string // "<Title> · Kazari" in the tab
	Assets Assets
}

// Link is one destination of the shell's navigation.
type Link struct {
	Label, Href string
	Current     bool
}

// Form is a POST the shell renders as a button: where it goes and the hidden
// fields it carries, such as a form token.
type Form struct {
	Action string
	Hidden map[string]string
}

// Shell is what "theme/shell" reads: the sidebar around a page.
type Shell struct {
	Page
	Home    string // where the mark links to
	Host    string // under the wordmark, such as art.example.com; may be empty
	Label   string // names the navigation for screen readers, such as "Dashboard"
	Nav     []Link
	Person  string // the signed-in person's name
	Detail  string // under the name, such as "ana · admin"
	SignOut Form   // no button when Action is empty
}

// Templates returns a new template set holding the definitions pages share,
// for a package to parse its own pages into:
//
//	theme/head        the document up to </head>; takes a Page
//	theme/mark        the ring with its gentian segment
//	theme/brand       the mark and the wordmark
//	theme/door        theme/head and theme/door-open; takes a Page
//	theme/door-open   <body> up to the open card of a front-door page
//	theme/door-close  closes it; takes the line under the card, or ""
//	theme/shell       the document up to the open <main> of the app shell; takes a Shell
//	theme/shell-close closes it
//	theme/inline      a style element holding Inline
//	icon/<name>       a 24px line icon in currentColor: alert, check, copy, arrow,
//	                  external, search, key, link, clock, download, plus, bin, lock
func Templates() *template.Template {
	return template.Must(shared.Clone())
}
