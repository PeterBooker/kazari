package mcpserver

import (
	"strings"
	"testing"
	"time"
)

func TestParseGrace(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", 0, false},
		{"72h", 72 * time.Hour, false},
		{"14d", 14 * 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"soon", 0, true},
		{"-1h", 0, true},
		{"0d", 0, true},
	}
	for _, c := range cases {
		got, err := parseGrace(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("parseGrace(%q) = %v, %v", c.in, got, err)
		}
	}
}

func TestParseFileURI(t *testing.T) {
	cases := []struct {
		uri, slug, path string
		ok              bool
	}{
		{"kazari://artifacts/demo/files/index.html", "demo", "index.html", true},
		{"kazari://artifacts/demo/files/assets/a%20b.png", "demo", "assets/a b.png", true},
		{"kazari://artifacts/demo/files/assets%2Fapp.js", "demo", "assets/app.js", true},
		{"kazari://artifacts/demo", "", "", false},
		{"kazari://artifacts/demo/files/", "", "", false},
		{"https://example.com/demo/files/x", "", "", false},
	}
	for _, c := range cases {
		slug, path, ok := parseFileURI(c.uri)
		if slug != c.slug || path != c.path || ok != c.ok {
			t.Errorf("parseFileURI(%q) = %q, %q, %v", c.uri, slug, path, ok)
		}
	}
}

func TestDescribeCSP(t *testing.T) {
	got := describeCSP("default-src 'none'; script-src 'self' https://cdn.example.com; frame-ancestors 'none'")
	for _, s := range []string{"scripts from own files, https://cdn.example.com", "styles from nowhere",
		"cannot be embedded", "Policy: default-src 'none'"} {
		if !strings.Contains(got, s) {
			t.Errorf("describeCSP lacks %q: %s", s, got)
		}
	}
	if strings.Contains(got, "frames from") {
		t.Errorf("describeCSP mentions frame-src the policy does not set: %s", got)
	}
	if got := describeCSP(""); !strings.Contains(got, "none is sent") {
		t.Errorf("empty CSP = %q", got)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{2 << 20: "2 MiB", 256 << 10: "256 KiB", 1500: "1500 bytes", 1 << 30: "1 GiB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDecode(t *testing.T) {
	for _, c := range []struct{ content, enc, want string }{
		{"hi", "", "hi"},
		{"hi", "utf8", "hi"},
		{"aGk=", "base64", "hi"},
		{"aGk", "base64", "hi"},
		{"aG\nk=", "base64", "hi"},
	} {
		got, err := decode("p", c.content, c.enc)
		if err != nil || got != c.want {
			t.Errorf("decode(%q, %q) = %q, %v", c.content, c.enc, got, err)
		}
	}
	if _, err := decode("p", "x", "hex"); err == nil {
		t.Errorf("decode with encoding hex: no error")
	}
}
