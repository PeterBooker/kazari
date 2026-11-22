package cli

import (
	"strings"
	"testing"
)

func FuzzIgnore(f *testing.F) {
	f.Add("*.map\nnode_modules/\n!keep.map\n", "a/keep.map", false)
	f.Add("/docs/**/draft-*\n# c\n", "docs/x/draft-1", false)
	f.Add("**\n!**/", "a/b", true)
	f.Add("[", "[", false)
	f.Fuzz(func(t *testing.T, rules, rel string, dir bool) {
		parsed := parseIgnore(rules)
		matched, include := parsed.match(rel, dir)
		if include && !matched {
			t.Errorf("parseIgnore(%q).match(%q) includes without a matching rule", rules, rel)
		}
		// A last rule that names the path exactly decides it.
		if rel == "" || strings.ContainsAny(rel, "*?[\\\n\r") || strings.HasPrefix(rel, "!") || strings.HasPrefix(rel, "#") ||
			strings.HasSuffix(rel, "/") || strings.Contains(rel, "//") || strings.TrimRight(rel, " \t") != rel {
			return
		}
		exact := parseIgnore(rules + "\n/" + rel + "\n")
		if m, inc := exact.match(rel, dir); !m || inc {
			t.Errorf("rules %q then /%s: match(%q) = %t, %t; want left out", rules, rel, rel, m, inc)
		}
		back := parseIgnore(rules + "\n!/" + rel + "\n")
		if m, inc := back.match(rel, dir); !m || !inc {
			t.Errorf("rules %q then !/%s: match(%q) = %t, %t; want sent", rules, rel, rel, m, inc)
		}
	})
}

func FuzzNormalizeURL(f *testing.F) {
	for _, s := range []string{"api.art.test", "https://api.art.test/v1/", "http://127.0.0.1:8080", "ftp://x", "", " https://a/b/v1 ", "https://[::1]:1/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := normalizeURL(raw)
		if err != nil {
			return
		}
		again, err := normalizeURL(got)
		if err != nil || again != got {
			t.Errorf("normalizeURL(%q) = %q, then %q, %v; want the same again", raw, got, again, err)
		}
		if strings.HasSuffix(got, "/") {
			t.Errorf("normalizeURL(%q) = %q, which ends in /", raw, got)
		}
	})
}
