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
