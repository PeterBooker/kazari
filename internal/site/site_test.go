package site

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kazari/internal/artifact"
	"kazari/internal/blob"
)

const (
	testBase = "art.test"
	testCSP  = "default-src 'self'; frame-ancestors 'none'"
)

// memBlobs is an in-memory blob.Store that counts open readers so tests
// can check every blob is closed.
type memBlobs struct {
	mu    sync.Mutex
	data  map[string][]byte
	open  int
	opens int
}

func newMemBlobs() *memBlobs { return &memBlobs{data: map[string][]byte{}} }

func (m *memBlobs) Put(_ context.Context, r io.Reader) (string, int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	m.mu.Lock()
	m.data[hash] = b
	m.mu.Unlock()
	return hash, int64(len(b)), nil
}

func (m *memBlobs) Open(_ context.Context, hash string) (io.ReadSeekCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[hash]
	if !ok {
		return nil, blob.ErrNotFound
	}
	m.open++
	m.opens++
	return &memReader{Reader: bytes.NewReader(b), m: m}, nil
}

func (m *memBlobs) Stat(_ context.Context, hash string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[hash]
	if !ok {
		return 0, blob.ErrNotFound
	}
	return int64(len(b)), nil
}

func (m *memBlobs) Delete(_ context.Context, hash string) error {
	m.mu.Lock()
	delete(m.data, hash)
	m.mu.Unlock()
	return nil
}

func (m *memBlobs) openCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open
}

type memReader struct {
	*bytes.Reader
	m *memBlobs
}

func (r *memReader) Close() error {
	r.m.mu.Lock()
	r.m.open--
	r.m.mu.Unlock()
	return nil
}

// fakeResolver serves fixed Published records and file maps.
type fakeResolver struct {
	latest   map[string]*artifact.Published
	versions map[string]map[int]*artifact.Published
	files    map[int64]map[string]artifact.File
	err      error // returned by every Resolve call when set
}

