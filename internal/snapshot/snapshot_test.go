package snapshot_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/snapshot"
)

const policy = "default-src 'self'; script-src 'self' https://cdnjs.cloudflare.com; style-src 'self' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; img-src 'self' data: https:"

type site map[string]struct{ ct, body string }

func serve(t *testing.T, s site) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := s[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", f.ct)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func taker(t *testing.T, opts snapshot.Options) *snapshot.Taker {
	t.Helper()
	opts.CSP = policy
	return snapshot.New(fetch.New(fetch.Options{Transport: http.DefaultTransport, Dir: t.TempDir()}), opts)
}

func files(t *testing.T, snap *artifact.Snapshot) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range snap.Files {
		data, err := io.ReadAll(f.Content)
		if err != nil {
			t.Fatal(err)
		}
		out[f.Path] = string(data)
	}
	return out
}

func TestTakeLocalisesAndRewrites(t *testing.T) {
	srv := serve(t, site{
		"/": {"text/html; charset=utf-8", `<!doctype html><html><head><meta charset="iso-8859-1"><title> My  Page </title>` +
			`<meta name="description" content="About it"><base href="/sub/">` +
			`<link rel="stylesheet" href="../s.css" integrity="sha256-x" crossorigin="anonymous">` +
			`<link rel="stylesheet" href="https://fonts.googleapis.com/css?family=A">` +
			`<link rel="canonical" href="/"><script src="https://cdnjs.cloudflare.com/x.js"></script></head>` +
			`<body onload="go()"><header><h1>Hello</h1><nav><a href="../about">About</a></nav></header>` +
			`<p>` + strings.Repeat("word ", 50) + `</p>` +
			`<main><img src="../a.png" srcset="../a.png 1x, ../b.png 2x"><p style="background:url(../bg.png)">x</p>` +
			`<form action="post"><input></form><script>track()</script><iframe src="../f"></iframe></main>` +
			`<footer><picture><source srcset="../missing.png"><img src="data:image/gif;base64,R0lG"></picture></footer></body></html>`},
		"/s.css":    {"text/css", `@import "more.css"; body{background:url('bg.png')} @font-face{src:url(https://fonts.gstatic.com/f.woff2)}`},
		"/more.css": {"text/css", `h1{color:red}`},
		"/a.png":    {"image/png", "PNG-A"},
		"/b.png":    {"image/png", "PNG-B"},
		"/bg.png":   {"image/png", "PNG-BG"},
	})
	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL+"/", false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	got := files(t, snap)
	page := got["index.html"]

	if snap.Title != " My  Page " || snap.Description != "About it" {
		t.Errorf("title %q description %q", snap.Title, snap.Description)
	}
	for _, want := range []string{
		`<meta charset="utf-8"/>`, `href="assets/`, `-s.css"`, `href="https://fonts.googleapis.com/css?family=A"`,
		`href="` + srv.URL + `/about"`, `action="` + srv.URL + `/sub/post"`, `src="` + srv.URL + `/f"`,
		`srcset="assets/`, `-a.png 1x, assets/`, `-b.png 2x"`, `background:url(&#34;assets/`, `src="data:image/gif;base64,R0lG"`,
		`srcset="` + srv.URL + `/missing.png"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
	for _, bad := range []string{"<script", "<base", "canonical", "integrity", "crossorigin", "onload", "iso-8859-1", "x.js"} {
		if strings.Contains(page, bad) {
			t.Errorf("page still has %q:\n%s", bad, page)
		}
	}
	var css string
	for p, c := range got {
		if strings.HasSuffix(p, "-s.css") {
			css = c
		}
	}
	if !strings.Contains(css, `@import "`) || strings.Contains(css, `assets/`) || !strings.Contains(css, `-more.css"`) ||
		!strings.Contains(css, `url("`) || !strings.Contains(css, `-bg.png")`) || !strings.Contains(css, `url("https://fonts.gstatic.com/f.woff2")`) {
		t.Errorf("css = %q", css)
	}
	r := snap.Report
	if r.Source != srv.URL+"/" || r.Fetched != 5 || r.ScriptsRemoved != 2 || len(r.Skipped) != 1 ||
		!strings.Contains(r.Skipped[0].Reason, "404") || len(r.Notes) != 0 {
		t.Errorf("report = %+v", r)
	}
	if len(r.Outline) < 4 || r.Outline[0].Tag != "<header>" || r.Outline[0].Heading != "Hello" || r.Outline[1].Tag != "<h1>" {
		t.Errorf("outline = %+v", r.Outline)
	}
	if len(snap.Files) != 6 || snap.Files[0].Path != "index.html" || snap.Files[0].ContentType != "text/html; charset=utf-8" {
		t.Errorf("files = %+v", snap.Files)
	}
}

func TestTakeKeepsScripts(t *testing.T) {
	srv := serve(t, site{
		"/":       {"text/html", `<html><head><script src="app.js"></script></head><body onclick="x()"><div id="app"></div><script>boot()</script></body></html>`},
		"/app.js": {"text/javascript", `boot=()=>1`},
	})
	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	page := files(t, snap)["index.html"]
	if !strings.Contains(page, `<script src="assets/`) || !strings.Contains(page, `onclick="x()"`) || !strings.Contains(page, `boot()`) || snap.Report.ScriptsRemoved != 0 {
		t.Errorf("page = %s", page)
	}

	snap, err = taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if len(snap.Report.Notes) != 1 || !strings.Contains(snap.Report.Notes[0], "JavaScript") {
		t.Errorf("shell note = %v", snap.Report.Notes)
	}
}

