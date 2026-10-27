package csp_test

import (
	"testing"

	"github.com/PeterBooker/kazari/internal/csp"
)

func TestParseKeepsFirstDirective(t *testing.T) {
	d := csp.Parse("default-src 'self'; img-src https:; img-src 'none'; frame-ancestors")
	if got := d["img-src"]; len(got) != 1 || got[0] != "https:" {
		t.Errorf("img-src = %v", got)
	}
	if got, ok := d["frame-ancestors"]; !ok || len(got) != 0 {
		t.Errorf("frame-ancestors = %v, %v", got, ok)
	}
	if src, ok := csp.Sources(d, "style-src"); !ok || src[0] != "'self'" {
		t.Errorf("style-src fell back to %v, %v", src, ok)
	}
	if _, ok := csp.Sources(map[string][]string{}, "style-src"); ok {
		t.Error("empty policy has sources")
	}
}

func TestHostAllowed(t *testing.T) {
	src := []string{"'self'", "https:", "*", "https://fonts.googleapis.com", "https://*.jsdelivr.net", "cdnjs.cloudflare.com", "https://x.test:8443", "https://p.test:*"}
	tests := []struct {
		origin string
		want   bool
	}{
		{"https://fonts.googleapis.com", true},
		{"https://fonts.googleapis.com:443", true},
		{"http://fonts.googleapis.com", false},
		{"https://cdn.jsdelivr.net", true},
		{"https://jsdelivr.net", false},
		{"http://cdnjs.cloudflare.com", true},
		{"https://example.com", false},
		{"https://x.test:8443", true},
		{"https://x.test", false},
		{"https://p.test:9", true},
		{"not a url", false},
	}
	for _, tt := range tests {
		if got := csp.HostAllowed(src, tt.origin); got != tt.want {
			t.Errorf("HostAllowed(%q) = %v, want %v", tt.origin, got, tt.want)
		}
	}
}