func (f *fakeResolver) ResolvePublished(_ context.Context, slug string) (*artifact.Published, error) {
	if f.err != nil {
		return nil, f.err
	}
	p, ok := f.latest[slug]
	if !ok {
		return nil, artifact.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakeResolver) ResolveVersion(_ context.Context, slug string, n int) (*artifact.Published, error) {
	if f.err != nil {
		return nil, f.err
	}
	if _, ok := f.latest[slug]; !ok {
		return nil, artifact.ErrNotFound
	}
	p, ok := f.versions[slug][n]
	if !ok {
		return nil, artifact.Errorf(artifact.ErrVersionNotFound, "%s has no version %d", slug, n)
	}
	cp := *p
	return &cp, nil
}

func (f *fakeResolver) Files(_ context.Context, versionID int64) (map[string]artifact.File, error) {
	m, ok := f.files[versionID]
	if !ok {
		return nil, errors.New("no such version id")
	}
	return m, nil
}

type testFile struct {
	path, contentType, body string
}

type fixture struct {
	res   *fakeResolver
	blobs *memBlobs
	logs  *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{
		res: &fakeResolver{
			latest:   map[string]*artifact.Published{},
			versions: map[string]map[int]*artifact.Published{},
			files:    map[int64]map[string]artifact.File{},
		},
		blobs: newMemBlobs(),
		logs:  &bytes.Buffer{},
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	fx.add(t, "demo", 1, 1, nil, []testFile{{"index.html", "text/html", "demo v1"}})
	fx.add(t, "demo", 2, 2, nil, []testFile{
		{"index.html", "text/html", "demo v2 index"},
		{"app.js", "application/javascript", "console.log(1)"},
		{"style.css", "text/css; charset=iso-8859-1", "body{}"},
		{"data.json", "application/json", `{"a":1}`},
		{"logo.png", "image/png", "\x89PNG"},
		{"blob.bin", "", "raw"},
		{"docs/index.html", "text/html", "docs index"},
		{"docs/page.html", "text/html", "docs page"},
		{"big.txt", "text/plain", "0123456789"},
		{"a..b.txt", "text/plain", "dots"},
	})
	fx.add(t, "custom", 3, 1, nil, []testFile{
		{"index.html", "text/html", "custom index"},
		{"404.html", "text/html", "custom not found"},
	})
	fx.add(t, "spa", 4, 1, map[string]string{artifact.MetaSPA: "true"}, []testFile{
		{"index.html", "text/html", "spa shell"},
		{"assets/app.js", "application/javascript", "spa()"},
		{"404.html", "text/html", "spa not found"},
	})
	fx.add(t, "spa-bare", 5, 1, map[string]string{artifact.MetaSPA: "true"}, []testFile{
		{"index.html", "text/html", "bare shell"},
	})
	fx.add(t, "csp", 6, 1, map[string]string{artifact.MetaCSP: "default-src *"}, []testFile{
		{"index.html", "text/html", "csp index"},
		{"404.html", "text/html", "csp not found"},
	})
	fx.add(t, "indexable", 7, 1, map[string]string{artifact.MetaIndexable: "true"}, []testFile{
		{"index.html", "text/html", "indexable"},
	})
	fx.add(t, "broken", 8, 1, nil, nil)
	fx.res.files[8] = map[string]artifact.File{
		"index.html": {Path: "index.html", Hash: strings.Repeat("0", 64), Size: 3, ContentType: "text/html"},
	}
	fx.add(t, "nofiles", 9, 1, nil, nil)
	delete(fx.res.files, 9) // Files fails for this version

	fx.res.latest["purged"] = &artifact.Published{Slug: "purged", PurgedAt: &now}
	fx.res.versions["purged"] = map[int]*artifact.Published{1: {Slug: "purged", PurgedAt: &now}}
	fx.res.latest["unpub"] = &artifact.Published{Slug: "unpub", VersionID: 20, VersionNumber: 1, UnpublishedAt: &now}
	fx.res.latest["deleted"] = &artifact.Published{Slug: "deleted", VersionID: 21, VersionNumber: 1, DeletedAt: &now}
	fx.res.latest["empty"] = &artifact.Published{Slug: "empty"}
	fx.res.files[20] = map[string]artifact.File{}
	fx.res.files[21] = map[string]artifact.File{}
	return fx
}

// add stores files as version n (row id) of slug and makes the highest
// added version the published one.
func (fx *fixture) add(t *testing.T, slug string, id int64, n int, meta map[string]string, files []testFile) {
	t.Helper()
	p := &artifact.Published{Slug: slug, VersionID: id, VersionNumber: n, Visibility: artifact.Public, Metadata: meta}
	p.DeriveFlags()
	if cur, ok := fx.res.latest[slug]; !ok || cur.VersionNumber < n {
		fx.res.latest[slug] = p
	}
	if fx.res.versions[slug] == nil {
		fx.res.versions[slug] = map[int]*artifact.Published{}
	}
	fx.res.versions[slug][n] = p
	m := map[string]artifact.File{}
	for _, f := range files {
		hash, size, err := fx.blobs.Put(context.Background(), strings.NewReader(f.body))
		if err != nil {
			t.Fatal(err)
		}
		m[f.path] = artifact.File{Path: f.path, Hash: hash, Size: size, ContentType: f.contentType}
	}
	fx.res.files[id] = m
}

func (fx *fixture) handler(opts Options) http.Handler {
	if opts.BaseDomain == "" {
		opts.BaseDomain = testBase
	}
	opts.Logger = slog.New(slog.NewTextHandler(fx.logs, nil))
	return NewHandler(fx.res, fx.blobs, opts)
}

func (fx *fixture) hash(slug string, n int, name string) string {
	return fx.res.files[fx.res.versions[slug][n].VersionID][name].Hash
}

func do(t *testing.T, h http.Handler, method, host, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func checkHeaders(t *testing.T, rec *httptest.ResponseRecorder, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, present := rec.Header()[http.CanonicalHeaderKey(k)]
		switch {
		case v == "" && present:
			t.Errorf("header %s = %q, want absent", k, got)
		case v != "" && rec.Header().Get(k) != v:
			t.Errorf("header %s = %q, want %q", k, rec.Header().Get(k), v)
		}
	}
}

func checkClosed(t *testing.T, fx *fixture) {
	t.Helper()
	if n := fx.blobs.openCount(); n != 0 {
		t.Errorf("%d blob readers left open", n)
	}
}

func TestServeLatestAndVersion(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{DefaultCSP: testCSP})

	tests := []struct {
		name    string
		host    string
		target  string
		body    string
		slug    string // the ETag is the hash of slug, version, file
		version int
		file    string
		cache   string
		ctype   string
		robots  string
	}{
		{
			name: "latest root", host: "demo.art.test", target: "/",
			body: "demo v2 index", slug: "demo", version: 2, file: "index.html",
			cache: "no-cache", ctype: "text/html; charset=utf-8", robots: "noindex",
		},
		{
			name: "latest with port", host: "demo.art.test:8080", target: "/index.html",
			body: "demo v2 index", slug: "demo", version: 2, file: "index.html",
			cache: "no-cache", ctype: "text/html; charset=utf-8", robots: "noindex",
		},
		{
			name: "pinned old version", host: "demo--v1.art.test", target: "/",
			body: "demo v1", slug: "demo", version: 1, file: "index.html",
			cache: "public, max-age=31536000, immutable", ctype: "text/html; charset=utf-8", robots: "noindex",
		},
		{
			name: "pinned current version", host: "demo--v2.art.test", target: "/app.js",
			body: "console.log(1)", slug: "demo", version: 2, file: "app.js",
			cache: "public, max-age=31536000, immutable", ctype: "application/javascript; charset=utf-8", robots: "noindex",
		},
		{
			name: "directory index", host: "demo.art.test", target: "/docs/",
			body: "docs index", slug: "demo", version: 2, file: "docs/index.html",
			cache: "no-cache", ctype: "text/html; charset=utf-8", robots: "noindex",
		},
		{
			name: "nested file", host: "demo.art.test", target: "/docs/page.html?x=1",
			body: "docs page", slug: "demo", version: 2, file: "docs/page.html",
			cache: "no-cache", ctype: "text/html; charset=utf-8", robots: "noindex",
		},
		{
			name: "double slashes cleaned", host: "demo.art.test", target: "//docs//page.html",
			body: "docs page", slug: "demo", version: 2, file: "docs/page.html",
			cache: "no-cache", ctype: "text/html; charset=utf-8", robots: "noindex",
		},
		{
			name: "dots inside a name", host: "demo.art.test", target: "/a..b.txt",
			body: "dots", slug: "demo", version: 2, file: "a..b.txt",
			cache: "no-cache", ctype: "text/plain; charset=utf-8", robots: "noindex",
		},
		{
			name: "indexable drops noindex", host: "indexable.art.test", target: "/",
			body: "indexable", slug: "indexable", version: 1, file: "index.html",
			cache: "no-cache", ctype: "text/html; charset=utf-8", robots: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tt.host, tt.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body)
			}
			if got := rec.Body.String(); got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
			etag := `"` + fx.hash(tt.slug, tt.version, tt.file) + `"`
			checkHeaders(t, rec, map[string]string{
				"Content-Type":            tt.ctype,
				"ETag":                    etag,
				"Cache-Control":           tt.cache,
				"X-Robots-Tag":            tt.robots,
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": testCSP,
				"Referrer-Policy":         "strict-origin-when-cross-origin",
				"Last-Modified":           "",
			})
			checkClosed(t, fx)
		})
	}
}

