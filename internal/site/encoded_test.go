package site

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
)

// decode reverses the content coding name.
func decode(t *testing.T, name string, b []byte) string {
	t.Helper()
	var r io.Reader
	switch name {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		r = zr
	default:
		t.Fatalf("decode: no decoder for %q", name)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode %s body: %v", name, err)
	}
	return string(out)
}

func comp(h http.Handler) *compressor { return h.(*handler).comp }

func TestEncodingsRoundTrip(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{})
	for _, cd := range codings {
		for path, stored := range map[string]string{"/": bigHTML, "/app.js": bigJS, "/data.json": bigJSON, "/missing.html": big404} {
			t.Run(cd.name+path, func(t *testing.T) {
				rec := do(t, h, http.MethodGet, "compressed.art.test", path, http.Header{"Accept-Encoding": {cd.name}})
				checkHeaders(t, rec, map[string]string{"Content-Encoding": cd.name, "Vary": "Accept-Encoding",
					"Content-Length": strconv.Itoa(rec.Body.Len())})
				if got := decode(t, cd.name, rec.Body.Bytes()); got != stored {
					t.Errorf("GET %s in %s: decoded body of %d bytes, want the stored %d", path, cd.name, len(got), len(stored))
				}
			})
		}
	}
	checkClosed(t, fx)
}

// Range, HEAD and revalidation answer each coding from its own bytes and
// under its own ETag, and a revalidation compresses nothing.
func TestEncodingsRangeHeadAndETag(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := fx.handler(Options{})
	hash := fx.hash("compressed", 1, "app.js")
	for _, cd := range codings {
		t.Run(cd.name, func(t *testing.T) {
			accept := http.Header{"Accept-Encoding": {cd.name}}
			etag := codingETag(hash, cd)

			rec := do(t, h, http.MethodGet, "compressed.art.test", "/app.js", http.Header{"Accept-Encoding": {cd.name}, "If-None-Match": {etag}})
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
				t.Errorf("GET with its ETag: status %d, %d bytes, want 304 and no body", rec.Code, rec.Body.Len())
			}
			checkHeaders(t, rec, map[string]string{"ETag": etag, "Content-Encoding": ""})
			if comp(h).cache.Contains(encodedKey{hash, cd.name}) {
				t.Error("a revalidation compressed the file")
			}

			whole := do(t, h, http.MethodGet, "compressed.art.test", "/app.js", accept).Body.Bytes()
			rec = do(t, h, http.MethodHead, "compressed.art.test", "/app.js", accept)
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Errorf("HEAD: status %d, %d bytes, want 200 and no body", rec.Code, rec.Body.Len())
			}
			checkHeaders(t, rec, map[string]string{"Content-Encoding": cd.name, "ETag": etag, "Content-Length": strconv.Itoa(len(whole))})

			rec = do(t, h, http.MethodGet, "compressed.art.test", "/app.js", http.Header{"Accept-Encoding": {cd.name}, "Range": {"bytes=2-9"}})
			if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), whole[2:10]) {
				t.Errorf("GET bytes=2-9: status %d, body %q, want 206 with %q", rec.Code, rec.Body.Bytes(), whole[2:10])
			}
			checkHeaders(t, rec, map[string]string{"Content-Encoding": cd.name, "ETag": etag, "Content-Length": "8"})

			rec = do(t, h, http.MethodGet, "compressed.art.test", "/app.js", http.Header{"Accept-Encoding": {cd.name},
				"Range": {"bytes=0-3"}, "If-Range": {etag}})
			if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), whole[:4]) {
				t.Errorf("GET with If-Range on its ETag: status %d, body %q, want 206 with %q", rec.Code, rec.Body.Bytes(), whole[:4])
			}
		})
	}
	checkClosed(t, fx)
}

// A spike of requests for one file compresses it once.
func TestCompressedOnce(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	rec := instrumenttest.New(t)
	h := fx.handler(Options{Telemetry: rec.Providers})

	const n = 16
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
			req.Host = "compressed.art.test"
			req.Header.Set("Accept-Encoding", "gzip")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			bodies[i] = w.Body.Bytes()
		})
	}
	wg.Wait()
	for i, b := range bodies {
		if got := decode(t, "gzip", b); got != bigJS {
			t.Errorf("GET /app.js #%d: body of %d bytes, want the stored %d", i+1, len(got), len(bigJS))
		}
	}
	rec.Want("kazari.site.compressions", 1, codingKey.String("gzip"))
	hits, _ := rec.Sum("kazari.site.compression.cache.lookups", cacheResultKey.String("hit"))
	misses, _ := rec.Sum("kazari.site.compression.cache.lookups", cacheResultKey.String("miss"))
	if hits+misses != n || misses < 1 {
		t.Errorf("cache lookups: %v hits, %v misses; want %d in all, at least one a miss", hits, misses, n)
	}
	checkClosed(t, fx)
}

