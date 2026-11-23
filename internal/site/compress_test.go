package site

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/views"
)

var (
	bigHTML = "<!doctype html><title>Report</title>" + strings.Repeat("<p>Quarterly figures for the region.</p>\n", 60)
	bigCSS  = strings.Repeat("body { margin: 0; padding: 0; font-family: sans-serif; }\n", 40)
	bigJS   = strings.Repeat("console.log('a line of a fairly ordinary script');\n", 40)
	bigSVG  = `<svg xmlns="http://www.w3.org/2000/svg">` + strings.Repeat(`<rect width="10" height="10"/>`, 60) + `</svg>`
	bigJSON = "[" + strings.Repeat(`{"region":"north","total":12},`, 60) + "{}]"
	bigPNG  = "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\x01\x02\x03", 400)
	big404  = "<!doctype html><title>Not here</title>" + strings.Repeat("<p>Nothing at this address.</p>\n", 60)
)

// addCompressed adds "compressed" (version 1, id 40) with files on either
// side of every rule, and "compressed-team", a members copy.
func (fx *fixture) addCompressed(t *testing.T) {
	t.Helper()
	files := []testFile{
		{"index.html", "text/html", bigHTML},
		{"style.css", "text/css", bigCSS},
		{"app.js", "text/javascript; charset=utf-8", bigJS},
		{"logo.svg", "image/svg+xml", bigSVG},
		{"data.json", "application/json", bigJSON},
		{"photo.png", "image/png", bigPNG},
		{"font.woff2", "font/woff2", bigPNG},
		{"small.html", "text/html", "<p>small</p>"},
		{"404.html", "text/html", big404},
	}
	fx.add(t, "compressed", 40, 1, nil, files)
	fx.add(t, "compressed-team", 41, 1, nil, files)
	fx.res.versions["compressed-team"][1].Visibility = artifact.Members
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return string(out)
}

func TestCompressedFiles(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{DefaultCSP: testCSP, Gate: &fakeGate{allow: true}})
	gz := http.Header{"Accept-Encoding": {"gzip, deflate, br"}}

	tests := []struct {
		name   string
		host   string
		path   string
		header http.Header
		body   string
		gzip   bool
		vary   bool
		ctype  string
		cache  string
	}{
		{name: "html", path: "/", header: gz, body: bigHTML, gzip: true, vary: true, ctype: "text/html; charset=utf-8"},
		{name: "css", path: "/style.css", header: gz, body: bigCSS, gzip: true, vary: true, ctype: "text/css; charset=utf-8"},
		{name: "javascript", path: "/app.js", header: gz, body: bigJS, gzip: true, vary: true, ctype: "text/javascript; charset=utf-8"},
		{name: "svg", path: "/logo.svg", header: gz, body: bigSVG, gzip: true, vary: true, ctype: "image/svg+xml"},
		{name: "json", path: "/data.json", header: gz, body: bigJSON, gzip: true, vary: true, ctype: "application/json; charset=utf-8"},
		{name: "no accept-encoding", path: "/", body: bigHTML, vary: true, ctype: "text/html; charset=utf-8"},
		{name: "gzip refused", path: "/", header: http.Header{"Accept-Encoding": {"gzip;q=0, br"}}, body: bigHTML, vary: true},
		{name: "gzip refused over star", path: "/", header: http.Header{"Accept-Encoding": {"*, gzip;q=0"}}, body: bigHTML, vary: true},
		{name: "star", path: "/", header: http.Header{"Accept-Encoding": {"*"}}, body: bigHTML, gzip: true, vary: true},
		{name: "star refused", path: "/", header: http.Header{"Accept-Encoding": {"*;q=0"}}, body: bigHTML, vary: true},
		{name: "x-gzip", path: "/", header: http.Header{"Accept-Encoding": {"x-gzip"}}, body: bigHTML, gzip: true, vary: true},
		{name: "upper case with weight", path: "/", header: http.Header{"Accept-Encoding": {"GZIP;Q=0.5"}}, body: bigHTML, gzip: true, vary: true},
		{name: "second field line", path: "/", header: http.Header{"Accept-Encoding": {"br", "gzip"}}, body: bigHTML, gzip: true, vary: true},
		{name: "other codings only", path: "/", header: http.Header{"Accept-Encoding": {"br, zstd"}}, body: bigHTML, vary: true},
		{name: "small file", path: "/small.html", header: gz, body: "<p>small</p>"},
		{name: "png", path: "/photo.png", header: gz, body: bigPNG, ctype: "image/png"},
		{name: "woff2", path: "/font.woff2", header: gz, body: bigPNG, ctype: "font/woff2"},
		{name: "if-match", path: "/", header: http.Header{"Accept-Encoding": {"gzip"}, "If-Match": {"*"}}, body: bigHTML, vary: true},
		{name: "pinned", host: "compressed--v1.art.test", path: "/", header: gz, body: bigHTML, gzip: true, vary: true,
			cache: "public, max-age=31536000, immutable"},
		{name: "gated", host: "compressed-team.art.test", path: "/", header: gz, body: bigHTML, gzip: true, vary: true,
			cache: "private, no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := tt.host
			if host == "" {
				host = "compressed.art.test"
			}
			rec := do(t, h, http.MethodGet, host, tt.path, tt.header)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s%s: status %d, want 200", host, tt.path, rec.Code)
			}
			slug, _, _ := strings.Cut(strings.TrimSuffix(host, ".art.test"), "--")
			file := strings.TrimPrefix(tt.path, "/")
			if file == "" {
				file = "index.html"
			}
			hash := fx.hash(slug, 1, file)
			want := map[string]string{"Content-Encoding": "", "ETag": `"` + hash + `"`, "Vary": "",
				"X-Content-Type-Options": "nosniff", "Content-Security-Policy": testCSP}
			body := rec.Body.String()
			if tt.gzip {
				want["Content-Encoding"] = "gzip"
				want["ETag"] = `"` + hash + `-gzip"`
				want["Content-Length"] = ""
				body = gunzip(t, rec.Body.Bytes())
			}
			if tt.vary {
				want["Vary"] = "Accept-Encoding"
			}
			if tt.ctype != "" {
				want["Content-Type"] = tt.ctype
			}
			if tt.cache != "" {
				want["Cache-Control"] = tt.cache
			}
			checkHeaders(t, rec, want)
			if body != tt.body {
				t.Errorf("GET %s%s with %v: body of %d bytes, want the stored %d", host, tt.path, tt.header, len(body), len(tt.body))
			}
		})
	}
	checkClosed(t, fx)
}