func TestContentTypeCharset(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{})
	tests := []struct {
		target string
		want   string
	}{
		{"/index.html", "text/html; charset=utf-8"},
		{"/app.js", "application/javascript; charset=utf-8"},
		{"/data.json", "application/json; charset=utf-8"},
		{"/style.css", "text/css; charset=iso-8859-1"},
		{"/logo.png", "image/png"},
		{"/blob.bin", "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "demo.art.test", tt.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			checkHeaders(t, rec, map[string]string{"Content-Type": tt.want})
		})
	}

	unit := []struct{ in, want string }{
		{"text/plain", "text/plain; charset=utf-8"},
		{"TEXT/Plain", "TEXT/Plain; charset=utf-8"},
		{"text/html; Charset=UTF-8", "text/html; Charset=UTF-8"},
		{"application/json", "application/json; charset=utf-8"},
		{"application/javascript", "application/javascript; charset=utf-8"},
		{"application/wasm", "application/wasm"},
		{"image/svg+xml", "image/svg+xml"},
		{"", "application/octet-stream"},
	}
	for _, tt := range unit {
		if got := contentType(tt.in); got != tt.want {
			t.Errorf("contentType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPathRules(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{DefaultCSP: testCSP})
	instance404 := string(defaultNotFoundPage)

	tests := []struct {
		name     string
		host     string
		target   string
		status   int
		body     string
		location string
		cache    string
	}{
		{"directory without slash redirects", "demo.art.test", "/docs", 301, "", "/docs/", "no-cache"},
		{"redirect keeps query", "demo.art.test", "/docs?a=b", 301, "", "/docs/?a=b", "no-cache"},
		{"redirect on version host", "demo--v2.art.test", "/docs", 301, "", "/docs/", "public, max-age=31536000, immutable"},
		{"miss without 404.html", "demo.art.test", "/nope.html", 404, instance404, "", "no-cache"},
		{"miss on version host is not immutable", "demo--v2.art.test", "/nope", 404, instance404, "", "no-cache"},
		{"directory miss", "demo.art.test", "/nope/", 404, instance404, "", "no-cache"},
		{"artifact 404.html", "custom.art.test", "/missing", 404, "custom not found", "", "no-cache"},
		{"artifact 404.html for asset", "custom.art.test", "/missing.png", 404, "custom not found", "", "no-cache"},
		{"artifact 404.html served directly", "custom.art.test", "/404.html", 200, "custom not found", "", "no-cache"},
		{"spa route", "spa.art.test", "/settings/profile", 200, "spa shell", "", "no-cache"},
		{"spa directory route", "spa.art.test", "/settings/", 200, "spa shell", "", "no-cache"},
		{"spa asset", "spa.art.test", "/assets/app.js", 200, "spa()", "", "no-cache"},
		{"spa asset miss uses 404.html", "spa.art.test", "/assets/missing.js", 404, "spa not found", "", "no-cache"},
		{"spa without 404.html", "spa-bare.art.test", "/missing.css", 404, instance404, "", "no-cache"},
		{"spa without 404.html route", "spa-bare.art.test", "/route", 200, "bare shell", "", "no-cache"},
		{"no spa for non-spa artifact", "demo.art.test", "/route", 404, instance404, "", "no-cache"},
		{"dotdot at root", "demo.art.test", "/../index.html", 404, instance404, "", "no-cache"},
		{"encoded dotdot", "demo.art.test", "/%2e%2e/index.html", 404, instance404, "", "no-cache"},
		{"dotdot inside", "demo.art.test", "/docs/../index.html", 404, instance404, "", "no-cache"},
		{"encoded dotdot inside", "demo.art.test", "/docs/%2E%2E/index.html", 404, instance404, "", "no-cache"},
		{"trailing dotdot", "demo.art.test", "/docs/..", 404, instance404, "", "no-cache"},
		{"encoded NUL", "demo.art.test", "/index.html%00", 404, instance404, "", "no-cache"},
		{"encoded control char", "demo.art.test", "/index%0a.html", 404, instance404, "", "no-cache"},
		{"encoded slash dotdot", "demo.art.test", "/docs%2f..%2findex.html", 404, instance404, "", "no-cache"},
		{"dotdot does not trigger spa", "spa.art.test", "/../etc/passwd", 404, instance404, "", "no-cache"},
		{"double slash redirect stays on host", "demo.art.test", "//docs", 301, "", "/docs/", "no-cache"},
		{"encoded backslash", "demo.art.test", "/%5Cevil.example", 404, instance404, "", "no-cache"},
		{"backslash does not trigger spa", "spa.art.test", "/%5Cevil.example", 404, instance404, "", "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tt.host, tt.target, nil)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %q", rec.Code, tt.status, rec.Body)
			}
			if tt.status != 301 {
				if got := rec.Body.String(); got != tt.body {
					t.Errorf("body = %q, want %q", got, tt.body)
				}
			}
			want := map[string]string{
				"Cache-Control":           tt.cache,
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": testCSP,
			}
			if tt.location != "" {
				want["Location"] = tt.location
			}
			if tt.status == 404 {
				want["X-Robots-Tag"] = "noindex"
				want["Content-Type"] = "text/html; charset=utf-8"
				want["ETag"] = ""
			}
			checkHeaders(t, rec, want)
			checkClosed(t, fx)
		})
	}
}

