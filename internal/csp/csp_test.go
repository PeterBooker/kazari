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

func TestMerge(t *testing.T) {
	const base = "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; frame-ancestors 'none'"
	tests := []struct {
		name, add, want string
	}{
		{"empty", "", base},
		{"adds to a directive", "script-src https://cdn.example.com",
			"default-src 'self'; script-src 'self' https://cdn.jsdelivr.net https://cdn.example.com; frame-ancestors 'none'"},
		{"new directive starts from default-src", "frame-src https://www.youtube.com",
			"default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; frame-ancestors 'none'; frame-src 'self' https://www.youtube.com"},
		{"no duplicates", "script-src https://cdn.jsdelivr.net", base},
		{"frame-ancestors stays", "frame-ancestors *", base},
		{"wildcard left out", "img-src *", "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; frame-ancestors 'none'; img-src 'self'"},
	}
	for _, tt := range tests {
		if got := csp.Merge(base, tt.add); got != tt.want {
			t.Errorf("%s: Merge(%q) = %q, want %q", tt.name, tt.add, got, tt.want)
		}
	}
	if got, want := csp.Merge("default-src 'none'", "img-src https://i.test"), "default-src 'none'; img-src https://i.test"; got != want {
		t.Errorf("Merge over 'none' = %q, want %q", got, want)
	}
}

func TestValidateAddition(t *testing.T) {
	tests := []struct {
		add string
		ok  bool
	}{
		{"script-src https://cdn.example.com; frame-src https://www.youtube.com", true},
		{"img-src data: blob:", true},
		{"script-src 'unsafe-eval'", true},
		{"frame-ancestors https://x.test", false},
		{"base-uri https://x.test", false},
		{"connect-src *", false},
		{"default-src https:", false},
		{"img-src 'none'", false},
		{"script-src", false},
	}
	for _, tt := range tests {
		if err := csp.ValidateAddition(tt.add); (err == nil) != tt.ok {
			t.Errorf("ValidateAddition(%q) = %v, want ok %v", tt.add, err, tt.ok)
		}
	}
}

func TestValidateAdditionRefusesEveryHost(t *testing.T) {
	tests := []struct {
		name string
		src  []string
		ok   bool
	}{
		{"wildcard", []string{"*", "*:*", "*:443", "*/path", "//*", "*/a://b.test"}, false},
		{"network scheme", []string{"http:", "HTTPS:", "ws:", "wss:"}, false},
		{"wildcard host with scheme", []string{"https://*", "HTTPS://*", "wss://*", "http://*:*", "https://*:443", "https://*/x.js", "ftp://*", "*://*"}, false},
		{"wildcard host after a bare scheme", []string{"https:*", "https:*:443", "wss:/*"}, false},
		{"wildcard without a domain", []string{"*.", "https://*.", "https://*..", "https://*.*", "https://**", "https://a*"}, false},
		{"no host", []string{"https://", ":443", "."}, false},
		{"named host", []string{"https://cdn.example.com", "cdn.example.com", "example.com:443", "https://example.com:*", "example.com:*", "https://cdn.example.com/lib/"}, true},
		{"subdomain wildcard", []string{"https://*.example.com", "*.example.com", "wss://*.example.com:*", "https://*.example.com."}, true},
		{"other scheme", []string{"data:", "blob:", "mediastream:"}, true},
		{"keyword", []string{"'self'", "'unsafe-eval'", "'nonce-abc'", "'sha256-abc='"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, s := range tt.src {
				add := "img-src " + s
				if err := csp.ValidateAddition(add); (err == nil) != tt.ok {
					t.Errorf("ValidateAddition(%q) = %v, want ok %v", add, err, tt.ok)
				}
				merged := csp.Merge("img-src https://base.test", add)
				if added := merged != "img-src https://base.test"; added != tt.ok {
					t.Errorf("Merge(img-src https://base.test, %q) = %q, want the source added %v", add, merged, tt.ok)
				}
			}
		})
	}
}

func TestAllows(t *testing.T) {
	src := []string{"'self'", "data:", "https://fonts.googleapis.com", "*.jsdelivr.net"}
	tests := []struct {
		url  string
		want bool
	}{
		{"https://fonts.googleapis.com/css2?family=Inter", true},
		{"https://cdn.jsdelivr.net/npm/x.js", true},
		{"https://cdnjs.cloudflare.com/x.css", false},
		{"https://example.com/a.png", false},
	}
	for _, tt := range tests {
		if got := csp.Allows(src, tt.url); got != tt.want {
			t.Errorf("Allows(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
	if !csp.Allows([]string{"https:"}, "https://any.test/x.png") {
		t.Error("Allows(https:, https://any.test) = false, want true")
	}
	if !csp.Allows([]string{"*"}, "https://any.test/x.png") {
		t.Error("Allows(*, https://any.test) = false, want true")
	}
}
