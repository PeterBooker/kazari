package site_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/site"
)

func FuzzParseHost(f *testing.F) {
	for _, host := range []string{"demo.art.test", "demo--v3.art.test", "demo--v123456789.art.test", "demo.art.test:8443",
		"DeMo--V2.ART.Test", "demo.art.test.", "my-demo-1--v4.art.test", "nav2.art.test", "api.art.test", "art.test", ".art.test",
		"demo--v0.art.test", "demo--v03.art.test", "demo--v1234567890.art.test", "demo--vx.art.test", "demo--v.art.test",
		"de--mo--v2.art.test", "demo--v1--v2.art.test", "--v2.art.test", "ab.art.test", "-demo.art.test", "de_mo.art.test",
		"a.demo.art.test", "demoart.test", "demo.art.test:1:2", "[::1]:8080", "[demo.art.test]:80", ""} {
		f.Add(host, "art.test")
	}
	f.Add("demo.art.test", "ART.TEST")
	f.Add("demo.art.test", "art.test.")
	f.Add("demo.art.test", "")
	f.Fuzz(func(t *testing.T, host, base string) {
		slug, version, ok := site.ParseHost(host, base)
		if !ok {
			if slug != "" || version != 0 {
				t.Errorf("ParseHost(%q, %q) = (%q, %d, false), want (\"\", 0, false)", host, base, slug, version)
			}
			return
		}
		if err := artifact.ValidateSlug(slug, nil); err != nil {
			t.Errorf("ParseHost(%q, %q) = %q, which is no slug: %v", host, base, slug, err)
		}
		if version < 0 || version > 999999999 {
			t.Errorf("ParseHost(%q, %q) = version %d, want 0 to 999999999", host, base, version)
		}
		// A base domain holds no port, and a host under one that does cannot
		// be written back.
		if strings.Contains(base, ":") {
			return
		}
		canonical := slug
		if version > 0 {
			canonical += artifact.VersionSeparator + strconv.Itoa(version)
		}
		canonical += "." + base
		if s, v, ok := site.ParseHost(canonical, base); !ok || s != slug || v != version {
			t.Errorf("ParseHost(%q, %q) = (%q, %d, %v), want (%q, %d, true)", canonical, base, s, v, ok, slug, version)
		}
	})
}

func FuzzAcceptsGzip(f *testing.F) {
	for _, v := range []string{"", "gzip", "x-gzip", "*", "GZIP", "gzip;q=0", "gzip; q=0.5", "gzip;q=1.000", "gzip;q=1.5",
		"gzip;q=0.0001", "gzip;q=", "gzip;level=1", "gzip;;q=1", "br, gzip;q=0, *", "*;q=0", "*;Q=0.3, gzip;q=0",
		"deflate, gzip;q=1.0, *;q=0.5", " , ,gzip ,", "identity", "gzip;q=0;q=1", "gzip;q=-0", "gzip;q=0x1", "gzip;q=1e0"} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v string) {
		got := site.AcceptsGzip([]string{v})
		lower := strings.ToLower(v)
		if got && !strings.Contains(lower, "gzip") && !strings.Contains(lower, "*") {
			t.Errorf("AcceptsGzip(%q) = true, which names neither gzip nor *", v)
		}
		if upper := asciiUpper(v); site.AcceptsGzip([]string{upper}) != got {
			t.Errorf("AcceptsGzip(%q) = %v, but %v for %q", v, got, !got, upper)
		}
		if !site.AcceptsGzip([]string{v, "gzip"}) {
			t.Errorf("AcceptsGzip(%q, \"gzip\") = false, want true", v)
		}
	})
}

func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func FuzzNoneMatch(f *testing.F) {
	hash := strings.Repeat("ab", 32)
	for _, v := range []string{"", "*", `"x"`, `W/"x"`, `"x", "y"`, `"x`, `W/`, `W/x`, `,,"x",`, `"a,b"`,
		`"` + hash + `-gzip"`, `W/"` + hash + `-gzip"`, `"` + hash + `"`, ` "x" , *`} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v string) {
		etag := site.GzipETag(hash)
		got := site.NoneMatch([]string{v}, etag)
		if got && !strings.Contains(v, "*") && !strings.Contains(v, etag) {
			t.Errorf("NoneMatch(%q, %s) = true, which holds neither * nor the tag", v, etag)
		}
		if !site.NoneMatch([]string{v, etag}, etag) {
			t.Errorf("NoneMatch(%q, %s) on a second line = false, want true", v, etag)
		}
		if site.NoneMatch(nil, etag) {
			t.Errorf("NoneMatch(nil, %s) = true, want false", etag)
		}
	})
}