func TestStates(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{DefaultCSP: testCSP})
	instance404 := string(defaultNotFoundPage)
	gone := string(defaultGonePage)

	tests := []struct {
		name   string
		host   string
		status int
		body   string
	}{
		{"unknown slug", "nobody.art.test", 404, instance404},
		{"unknown version", "demo--v9.art.test", 404, instance404},
		{"unknown slug pinned", "nobody--v1.art.test", 404, instance404},
		{"unpublished", "unpub.art.test", 404, instance404},
		{"soft-deleted", "deleted.art.test", 404, instance404},
		{"nothing published", "empty.art.test", 404, instance404},
		{"purged", "purged.art.test", 410, gone},
		{"purged pinned", "purged--v1.art.test", 410, gone},
		{"apex", "art.test", 404, instance404},
		{"other domain", "demo.example.com", 404, instance404},
		{"invalid label", "de--mo.art.test", 404, instance404},
		{"extra label", "x.demo.art.test", 404, instance404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tt.host, "/", nil)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			if got := rec.Body.String(); got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
			checkHeaders(t, rec, map[string]string{
				"Content-Type":            "text/html; charset=utf-8",
				"Cache-Control":           "no-cache",
				"X-Robots-Tag":            "noindex",
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": testCSP,
				"ETag":                    "",
			})
		})
	}
}