func TestTakeLimits(t *testing.T) {
	srv := serve(t, site{
		"/":        {"text/html", `<html><body><img src="big.png"><img src="a.png"><img src="b.png"></body></html>`},
		"/big.png": {"image/png", strings.Repeat("x", 500)},
		"/a.png":   {"image/png", "aaaa"},
		"/b.png":   {"image/png", "bbbb"},
	})
	snap, err := taker(t, snapshot.Options{MaxFileBytes: 200, MaxTotalBytes: 1000, MaxFiles: 2}).Take(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	r := snap.Report
	if r.Fetched != 1 || len(r.Skipped) != 2 || !strings.Contains(r.Skipped[0].Reason, "larger than 200") || r.Skipped[1].Reason != "file limit reached" {
		t.Errorf("report = %+v", r)
	}
}

func TestTakeRefusesNonPages(t *testing.T) {
	srv := serve(t, site{"/x.png": {"image/png", "PNG"}})
	_, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL+"/x.png", false)
	if !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "not an HTML page") {
		t.Errorf("png: %v", err)
	}
	_, err = taker(t, snapshot.Options{}).Take(context.Background(), srv.URL+"/missing", false)
	if !errors.Is(err, artifact.ErrFetchFailed) {
		t.Errorf("missing: %v", err)
	}
}

