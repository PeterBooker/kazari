package artifact_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/textedit"
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

const (
	checkIndex = `<link rel="stylesheet" href="css/site.css"><script src="https://cdn.tailwindcss.com"></script>` +
		`<img src="logo.png"><style>.x{background:url(hero.jpg)}</style>`
	checkCSS  = `@import url("https://fonts.bunny.net/css"); @font-face{src:url(../fonts/a.woff2)}`
	checkPage = `<img src="pic.png"><img src="/logo.png">`
)

func checkSite() []artifact.FileInput {
	return files("index.html", checkIndex, "css/site.css", checkCSS, "a/x.html", checkPage, "b/x.html", checkPage,
		"a/pic.png", "png", "logo.png", "png")
}

func TestCheckCacheWarnings(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	opts := artifact.Options{CSP: checkPolicy}
	svc, _ := countingService(e, opts)
	_, v, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "site", Files: checkSite()})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"b/x.html refers to b/pic.png, which is not in this version",
		"css/site.css loads https://fonts.bunny.net/css, which the page's Content-Security-Policy (style-src) blocks: ship the file in the artifact instead",
		"css/site.css refers to fonts/a.woff2, which is not in this version",
		"index.html loads https://cdn.tailwindcss.com, which the page's Content-Security-Policy (script-src) blocks: ship the file in the artifact instead",
		"index.html refers to hero.jpg, which is not in this version",
	}
	if diff := cmp.Diff(want, v.Warnings); diff != "" {
		t.Errorf("Create: warnings (-want +got):\n%s", diff)
	}
	thrashing := opts
	thrashing.CheckCacheEntries = 1
	tiny, _ := countingService(e, thrashing)
	for _, tt := range []struct {
		name string
		svc  *artifact.Service
	}{
		{"cached", svc},
		{"uncached", func() *artifact.Service { s, _ := countingService(e, opts); return s }()},
		{"evicting", tiny},
		{"evicting again", tiny},
	} {
		_, v, err := tt.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "site", Files: files("index.html", checkIndex)})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, v.Warnings); diff != "" {
			t.Errorf("%s: unchanged Update: warnings (-want +got):\n%s", tt.name, diff)
		}
	}
	_, v, err = svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "site", Files: files("b/pic.png", "png", "hero.jpg", "jpg")})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want[1:4], v.Warnings); diff != "" {
		t.Errorf("Update adding the missing files: warnings (-want +got):\n%s", diff)
	}
}

func TestCheckCacheReads(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	svc, blobs := countingService(e, artifact.Options{CSP: checkPolicy})
	if _, _, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "site", Name: "Site", Files: checkSite()}); err != nil {
		t.Fatal(err)
	}
	if got := len(blobs.opened()); got != 4 {
		t.Errorf("Create opened %d blobs, want the 4 pages and stylesheets", got)
	}

	blobs.reset()
	if _, _, err := svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "site", Files: files("index.html", checkIndex)}); err != nil {
		t.Fatal(err)
	}
	if got := blobs.opened(); len(got) != 0 {
		t.Errorf("unchanged Update opened %d blobs, want none", len(got))
	}

	blobs.reset()
	if _, _, err := svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "site", Files: files("logo.png", "png2")}); err != nil {
		t.Fatal(err)
	}
	if got := blobs.opened(); len(got) != 0 {
		t.Errorf("Update of an image opened %d blobs, want none", len(got))
	}

	blobs.reset()
	edited := strings.Replace(checkPage, "pic.png", "other.png", 1)
	_, _, err := svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "site", Files: []artifact.FileInput{
		{Path: "b/x.html", Edits: []textedit.Edit{{OldText: "pic.png", NewText: "other.png"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{sha(checkPage), sha(edited)}, blobs.opened()); diff != "" {
		t.Errorf("Update editing b/x.html: opened (-want +got):\n%s", diff)
	}
	if got, want := blobs.bytes.Load(), int64(len(checkPage)+len(edited)); got != want {
		t.Errorf("Update editing b/x.html read %d bytes, want %d", got, want)
	}
}

func TestCheckCacheBudget(t *testing.T) {
	tests := []struct {
		name    string
		entries int
		bytes   int64
	}{
		{"entries", 3, artifact.DefaultCheckCacheBytes},
		{"bytes", artifact.DefaultCheckCacheEntries, 1500},
	}
	for _, tt := range tests {
		e := newEnv(t, artifact.Options{})
		alice := e.user(t, "alice")
		svc, _ := countingService(e, artifact.Options{CheckCacheEntries: tt.entries, CheckCacheBytes: tt.bytes})
		pairs := []string{"index.html", "<p>home</p>"}
		for i := range 20 {
			pairs = append(pairs, fmt.Sprintf("page-%02d.html", i), fmt.Sprintf(`<img src="img/%d.png"><script src="https://x.test/%d.js"></script>`, i, i))
		}
		var huge strings.Builder
		for i := range 100 {
			fmt.Fprintf(&huge, `<img src="img/huge-%d.png">`, i)
		}
		pairs = append(pairs, "huge.html", huge.String())
		_, v, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "site", Files: files(pairs...)})
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Warnings) == 0 {
			t.Errorf("%s: Create: no warnings, want the missing images", tt.name)
		}
		entries, bytes := artifact.CheckCacheSize(svc)
		if entries == 0 || entries > tt.entries || bytes > tt.bytes {
			t.Errorf("%s: cache holds %d entries of %d bytes, want 1 to %d entries of at most %d bytes", tt.name, entries, bytes, tt.entries, tt.bytes)
		}
	}
}

