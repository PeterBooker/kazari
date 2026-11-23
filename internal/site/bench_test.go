package site

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/blob"
)

// BenchmarkCompressedJS requests a 1 MiB script in each coding, from the
// cache (hit) and from a fresh one (miss), and reports the encoded size.
func BenchmarkCompressedJS(b *testing.B) {
	blobs, err := blob.NewFS(b.TempDir(), blob.Options{})
	if err != nil {
		b.Fatal(err)
	}
	var js strings.Builder
	for i := 0; js.Len() < 1<<20; i++ {
		fmt.Fprintf(&js, "export function item%d(x) { return compute(x, %d, 'label-%d'); }\n", i, i*7919%10007, i)
	}
	hash, size, err := blobs.Put(context.Background(), strings.NewReader(js.String()[:1<<20]))
	if err != nil {
		b.Fatal(err)
	}
	p := &artifact.Published{ArtifactID: 1, Slug: "bench", VersionID: 1, VersionNumber: 1, Visibility: artifact.Public}
	p.DeriveFlags()
	res := &fakeResolver{
		latest:   map[string]*artifact.Published{"bench": p},
		versions: map[string]map[int]*artifact.Published{"bench": {1: p}},
		files: map[int64]map[string]artifact.File{1: {
			"app.js": {Path: "app.js", Hash: hash, Size: size, ContentType: "text/javascript"},
		}},
	}
	opts := Options{BaseDomain: testBase, Logger: slog.New(slog.DiscardHandler), CompressionCache: DefaultCompressionCache}
	serve := func(b *testing.B, h http.Handler, name string) int {
		b.Helper()
		req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
		req.Host = "bench." + testBase
		req.Header.Set("Accept-Encoding", name)
		w := &discardWriter{header: http.Header{}}
		h.ServeHTTP(w, req)
		if w.status != http.StatusOK || w.header.Get("Content-Encoding") != name {
			b.Fatalf("GET /app.js: status %d, encoding %q, want 200 in %s", w.status, w.header.Get("Content-Encoding"), name)
		}
		n, _ := strconv.Atoi(w.header.Get("Content-Length"))
		return n
	}
	for _, cd := range codings {
		b.Run(cd.name+"/hit", func(b *testing.B) {
			h := NewHandler(res, blobs, opts)
			encoded := serve(b, h, cd.name)
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				serve(b, h, cd.name)
			}
			b.ReportMetric(float64(encoded), "encoded-bytes")
		})
		b.Run(cd.name+"/miss", func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				h := NewHandler(res, blobs, opts)
				b.StartTimer()
				serve(b, h, cd.name)
			}
		})
	}
}

// discardWriter drops the body, so the benchmark measures the server.
type discardWriter struct {
	header http.Header
	status int
}

func (d *discardWriter) Header() http.Header { return d.header }

func (d *discardWriter) WriteHeader(status int) {
	if d.status == 0 {
		d.status = status
	}
}

func (d *discardWriter) Write(p []byte) (int, error) {
	d.WriteHeader(http.StatusOK)
	return len(p), nil
}