func TestFormatNeverCreatesWhitespaceBetweenInlineBoxes(t *testing.T) {
	src := `<!doctype html><html><head><title>t</title><style>a{}</style></head><body><div><div>x</div><div>y</div></div>` +
		`<p><span>a</span><span>b</span></p><ul>` + "\n" + `<li>1</li><li>2</li></ul><pre>  keep
 this </pre></body></html>`
	srv := serve(t, site{"/": {"text/html", src}})
	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	page := files(t, snap)["index.html"]
	for _, want := range []string{"<div><div>x</div><div>y</div></div>", "<p><span>a</span><span>b</span></p>", "<ul>\n      <li>1</li><li>2</li></ul>\n    <pre>  keep\n this </pre>",
		"\n  <head>\n    <meta charset=\"utf-8\"/>\n    <title>t</title>\n    <style>a{}</style>\n  </head>"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
	if strings.Count(page, "\n") < 6 {
		t.Errorf("page not formatted:\n%s", page)
	}
}

func TestTakeStylesheetCycleAndRedirectBase(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><link rel="stylesheet" href="/a.css"><link rel="stylesheet" href="/theme.css"></head><body><p>`+strings.Repeat("w ", 50)+`</p></body></html>`)
	})
	mux.HandleFunc("/a.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, `@import "a.css"; @import url(b.css); p{}`)
	})
	mux.HandleFunc("/b.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, `@import "a.css"; b{}`)
	})
	mux.HandleFunc("/theme.css", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v2/theme.css", http.StatusFound)
	})
	mux.HandleFunc("/v2/theme.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, `h1{background:url(fonts/bg.png)}`)
	})
	hits := map[string]int{}
	mux.HandleFunc("/v2/fonts/bg.png", func(w http.ResponseWriter, _ *http.Request) {
		hits["bg"]++
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, "PNG")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL+"/", false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	got := files(t, snap)
	paths := map[string]int{}
	for p := range got {
		paths[p]++
	}
	if len(got) != 5 || snap.Report.Fetched != 4 || len(snap.Report.Skipped) != 0 {
		t.Errorf("files %v report %+v", paths, snap.Report)
	}
	var a, b string
	for p, c := range got {
		switch {
		case strings.HasSuffix(p, "-a.css"):
			a = c
		case strings.HasSuffix(p, "-b.css"):
			b = c
		}
	}
	if !strings.Contains(a, `@import "`) || strings.Contains(a, "http") || !strings.Contains(b, "-a.css\"") {
		t.Errorf("cycle: a=%q b=%q", a, b)
	}
	if hits["bg"] != 1 {
		t.Errorf("redirected stylesheet resolved its background %d times", hits["bg"])
	}
}

func TestTakeRemovesScriptURLs(t *testing.T) {
	src := `<html><body><a href="javascript:alert(1)">x</a><a href="/ok">y</a><iframe srcdoc="<script>1</script>"></iframe><form action=" JavaScript:go()"></form></body></html>`
	srv := serve(t, site{"/": {"text/html", src}})
	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	page := files(t, snap)["index.html"]
	if strings.Contains(strings.ToLower(page), "javascript:") || strings.Contains(page, "srcdoc") || !strings.Contains(page, `href="`+srv.URL+`/ok"`) {
		t.Errorf("page = %s", page)
	}
	snap, err = taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if page := files(t, snap)["index.html"]; !strings.Contains(page, "javascript:alert(1)") || !strings.Contains(page, "srcdoc") {
		t.Errorf("kept scripts: %s", page)
	}
}

