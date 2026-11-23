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

func FuzzAcceptEncoding(f *testing.F) {
	for _, v := range []string{"", "gzip", "x-gzip", "*", "GZIP", "gzip;q=0", "gzip; q=0.5", "gzip;q=1.000", "gzip;q=1.5",
		"gzip;q=0.0001", "gzip;q=", "gzip;level=1", "gzip;;q=1", "br, gzip;q=0, *", "*;q=0", "*;Q=0.3, gzip;q=0",
		"deflate, gzip;q=1.0, *;q=0.5", " , ,gzip ,", "identity", "gzip;q=0;q=1", "gzip;q=-0", "gzip;q=0x1", "gzip;q=1e0",
		"br;q=0.9, zstd;q=0.9, gzip;q=0.8", "gzip, deflate, br, zstd", "identity;q=1, br;q=0.5", "zstd;q=0, *", "BR"} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v string) {
		lower := strings.ToLower(v)
		for _, name := range []string{"br", "zstd", "gzip"} {
			got := site.AcceptWeight([]string{v}, name)
			if got < 0 || got > 1 {
				t.Errorf("AcceptWeight(%q, %s) = %v, want a weight from 0 to 1", v, name, got)
			}
			if got > 0 && !strings.Contains(lower, name) && !strings.Contains(lower, "*") {
				t.Errorf("AcceptWeight(%q, %s) = %v, from a field that names neither %s nor *", v, name, got, name)
			}
			if upper := asciiUpper(v); site.AcceptWeight([]string{upper}, name) != got {
				t.Errorf("AcceptWeight(%q, %s) = %v, but %v for %q", v, name, got, site.AcceptWeight([]string{upper}, name), upper)
			}
			if w := site.AcceptWeight([]string{v, name}, name); w != 1 {
				t.Errorf("AcceptWeight(%q, %q, %s) = %v, want 1", v, name, name, w)
			}
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
		etag := site.CodingETag(hash, "gzip")
		got := site.NoneMatch([]string{v}, etag)
		if got && !strings.Contains(v, "*") && !strings.Contains(v, etag) {
			t.Errorf("NoneMatch(%q, %s) = true, which holds neither * nor the tag", v, etag)
		}
		if !site.NoneMatch([]string{v, etag}, etag) {
			t.Errorf("NoneMatch(%q, %s) on a second line = false, want true", v, etag)
		}
		if site.Match([]string{v}, etag) && !got {
			t.Errorf("Match(%q, %s) = true but NoneMatch = false; a strong match is also a weak one", v, etag)
		}
		if site.NoneMatch(nil, etag) {
			t.Errorf("NoneMatch(nil, %s) = true, want false", etag)
		}
	})
}