func TestCustomInstancePages(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{NotFoundPage: []byte("my 404"), GonePage: []byte("my 410")})
	if rec := do(t, h, http.MethodGet, "nobody.art.test", "/", nil); rec.Code != 404 || rec.Body.String() != "my 404" {
		t.Errorf("not found: got %d %q", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "purged.art.test", "/", nil); rec.Code != 410 || rec.Body.String() != "my 410" {
		t.Errorf("gone: got %d %q", rec.Code, rec.Body)
	}
}

func TestCSP(t *testing.T) {
	fx := newFixture(t)
	tests := []struct {
		name   string
		opts   Options
		host   string
		target string
		status int
		want   string // "" means the header is absent
	}{
		{"default", Options{DefaultCSP: testCSP}, "csp.art.test", "/", 200, testCSP},
		{"override allowed", Options{DefaultCSP: testCSP, AllowCSPOverride: true}, "csp.art.test", "/", 200, "default-src *"},
		{"override on artifact 404.html", Options{DefaultCSP: testCSP, AllowCSPOverride: true}, "csp.art.test", "/x", 404, "default-src *"},
		{"no override without metadata", Options{DefaultCSP: testCSP, AllowCSPOverride: true}, "demo.art.test", "/", 200, testCSP},
		{"instance page ignores override", Options{DefaultCSP: testCSP, AllowCSPOverride: true}, "nobody.art.test", "/", 404, testCSP},
		{"override with empty default", Options{AllowCSPOverride: true}, "csp.art.test", "/", 200, "default-src *"},
		{"both empty", Options{}, "csp.art.test", "/", 200, ""},
		{"both empty with override allowed", Options{AllowCSPOverride: true}, "demo.art.test", "/", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, fx.handler(tt.opts), http.MethodGet, tt.host, tt.target, nil)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			checkHeaders(t, rec, map[string]string{"Content-Security-Policy": tt.want})
		})
	}
}

func TestConditionalAndRange(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{DefaultCSP: testCSP})
	etag := `"` + fx.hash("demo", 2, "big.txt") + `"`

	t.Run("if-none-match", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "demo.art.test", "/big.txt", http.Header{"If-None-Match": {etag}})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body)
		}
		checkHeaders(t, rec, map[string]string{"ETag": etag, "Cache-Control": "no-cache"})
		checkClosed(t, fx)
	})
	t.Run("if-none-match stale", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "demo.art.test", "/big.txt", http.Header{"If-None-Match": {`"other"`}})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("range", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "demo.art.test", "/big.txt", http.Header{"Range": {"bytes=2-5"}})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", rec.Code)
		}
		if got := rec.Body.String(); got != "2345" {
			t.Errorf("body = %q, want %q", got, "2345")
		}
		checkHeaders(t, rec, map[string]string{
			"Content-Range":  "bytes 2-5/10",
			"Content-Length": "4",
			"ETag":           etag,
			"Accept-Ranges":  "bytes",
		})
		checkClosed(t, fx)
	})
	t.Run("unsatisfiable range", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "demo.art.test", "/big.txt", http.Header{"Range": {"bytes=50-60"}})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d, want 416", rec.Code)
		}
		checkClosed(t, fx)
	})
}

