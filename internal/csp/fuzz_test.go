package csp_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/csp"
)

func FuzzParse(f *testing.F) {
	for _, p := range []string{
		"default-src 'self'; img-src https:; img-src 'none'; frame-ancestors",
		"default-src 'self'; script-src 'self' https://cdnjs.cloudflare.com; style-src 'self' https://fonts.googleapis.com; " +
			"font-src 'self' https://fonts.gstatic.com; img-src 'self' data: https:",
		"IMG-SRC https://*.jsdelivr.net https://x.test:8443 https://p.test:* cdnjs.cloudflare.com *",
		";;  ; default-src\t'none' ;", "",
	} {
		f.Add(p, "https://cdn.jsdelivr.net")
	}
	f.Add("img-src https://x.test:8443", "https://x.test:8443")
	f.Add("img-src x.test", "not a url")
	f.Fuzz(func(t *testing.T, policy, origin string) {
		got := csp.Parse(policy)
		var b strings.Builder
		for _, name := range slices.Sorted(maps.Keys(got)) {
			if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, "; \t\n\r\v\f") {
				t.Errorf("Parse(%q) has the directive %q, want a lower-case name", policy, name)
			}
			for _, src := range got[name] {
				if src == "" || strings.ContainsAny(src, "; \t\n\r\v\f") {
					t.Errorf("Parse(%q)[%q] holds the source %q", policy, name, src)
				}
			}
			b.WriteString(name + " " + strings.Join(got[name], " ") + "; ")
		}
		if diff := cmp.Diff(got, csp.Parse(b.String())); diff != "" {
			t.Errorf("Parse(%q) differs from Parse(%q) (-want +got):\n%s", b.String(), policy, diff)
		}
		// A repeated directive is ignored.
		if diff := cmp.Diff(got, csp.Parse(policy+";"+policy)); diff != "" {
			t.Errorf("Parse(%q) twice over (-want +got):\n%s", policy, diff)
		}

		for name := range got {
			src, ok := csp.Sources(got, name)
			if !ok {
				t.Errorf("Sources(Parse(%q), %q) not found", policy, name)
			}
			allowed := csp.HostAllowed(src, origin)
			classes := !slices.ContainsFunc(src, func(s string) bool {
				return s != "*" && !strings.HasPrefix(s, "'") && !strings.HasSuffix(s, ":")
			})
			if allowed && classes {
				t.Errorf("HostAllowed(%q, %q) = true, want false without a host source", src, origin)
			}
		}
	})
}