func TestCompressedConditionalRequests(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{DefaultCSP: testCSP})
	hash := fx.hash("compressed", 1, "index.html")
	identity, gzipped := `"`+hash+`"`, `"`+hash+`-gzip"`

	tests := []struct {
		name     string
		header   http.Header
		status   int
		encoding string
		etag     string
	}{
		{"gzip etag", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {gzipped}}, http.StatusNotModified, "", gzipped},
		{"weak gzip etag", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {"W/" + gzipped}}, http.StatusNotModified, "", gzipped},
		{"gzip etag in a list", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {`"other", ` + gzipped}}, http.StatusNotModified, "", gzipped},
		{"gzip etag on a second line", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {`"other"`, gzipped}}, http.StatusNotModified, "", gzipped},
		{"star", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {"*"}}, http.StatusNotModified, "", gzipped},
		// A client holding the stored bytes that now asks for gzip is sent
		// the representation it asked for.
		{"identity etag asking for gzip", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {identity}}, http.StatusOK, "gzip", gzipped},
		{"gzip etag without gzip", http.Header{"If-None-Match": {gzipped}}, http.StatusOK, "", identity},
		{"identity etag without gzip", http.Header{"If-None-Match": {identity}}, http.StatusNotModified, "", identity},
		{"etag with a quote inside a list", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {`"a,` + hash + `-gzip"`}}, http.StatusOK, "gzip", gzipped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "compressed.art.test", "/", tt.header)
			if rec.Code != tt.status {
				t.Fatalf("GET with %v: status %d, want %d", tt.header, rec.Code, tt.status)
			}
			checkHeaders(t, rec, map[string]string{"Content-Encoding": tt.encoding, "ETag": tt.etag,
				"Vary": "Accept-Encoding", "Cache-Control": "no-cache"})
			if tt.status == http.StatusNotModified && rec.Body.Len() != 0 {
				t.Errorf("GET with %v: 304 with a body of %d bytes", tt.header, rec.Body.Len())
			}
		})
	}
	checkClosed(t, fx)
}