func TestMethods(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{DefaultCSP: testCSP})

	t.Run("head", func(t *testing.T) {
		rec := do(t, h, http.MethodHead, "demo.art.test", "/big.txt", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body)
		}
		checkHeaders(t, rec, map[string]string{
			"Content-Length": "10",
			"Content-Type":   "text/plain; charset=utf-8",
			"ETag":           `"` + fx.hash("demo", 2, "big.txt") + `"`,
		})
		checkClosed(t, fx)
	})
	t.Run("head artifact 404", func(t *testing.T) {
		rec := do(t, h, http.MethodHead, "custom.art.test", "/missing", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body)
		}
		checkHeaders(t, rec, map[string]string{"Content-Length": "16"})
		checkClosed(t, fx)
	})
	t.Run("head instance 404", func(t *testing.T) {
		rec := do(t, h, http.MethodHead, "nobody.art.test", "/", nil)
		if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Fatalf("got %d with %d body bytes, want 404 and none", rec.Code, rec.Body.Len())
		}
	})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		t.Run(m, func(t *testing.T) {
			rec := do(t, h, m, "demo.art.test", "/", nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
			checkHeaders(t, rec, map[string]string{
				"Allow":                  "GET, HEAD",
				"X-Content-Type-Options": "nosniff",
				"Cache-Control":          "no-cache",
			})
		})
	}
}

func TestInternalErrors(t *testing.T) {
	tests := []struct {
		name  string
		host  string
		setup func(fx *fixture)
		log   string
	}{
		{"missing blob", "broken.art.test", nil, "blob: not found"},
		{"files fail", "nofiles.art.test", nil, "no such version id"},
		{"resolver fails", "demo.art.test", func(fx *fixture) { fx.res.err = errors.New("db down") }, "db down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t)
			if tt.setup != nil {
				tt.setup(fx)
			}
			rec := do(t, fx.handler(Options{}), http.MethodGet, tt.host, "/", nil)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			checkHeaders(t, rec, map[string]string{"Cache-Control": "no-cache", "X-Content-Type-Options": "nosniff", "ETag": ""})
			if !strings.Contains(fx.logs.String(), tt.log) {
				t.Errorf("log %q does not mention %q", fx.logs, tt.log)
			}
		})
	}
}

func TestBlobsClosed(t *testing.T) {
	fx := newFixture(t)
	h := fx.handler(Options{})
	for _, target := range []string{"/", "/app.js", "/docs/", "/big.txt"} {
		do(t, h, http.MethodGet, "demo.art.test", target, nil)
	}
	do(t, h, http.MethodGet, "custom.art.test", "/missing", nil)
	if fx.blobs.opens != 5 {
		t.Errorf("opened %d blobs, want 5", fx.blobs.opens)
	}
	checkClosed(t, fx)
}

// Pinned versions are cached no longer than ImmutableMaxAge, so a slug
// freed after its tombstone is not answered from a cache of the old owner's
// content; values above a year are capped.
func TestImmutableMaxAge(t *testing.T) {
	fx := newFixture(t)
	for _, tt := range []struct {
		age  time.Duration
		want string
	}{
		{0, "public, max-age=31536000, immutable"},
		{90 * 24 * time.Hour, "public, max-age=7776000, immutable"},
		{10 * 365 * 24 * time.Hour, "public, max-age=31536000, immutable"},
	} {
		rec := do(t, fx.handler(Options{ImmutableMaxAge: tt.age}), http.MethodGet, "demo--v1.art.test", "/", nil)
		if got := rec.Header().Get("Cache-Control"); got != tt.want {
			t.Errorf("ImmutableMaxAge %v: Cache-Control = %q, want %q", tt.age, got, tt.want)
		}
	}
}

// A GET that announces a body it never sends is answered at once and the
// connection closed, instead of net/http waiting without a deadline to
// discard the body.
func TestAnnouncedBodyDoesNotHold(t *testing.T) {
	fx := newFixture(t)
	srv := httptest.NewServer(fx.handler(Options{}))
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: demo.art.test\r\nContent-Length: 10\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !resp.Close {
		t.Errorf("status = %d, close = %v; want 200 and Connection: close", resp.StatusCode, resp.Close)
	}
}
