package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/config"
)

func FuzzParseBytes(f *testing.F) {
	for _, s := range []string{"4MiB", "1000 KB", "52428800", "2 GiB", "1tib", "0", "1536KiB", "5B", " 7 mb ", "9223372036854775807",
		"9223372036854775808", "8388608TiB", "8388607TiB", "-1", "1.5MiB", "MiB", "1 XB", "1KiBB", "", "١"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := config.ParseBytes(s)
		if err != nil {
			if b != 0 {
				t.Errorf("ParseBytes(%q) = %d with an error, want 0", s, b)
			}
			return
		}
		if b < 0 {
			t.Errorf("ParseBytes(%q) = %d, want a size from 0", s, b)
		}
		if again, err := config.ParseBytes(b.String()); err != nil || again != b {
			t.Errorf("ParseBytes(%q) = %d, %v, want %d", b.String(), again, err, b)
		}
		var u config.Bytes
		if err := u.UnmarshalText([]byte(s)); err != nil || u != b {
			t.Errorf("UnmarshalText(%q) = %d, %v, want %d", s, u, err, b)
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{"15m", "336h", "14d", "1d12h", "0", "0d", "1h0m0s", "90s", "1.5h", "-5m", " 2d ", "106751d", "106752d",
		"106751d23h47m16.854775807s", "106751d24h", "1d-1h", "1d+1h", "-1d", "1.5d", "d", "1d1d", "5", "1dh", "", "١d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := config.ParseDuration(s)
		if err != nil {
			if d != 0 {
				t.Errorf("ParseDuration(%q) = %d with an error, want 0", s, d)
			}
			return
		}
		if again, err := config.ParseDuration(d.String()); err != nil || again != d {
			t.Errorf("ParseDuration(%q) = %d, %v, want %d", d.String(), again, err, d)
		}
		if std, err := time.ParseDuration(strings.TrimSpace(s)); err == nil && std != d.Std() {
			t.Errorf("ParseDuration(%q) = %v, want %v as time.ParseDuration gives", s, d.Std(), std)
		}
		if days, rest, ok := strings.Cut(strings.TrimSpace(s), "d"); ok && rest != "" {
			whole, err1 := config.ParseDuration(days + "d")
			part, err2 := config.ParseDuration(rest)
			if err1 != nil || err2 != nil || whole+part != d {
				t.Errorf("ParseDuration(%q) = %v, want %v (%v) and %v (%v) together", s, d, whole, err1, part, err2)
			}
		}
	})
}

func FuzzLoadFile(f *testing.F) {
	for _, s := range []string{
		"base_domain: art.kazari.test\ntls:\n  mode: self-signed\n",
		"limits:\n  max_files: lots\n  max_version_bytes: 50 megs\n",
		"auth:\n  oidc:\n    client_id: x\n",
		"auth: on\n", "listen: &l\n  https: \":443\"\nadmin:\n  <<: *l\n", "a: [b, {c: d}]\n", "- 1\n- 2\n", "base_domian: x\n", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		path := filepath.Join(t.TempDir(), "kazari.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadWith(path, func(string) (string, bool) { return "", false })
		if err != nil {
			if !strings.HasPrefix(err.Error(), "config: ") {
				t.Errorf("LoadWith(%q) error %q does not start with config:", doc, err)
			}
			return
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("LoadWith(%q) returned a configuration that does not validate: %v", doc, err)
		}
	})
}

func FuzzExpand(f *testing.F) {
	for _, s := range []string{"${TOKEN}", "${EMPTY}", "${TOKEN:-dflt}", "${MISSING:-dflt}", "${EMPTY:-dflt}", "${MISSING:-}",
		"${MISSING:-http://x:1/a?b=c$d{e # f}", "${MISSING:-127.0.0.1}:${PORT}", "$$5 and $${TOKEN}", "$TOKEN $ ${TOKEN}$",
		"${MISSING:-$${TOKEN}}", "plain text", "${MISSING}", "${1BAD}", "${TOKEN", "${}", "$", "$$$", ""} {
		f.Add(s, "s3cret")
	}
	f.Add("${TOKEN}", "$${TOKEN} ${TOKEN:-x} $$")
	f.Fuzz(func(t *testing.T, s, value string) {
		vars := func(name string) (string, bool) {
			switch name {
			case "TOKEN":
				return value, true
			case "EMPTY":
				return "", true
			}
			return "", false
		}
		got, errs := config.Expand(s, vars)
		if !strings.Contains(s, "${") {
			if want := strings.ReplaceAll(s, "$$", "$"); got != want || len(errs) > 0 {
				t.Errorf("Expand(%q) = %q, %v, want %q", s, got, errs, want)
			}
		}

		// What a variable holds, and text with its "$" doubled, come out as written.
		escaped := strings.ReplaceAll(s, "$", "$$")
		for in, want := range map[string]string{
			escaped:                          s,
			escaped + "${TOKEN}" + escaped:   s + value + s,
			"${EMPTY:-}${TOKEN:-}" + escaped: value + s,
		} {
			if got, errs := config.Expand(in, vars); got != want || len(errs) > 0 {
				t.Errorf("Expand(%q) with TOKEN=%q = %q, %v, want %q", in, value, got, errs, want)
			}
		}
		if !strings.Contains(s, "}") {
			in := "${MISSING:-" + s + "}"
			if got, errs := config.Expand(in, vars); got != s || len(errs) > 0 {
				t.Errorf("Expand(%q) = %q, %v, want the default", in, got, errs)
			}
		}
	})
}