func TestCompressedRangeIsIdentity(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{})
	hash := fx.hash("compressed", 1, "index.html")

	for _, header := range []http.Header{
		{"Accept-Encoding": {"gzip"}, "Range": {"bytes=0-14"}},
		{"Accept-Encoding": {"gzip"}, "Range": {"bytes=0-14"}, "If-Range": {`"` + hash + `"`}},
	} {
		rec := do(t, h, http.MethodGet, "compressed.art.test", "/", header)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("GET with %v: status %d, want 206", header, rec.Code)
		}
		checkHeaders(t, rec, map[string]string{"Content-Encoding": "", "ETag": `"` + hash + `"`, "Vary": "Accept-Encoding"})
		if got, want := rec.Body.String(), bigHTML[:15]; got != want {
			t.Errorf("GET with %v: body %q, want %q", header, got, want)
		}
	}
	// If-Range with the gzip ETag no longer names what a range is cut from,
	// so the whole file comes back.
	rec := do(t, h, http.MethodGet, "compressed.art.test", "/",
		http.Header{"Accept-Encoding": {"gzip"}, "Range": {"bytes=0-14"}, "If-Range": {`"` + hash + `-gzip"`}})
	if rec.Code != http.StatusOK || rec.Body.String() != bigHTML {
		t.Errorf("GET with a gzip If-Range: status %d, %d bytes, want 200 with the stored %d", rec.Code, rec.Body.Len(), len(bigHTML))
	}
	checkClosed(t, fx)
}

func TestCompressedHead(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{})
	rec := do(t, h, http.MethodHead, "compressed.art.test", "/", http.Header{"Accept-Encoding": {"gzip"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: status %d, want 200", rec.Code)
	}
	checkHeaders(t, rec, map[string]string{"Content-Encoding": "gzip", "Content-Length": "", "Vary": "Accept-Encoding",
		"Content-Type": "text/html; charset=utf-8"})
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD: body of %d bytes, want none", rec.Body.Len())
	}
	checkClosed(t, fx)
}

func TestCompressedNotFoundPage(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{})

	rec := do(t, h, http.MethodGet, "compressed.art.test", "/missing.html", http.Header{"Accept-Encoding": {"gzip"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /missing.html: status %d, want 404", rec.Code)
	}
	checkHeaders(t, rec, map[string]string{"Content-Encoding": "gzip", "Content-Length": "", "Vary": "Accept-Encoding",
		"X-Robots-Tag": "noindex"})
	if got := gunzip(t, rec.Body.Bytes()); got != big404 {
		t.Errorf("GET /missing.html: body of %d bytes, want the stored 404.html of %d", len(got), len(big404))
	}

	rec = do(t, h, http.MethodGet, "compressed.art.test", "/missing.html", nil)
	if rec.Code != http.StatusNotFound || rec.Body.String() != big404 {
		t.Errorf("GET /missing.html without gzip: status %d, %d bytes, want 404 with the stored %d", rec.Code, rec.Body.Len(), len(big404))
	}
	checkHeaders(t, rec, map[string]string{"Content-Encoding": "", "Vary": "Accept-Encoding"})
	checkClosed(t, fx)
}

// An instance page is never compressed, whatever the client accepts.
func TestErrorPagesUncompressed(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{})
	rec := do(t, h, http.MethodGet, "nothing-here.art.test", "/", http.Header{"Accept-Encoding": {"gzip"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET an unknown slug: status %d, want 404", rec.Code)
	}
	checkHeaders(t, rec, map[string]string{"Content-Encoding": "", "Vary": ""})
}

func TestCompressedCountsViews(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	v := &fakeViews{}
	h := fx.handler(Options{Views: v})
	gzipped := `"` + fx.hash("compressed", 1, "index.html") + `-gzip"`
	do(t, h, http.MethodGet, "compressed.art.test", "/", http.Header{"Accept-Encoding": {"gzip"}})
	do(t, h, http.MethodGet, "compressed.art.test", "/", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {gzipped}})
	id := int64(len("compressed"))
	got := v.take()
	for i := range got {
		got[i].IP, got[i].UA = "", ""
	}
	want := []views.Hit{
		{ArtifactID: id, Path: "index.html", Access: views.Access{Via: views.ViaPublic}},
		{ArtifactID: id, Path: "index.html", Access: views.Access{Via: views.ViaPublic}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("views of a compressed page (-want +got):\n%s", diff)
	}
}

// A compressed body is the same bytes for the same file, whichever pooled
// writer sends it.
func TestCompressedStable(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{})
	var first []byte
	for i := range 3 {
		req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
		req.Host = "compressed.art.test"
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if i == 0 {
			first = rec.Body.Bytes()
			continue
		}
		if !bytes.Equal(rec.Body.Bytes(), first) {
			t.Errorf("GET /app.js #%d: compressed body differs from the first", i+1)
		}
	}
}