func TestTakeBoundsTheReportAndRetriesNothing(t *testing.T) {
	var imgs strings.Builder
	for i := range 150 {
		fmt.Fprintf(&imgs, `<img src="/missing-%d.png">`, i)
	}
	imgs.WriteString(strings.Repeat(`<img src="/dead.png">`, 5))
	misses := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body>`+imgs.String()+`</body></html>`)
			return
		}
		if r.URL.Path == "/dead.png" {
			misses++
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	r := snap.Report
	if len(r.Skipped) != 100 || misses != 1 || len(r.Notes) != 1 || r.Notes[0] != "51 more resources were left remote" {
		t.Errorf("skipped %d, dead fetched %d times, notes %v", len(r.Skipped), misses, r.Notes)
	}
}

func TestTakeRefusesOversizedPages(t *testing.T) {
	srv := serve(t, site{"/": {"text/html", "<html><body><p>" + strings.Repeat("x", 4<<20) + "</p></body></html>"}})
	_, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, false)
	if !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized page: %v", err)
	}
}

// cancelAfter ends a context as the body at path is read to its end, so the
// fetch succeeds and what follows it runs with the context already over.
type cancelAfter struct {
	path   string
	cancel context.CancelFunc
}

func (c *cancelAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && r.URL.Path == c.path {
		resp.Body = cancelAtEOF{resp.Body, c.cancel}
	}
	return resp, err
}

type cancelAtEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelAtEOF) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.cancel()
	}
	return n, err
}

func TestTakeStopsParsingWhenTheContextEnds(t *testing.T) {
	var page strings.Builder
	page.WriteString("<!doctype html>")
	for i := range 2000 {
		fmt.Fprintf(&page, "<html a%d>", i)
	}
	srv := serve(t, site{
		"/page":  {"text/html", page.String()},
		"/small": {"text/html", "<!doctype html><title>Small</title><p>Hello"},
	})
	rt := &cancelAfter{path: "/page"}
	tk := snapshot.New(fetch.New(fetch.Options{Transport: rt, Dir: t.TempDir()}), snapshot.Options{})
	// One more import than there are slots, so a slot that stays taken fails.
	for range 5 {
		var ctx context.Context
		ctx, rt.cancel = context.WithCancel(context.Background())
		snap, err := tk.Take(ctx, srv.URL+"/page", false)
		if err == nil {
			snap.Close()
			t.Fatal("Take parsed the whole page after its context ended")
		}
		if !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "import budget exhausted") {
			t.Fatalf("Take: %v", err)
		}
	}
	snap, err := tk.Take(context.Background(), srv.URL+"/small", false)
	if err != nil {
		t.Fatalf("small page after stopped imports: %v", err)
	}
	snap.Close()
}

// A landmark the outline cannot find in the rendering costs a scan of the
// rest of the page, so it counts toward the limit like one it finds.
func TestTakeStopsOutliningAfterTheLimit(t *testing.T) {
	srv := serve(t, site{"/": {"text/html", "<!doctype html><title>x</title><body><svg>" +
		strings.Repeat("<section xlink:href=x />", 61) + "</svg><section id=after></section>"}})
	snap, err := taker(t, snapshot.Options{}).Take(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if o := snap.Report.Outline; len(o) != 0 {
		t.Errorf("outline searched on past the limit: %+v", o)
	}
}

// The budget running out while fetching assets leaves them remote; the page
// itself is still imported, with its outline.
func TestTakeKeepsThePageWhenTheContextEndsOnAnAsset(t *testing.T) {
	srv := serve(t, site{
		"/":      {"text/html", `<!doctype html><title>x</title><body><section><img src="/a.png"><img src="/b.png"></section>`},
		"/a.png": {"image/png", "a"},
		"/b.png": {"image/png", "b"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk := snapshot.New(fetch.New(fetch.Options{Transport: &cancelAfter{"/a.png", cancel}, Dir: t.TempDir()}), snapshot.Options{})
	snap, err := tk.Take(ctx, srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	r := snap.Report
	if len(r.Skipped) != 1 || r.Skipped[0].Reason != "import budget exhausted" || len(r.Outline) != 1 {
		t.Errorf("skipped %+v, outline %+v", r.Skipped, r.Outline)
	}
}

// A page deep in nesting with whitespace between block siblings renders at
// many times its size, and everything after the walk follows the rendering,
// so it stops at the file limit. One more import than there are slots.
func TestTakeRefusesRenderingPastTheFileLimit(t *testing.T) {
	srv := serve(t, site{
		"/":      {"text/html", "<!doctype html><title>x</title><body>" + strings.Repeat("<div>\n", 400) + strings.Repeat("<hr>\n", 2000)},
		"/small": {"text/html", "<!doctype html><title>x</title><body><section></section>"},
	})
	tk := taker(t, snapshot.Options{MaxFileBytes: 64 << 10})
	for range 5 {
		snap, err := tk.Take(context.Background(), srv.URL, false)
		if err == nil {
			snap.Close()
			t.Fatal("Take rendered the whole page past the file limit")
		}
		if !errors.Is(err, artifact.ErrTooLarge) {
			t.Fatalf("Take: %v", err)
		}
	}
	snap, err := tk.Take(context.Background(), srv.URL+"/small", false)
	if err != nil {
		t.Fatalf("small page after refused imports: %v", err)
	}
	defer snap.Close()
	if len(snap.Report.Outline) != 1 {
		t.Errorf("outline: %+v", snap.Report.Outline)
	}
}