// With every compression slot taken, a request does not wait: it gets the
// stored bytes, counted, unless its coding is cached already.
func TestCompressionBusy(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	rec := instrumenttest.New(t)
	h := fx.handler(Options{Compressions: 1, Telemetry: rec.Providers})
	gz := http.Header{"Accept-Encoding": {"gzip"}}

	do(t, h, http.MethodGet, "compressed.art.test", "/app.js", gz)
	comp(h).slots <- struct{}{}
	defer func() { <-comp(h).slots }()

	w := do(t, h, http.MethodGet, "compressed.art.test", "/", gz)
	if w.Code != http.StatusOK || w.Body.String() != bigHTML {
		t.Errorf("GET / with every slot taken: status %d, %d bytes, want 200 with the stored %d", w.Code, w.Body.Len(), len(bigHTML))
	}
	checkHeaders(t, w, map[string]string{"Content-Encoding": "", "Vary": "Accept-Encoding", "Content-Length": strconv.Itoa(len(bigHTML)),
		"ETag": `"` + fx.hash("compressed", 1, "index.html") + `"`})
	rec.Want("kazari.site.compression.busy", 1)

	w = do(t, h, http.MethodGet, "compressed.art.test", "/app.js", gz)
	if w.Header().Get("Content-Encoding") != "gzip" || decode(t, "gzip", w.Body.Bytes()) != bigJS {
		t.Errorf("GET /app.js, cached, with every slot taken: Content-Encoding %q, want gzip", w.Header().Get("Content-Encoding"))
	}
	w = do(t, h, http.MethodGet, "compressed.art.test", "/missing.html", gz)
	if w.Code != http.StatusNotFound || w.Body.String() != big404 {
		t.Errorf("GET /missing.html with every slot taken: status %d, %d bytes, want 404 with the stored %d", w.Code, w.Body.Len(), len(big404))
	}
	rec.Want("kazari.site.compression.busy", 2)
	rec.Want("kazari.site.compressions", 1)
	checkClosed(t, fx)
}

// The cache keeps within its budget by pushing out the least recent entry.
func TestCompressionCacheBudget(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	size := func(s string) int64 {
		b, err := encodeGzip([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return int64(len(b)) + entryOverhead
	}
	budget := size(bigJS) + size(bigCSS) + size(bigHTML)/2
	rec := instrumenttest.New(t)
	h := fx.handler(Options{CompressionCache: budget, Telemetry: rec.Providers})
	gz := http.Header{"Accept-Encoding": {"gzip"}}
	for _, p := range []string{"/app.js", "/style.css", "/app.js", "/"} {
		do(t, h, http.MethodGet, "compressed.art.test", p, gz)
	}
	held := map[string]bool{}
	for _, name := range []string{"app.js", "style.css", "index.html"} {
		held[name] = comp(h).cache.Contains(encodedKey{fx.hash("compressed", 1, name), "gzip"})
	}
	if !held["app.js"] || held["style.css"] || !held["index.html"] {
		t.Errorf("held after app.js, style.css, app.js, index.html = %v, want style.css, the least recent, pushed out", held)
	}
	if _, n := comp(h).cache.Size(); n > budget {
		t.Errorf("cache holds %d bytes, over its budget of %d", n, budget)
	}
	rec.Want("kazari.site.compression.cache.evictions", 1)
	if got, _ := rec.Sum("kazari.site.compression.cache.usage"); int64(got) != size(bigJS)+size(bigHTML) {
		t.Errorf("kazari.site.compression.cache.usage = %v, want %d", got, size(bigJS)+size(bigHTML))
	}
}

// compression_cache: 0 sends every file as stored.
func TestCompressionOff(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	h := NewHandler(fx.res, fx.blobs, Options{BaseDomain: testBase, Logger: slog.New(slog.NewTextHandler(fx.logs, nil))})
	for _, p := range []string{"/", "/missing.html"} {
		w := do(t, h, http.MethodGet, "compressed.art.test", p, http.Header{"Accept-Encoding": {"gzip, br, zstd"}})
		checkHeaders(t, w, map[string]string{"Content-Encoding": "", "Vary": ""})
		if p == "/" && w.Body.String() != bigHTML {
			t.Errorf("GET %s with compression off: %d bytes, want the stored %d", p, w.Body.Len(), len(bigHTML))
		}
	}
	checkClosed(t, fx)
}