// countingBlobs records the blobs opened and counts the bytes read from
// them.
type countingBlobs struct {
	artifact.BlobStore
	bytes atomic.Int64

	mu     sync.Mutex
	hashes []string
}

func (c *countingBlobs) Open(ctx context.Context, hash string) (io.ReadSeekCloser, error) {
	rc, err := c.BlobStore.Open(ctx, hash)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.hashes = append(c.hashes, hash)
	c.mu.Unlock()
	return readCounter{rc, &c.bytes}, nil
}

func (c *countingBlobs) opened() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.hashes)
}

func (c *countingBlobs) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hashes = nil
	c.bytes.Store(0)
}

type readCounter struct {
	io.ReadSeekCloser
	n *atomic.Int64
}

func (r readCounter) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	r.n.Add(int64(n))
	return n, err
}

// countingService is a Service over e's store whose blob reads are counted.
func countingService(e *env, opts artifact.Options) (*artifact.Service, *countingBlobs) {
	blobs := &countingBlobs{BlobStore: e.blobs}
	opts.Now = e.clock.Now
	return artifact.New(e.store, e.store, blobs, artifact.NoQuotas{}, nil, opts), blobs
}

// sitePage is page i of an n-page site, padded to about size bytes.
func sitePage(i, n, size int, variant string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><title>Page %d</title><link rel="stylesheet" href="/css/site.css">`, i)
	b.WriteString(`<script src="https://cdn.tailwindcss.com"></script><img src="img/missing.png">`)
	fmt.Fprintf(&b, `<a href="page-%d.html">next</a><a href="page-%d.html">previous</a><p>%s</p>`, (i+1)%n, (i+n-1)%n, variant)
	for b.Len() < size {
		fmt.Fprintf(&b, `<p>Paragraph of page %d with an <img src="img/%d.png"> and some text to read.</p>`, i, b.Len()%7)
	}
	return b.String()
}

func siteFiles(n, size int) []artifact.FileInput {
	pairs := []string{"index.html", sitePage(0, n, size, ""), "css/site.css", `@font-face{src:url(../fonts/a.woff2)}`}
	for i := 1; i < n; i++ {
		pairs = append(pairs, fmt.Sprintf("page-%d.html", i), sitePage(i, n, size, ""))
	}
	for i := range 7 {
		pairs = append(pairs, fmt.Sprintf("img/%d.png", i), "png")
	}
	return files(pairs...)
}

// BenchmarkUpdateOnePage changes one page of a 100-page site per
// iteration and reports the bytes the publish read back from the store.
func BenchmarkUpdateOnePage(b *testing.B) {
	e := newEnv(b, artifact.Options{CSP: checkPolicy})
	alice := e.user(b, "alice")
	svc, blobs := countingService(e, artifact.Options{CSP: checkPolicy})
	if _, _, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "site", Files: siteFiles(100, 64<<10)}); err != nil {
		b.Fatal(err)
	}
	blobs.reset()
	i := 0
	for b.Loop() {
		i++
		page := sitePage(3, 100, 64<<10, fmt.Sprint(i))
		if _, _, err := svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "site", Files: files("page-3.html", page)}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(blobs.bytes.Load())/float64(b.N), "read-B/op")
	b.ReportMetric(float64(len(blobs.opened()))/float64(b.N), "opens/op")
}
