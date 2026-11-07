package snapshot_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// growth is a byte budget that the fixtures of the tests below reach with a
// hundred references, while the pages themselves stay well inside it.
const growth = 64 << 10

func TestTakeRefusesPagesThatGrowPastTheBudget(t *testing.T) {
	long := "/" + strings.Repeat("a", 1<<10) + "/"
	base := `<base href="` + long + `">`
	links := func(el string) string { return base + strings.Repeat(el, 100) }
	var srcset []string
	for range 100 {
		srcset = append(srcset, "x 1x")
	}
	for _, tc := range []struct {
		name string
		page string
		css  string
	}{
		{name: "a", page: links(`<a href=x></a>`)},
		{name: "area", page: links(`<area href=x>`)},
		{name: "form", page: links(`<form action=x></form>`)},
		{name: "iframe", page: links(`<iframe src=x></iframe>`)},
		{name: "object", page: links(`<object data=x></object>`)},
		{name: "embed", page: links(`<embed src=x>`)},
		{name: "img", page: links(`<img src=x>`)},
		{name: "srcset", page: base + `<img srcset="` + strings.Join(srcset, ", ") + `">`},
		{name: "style attribute", page: links(`<p style="background:url(x)"></p>`)},
		{name: "style element", page: base + "<style>" + strings.Repeat("p{background:url(x)}", 100) + "</style>"},
		{name: "stylesheet", page: `<link rel="stylesheet" href="` + long + `s.css">`, css: strings.Repeat("p{background:url(x)}", 100)},
		{name: "indentation", page: strings.Repeat("<div>", 400) + strings.Repeat("<div></div> ", 100)},
		{name: "unusable base", page: `<base href="http://user@` + long[1:] + `">` + strings.Repeat(`<a href=x></a>`, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, site{
				"/":            {"text/html", "<!doctype html><html><body>" + tc.page + "</body></html>"},
				long + "s.css": {"text/css", tc.css},
			})
			snap, err := taker(t, snapshot.Options{MaxFileBytes: growth}).Take(context.Background(), srv.URL, false)
			if err == nil {
				defer snap.Close()
				t.Fatalf("Take succeeded with a %d-byte page", len(files(t, snap)["index.html"]))
			}
			if !errors.Is(err, artifact.ErrTooLarge) {
				t.Errorf("err = %v, want too_large", err)
			}
		})
	}
}

func TestTakeKeepsPagesWithinTheBudget(t *testing.T) {
	long := "/" + strings.Repeat("a", 1<<10) + "/"
	base := `<base href="` + long + `">`
	for _, tc := range []struct {
		name string
		page string
		want string
	}{
		{"relative links under the budget", base + strings.Repeat(`<a href=x></a>`, 50), long + "x"},
		{"links the base does not reach", base + strings.Repeat(`<a href="https://example.com/x"></a><a href="#x"></a><a href="mailto:x@example.com"></a>`, 100), "https://example.com/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, site{"/": {"text/html", "<html><body>" + tc.page + "</body></html>"}})
			snap, err := taker(t, snapshot.Options{MaxFileBytes: growth}).Take(context.Background(), srv.URL, false)
			if err != nil {
				t.Fatal(err)
			}
			defer snap.Close()
			if page := files(t, snap)["index.html"]; !strings.Contains(page, tc.want) {
				t.Errorf("page lacks %q", tc.want)
			}
		})
	}
}

// TestTakeBoundsAllocation sizes its page so that rewriting it in full comes
// to 64 MiB, a safe distance from what the same shape reaches at the page cap.
func TestTakeBoundsAllocation(t *testing.T) {
	page := `<html><body><base href="/` + strings.Repeat("a", 64<<10) + `/">` + strings.Repeat(`<a href=x></a>`, 1000) + "</body></html>"
	srv := serve(t, site{"/": {"text/html", page}})
	tk := taker(t, snapshot.Options{})

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	snap, err := tk.Take(context.Background(), srv.URL, false)
	runtime.ReadMemStats(&after)

	if err == nil {
		defer snap.Close()
	}
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Errorf("err = %v, want too_large", err)
	}
	const limit = 48 << 20
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		t.Errorf("allocated %d bytes, want at most %d", got, limit)
	}
}
