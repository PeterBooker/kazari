package site

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
)

func TestChoose(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	tag := func(name string) string { return `"` + hash + "-" + name + `"` }
	tests := []struct {
		name   string
		header http.Header
		cached []string
		want   string
		pinned bool
	}{
		{name: "absent field", header: http.Header{}},
		{name: "empty field", header: http.Header{"Accept-Encoding": {""}}},
		{name: "browser", header: http.Header{"Accept-Encoding": {"gzip, deflate, br, zstd"}}, want: "br"},
		{name: "order of the field ignored", header: http.Header{"Accept-Encoding": {"zstd, gzip, br"}}, want: "br"},
		{name: "zstd without br", header: http.Header{"Accept-Encoding": {"gzip, zstd"}}, want: "zstd"},
		{name: "gzip only", header: http.Header{"Accept-Encoding": {"gzip"}}, want: "gzip"},
		{name: "x-gzip", header: http.Header{"Accept-Encoding": {"x-gzip"}}, want: "gzip"},
		{name: "highest weight", header: http.Header{"Accept-Encoding": {"br;q=0.5, gzip;q=0.8, zstd;q=0.7"}}, want: "gzip"},
		{name: "refused", header: http.Header{"Accept-Encoding": {"br;q=0, zstd;q=0, gzip"}}, want: "gzip"},
		{name: "all refused", header: http.Header{"Accept-Encoding": {"br;q=0, zstd;q=0, gzip;q=0"}}},
		{name: "star", header: http.Header{"Accept-Encoding": {"*"}}, want: "br"},
		{name: "star refused", header: http.Header{"Accept-Encoding": {"*;q=0"}}},
		{name: "star below a coding", header: http.Header{"Accept-Encoding": {"*;q=0.1, gzip"}}, want: "gzip"},
		{name: "own weight over star", header: http.Header{"Accept-Encoding": {"*, br;q=0"}}, want: "zstd"},
		{name: "identity preferred", header: http.Header{"Accept-Encoding": {"identity;q=1, br;q=0.5"}}},
		{name: "identity below", header: http.Header{"Accept-Encoding": {"identity;q=0.4, br;q=0.5"}}, want: "br"},
		{name: "identity refused", header: http.Header{"Accept-Encoding": {"identity;q=0, gzip"}}, want: "gzip"},
		{name: "upper case", header: http.Header{"Accept-Encoding": {"GZIP;Q=0.9, ZSTD;Q=0.8"}}, want: "gzip"},
		{name: "cached first among equals", header: http.Header{"Accept-Encoding": {"gzip, br, zstd"}}, cached: []string{"gzip"}, want: "gzip"},
		{name: "cached second among equals", header: http.Header{"Accept-Encoding": {"gzip, br, zstd"}}, cached: []string{"zstd", "gzip"}, want: "zstd"},
		{name: "cached but weighed less", header: http.Header{"Accept-Encoding": {"gzip;q=0.5, br"}}, cached: []string{"gzip"}, want: "br"},
		{name: "cached but refused", header: http.Header{"Accept-Encoding": {"gzip;q=0, br"}}, cached: []string{"gzip"}, want: "br"},
		{name: "if-range names a coding", header: http.Header{"Accept-Encoding": {"br, gzip"}, "If-Range": {tag("gzip")}}, want: "gzip", pinned: true},
		{name: "if-range names a refused coding", header: http.Header{"Accept-Encoding": {"br"}, "If-Range": {tag("gzip")}}},
		{name: "if-range names the stored bytes", header: http.Header{"Accept-Encoding": {"br"}, "If-Range": {`"` + hash + `"`}}},
		{name: "if-match names a coding", header: http.Header{"Accept-Encoding": {"br, zstd"}, "If-Match": {tag("zstd")}}, want: "zstd", pinned: true},
		{name: "if-match star", header: http.Header{"Accept-Encoding": {"zstd, gzip"}, "If-Match": {"*"}}, want: "zstd", pinned: true},
		{name: "if-match weak", header: http.Header{"Accept-Encoding": {"br"}, "If-Match": {"W/" + tag("br")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := parseAcceptEncoding(tt.header.Values("Accept-Encoding"))
			cached := func(c *coding) bool { return slices.Contains(tt.cached, c.name) }
			c, pinned := choose(tt.header, acc, hash, cached)
			got := ""
			if c != nil {
				got = c.name
			}
			if got != tt.want || pinned != tt.pinned {
				t.Errorf("choose(%v, cached %v) = %q, pinned %v; want %q, pinned %v", tt.header, tt.cached, got, pinned, tt.want, tt.pinned)
			}
		})
	}
}

// A browser rejects a zstd frame whose window is over 8 MiB, so none is
// sent, however large the file.
func TestZstdWindow(t *testing.T) {
	src := make([]byte, maxCompressSize)
	for i := range src {
		src[i] = byte(i * 7919 >> 8)
	}
	b, err := encodeZstd(src)
	if err != nil {
		t.Fatal(err)
	}
	var hdr zstd.Header
	if err := hdr.Decode(b); err != nil {
		t.Fatalf("zstd.Header.Decode: %v", err)
	}
	if hdr.WindowSize > 8<<20 {
		t.Errorf("zstd frame of a %d-byte file has a window of %d bytes, want at most %d", len(src), hdr.WindowSize, 8<<20)
	}
	if got := decode(t, "zstd", b); got != string(src) {
		t.Errorf("zstd round trip: %d bytes, want %d", len(got), len(src))
	}
}

// With every slot taken, a client that accepts a coding cached already is
// sent it rather than the stored bytes, even if it prefers another.
func TestCompressionBusyServesCachedCoding(t *testing.T) {
	fx := newFixture(t)
	fx.addCompressed(t)
	rec := instrumenttest.New(t)
	h := fx.handler(Options{Compressions: 1, Telemetry: rec.Providers})
	hash := fx.hash("compressed", 1, "app.js")
	do(t, h, http.MethodGet, "compressed.art.test", "/app.js", http.Header{"Accept-Encoding": {"gzip"}})
	comp(h).slots <- struct{}{}
	defer func() { <-comp(h).slots }()

	w := do(t, h, http.MethodGet, "compressed.art.test", "/app.js", http.Header{"Accept-Encoding": {"br, zstd;q=0.9, gzip;q=0.5"}})
	if got := w.Header().Get("Content-Encoding"); got != "gzip" || decode(t, "gzip", w.Body.Bytes()) != bigJS {
		t.Errorf("GET preferring br with every slot taken: Content-Encoding %q, want the cached gzip", got)
	}
	checkHeaders(t, w, map[string]string{"ETag": `"` + hash + `-gzip"`})
	rec.Want("kazari.site.compression.busy", 0)

	// A validator that names br pins it: the stored bytes, not gzip.
	w = do(t, h, http.MethodGet, "compressed.art.test", "/app.js", http.Header{"Accept-Encoding": {"br, gzip"},
		"Range": {"bytes=0-3"}, "If-Range": {`"` + hash + `-br"`}})
	if got := w.Header().Get("Content-Encoding"); got != "" || w.Code != http.StatusOK {
		t.Errorf("GET with If-Range on br, every slot taken: status %d, Content-Encoding %q, want 200 with the stored bytes", w.Code, got)
	}
	rec.Want("kazari.site.compression.busy", 1)
}
