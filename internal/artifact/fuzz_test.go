package artifact_test

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/artifact"
)

func FuzzValidateSlug(f *testing.F) {
	for _, s := range []string{"demo", "my-demo-1", "abc", strings.Repeat("a", 63), strings.Repeat("a", 64), "ab", "de--mo",
		"-demo", "demo-", "Demo", "de_mo", "de mo", "démo", "api", ""} {
		f.Add(s, "api")
	}
	f.Fuzz(func(t *testing.T, slug, reserved string) {
		err := artifact.ValidateSlug(slug, []string{reserved})
		if err != nil {
			if !errors.Is(err, artifact.ErrInvalidSlug) {
				t.Errorf("ValidateSlug(%q) = %v, want invalid_slug", slug, err)
			}
			return
		}
		if len(slug) < artifact.SlugMinLen || len(slug) > artifact.SlugMaxLen {
			t.Errorf("ValidateSlug(%q) = nil, want an error for %d characters", slug, len(slug))
		}
		if slug == reserved {
			t.Errorf("ValidateSlug(%q) = nil, want an error for a reserved slug", slug)
		}
		if strings.Contains(slug, artifact.VersionSeparator) || strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
			t.Errorf("ValidateSlug(%q) = nil, want an error for its hyphens", slug)
		}
		if strings.Trim(slug, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Errorf("ValidateSlug(%q) = nil, want an error for a character outside a-z, 0-9 and -", slug)
		}
		if err := artifact.ValidateSlugPattern(slug); err != nil {
			t.Errorf("ValidateSlugPattern(%q) = %v, want nil for a valid slug", slug, err)
		}
	})
}

func FuzzSuggestSlug(f *testing.F) {
	for _, s := range []string{"Q3_Report", "my demo", "--a--b--", "Démo page", "ab", strings.Repeat("ab-", 40), "demo", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := artifact.SuggestSlug(s)
		if got == "" {
			return
		}
		if err := artifact.ValidateSlug(got, nil); err != nil || got == s {
			t.Errorf("SuggestSlug(%q) = %q, want a valid slug other than the input: %v", s, got, err)
		}
		if again := artifact.SuggestSlug(got); again != "" {
			t.Errorf("SuggestSlug(%q) = %q, then %q; want no suggestion for a valid slug", s, got, again)
		}
	})
}

func FuzzValidatePath(f *testing.F) {
	for _, p := range []string{"index.html", "a/b/c.css", "./a//b/../c.js", "a/./b", "dir/", "/abs", "..", "../x", "a/../../x",
		"a\\b", "a\x00b", "a\x7fb", "café.html", "bad\xff.html", ".kazari", ".kazari/x", "a/.kazari/x", ".kazarix",
		strings.Repeat("a", 255), strings.Repeat("a", 256), strings.Repeat("a/", 600), ""} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		clean, err := artifact.ValidatePath(p)
		if err != nil {
			if !errors.Is(err, artifact.ErrInvalidPath) {
				t.Errorf("ValidatePath(%q) = %v, want invalid_path", p, err)
			}
			if clean != "" {
				t.Errorf("ValidatePath(%q) = %q with an error, want \"\"", p, clean)
			}
			return
		}
		if !fs.ValidPath(clean) || clean == "." {
			t.Errorf("ValidatePath(%q) = %q, want a rooted-nowhere path without empty, . or .. elements", p, clean)
		}
		if want := strings.TrimPrefix(path.Clean(p), "/"); clean != want {
			t.Errorf("ValidatePath(%q) = %q, want %q", p, clean, want)
		}
		if !utf8.ValidString(clean) || strings.ContainsFunc(clean, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
			t.Errorf("ValidatePath(%q) = %q, want an error for its characters", p, clean)
		}
		if len(clean) > artifact.PathMaxBytes {
			t.Errorf("ValidatePath(%q) is %d bytes, want at most %d", p, len(clean), artifact.PathMaxBytes)
		}
		for seg := range strings.SplitSeq(clean, "/") {
			if len(seg) > artifact.SegmentMaxBytes {
				t.Errorf("ValidatePath(%q) has a segment of %d bytes, want at most %d", p, len(seg), artifact.SegmentMaxBytes)
			}
		}
		if first, _, _ := strings.Cut(clean, "/"); first == artifact.ReservedDir {
			t.Errorf("ValidatePath(%q) = %q, want an error for a path under %s", p, clean, artifact.ReservedDir)
		}
		if again, err := artifact.ValidatePath(clean); err != nil || again != clean {
			t.Errorf("ValidatePath(%q) = %q, %v, want it unchanged", clean, again, err)
		}
	})
}

func FuzzSlugPattern(f *testing.F) {
	for _, s := range [][3]string{
		{"reports-*", "reports-*", "reports-q3"}, {"reports-q3", "reports-*", "reports-q3"}, {"rep*", "reports-*", "reports"},
		{"reports-*", "rep*", "report"}, {"demo", "demo", "demo"}, {"*", "a*", "abc"}, {"a*", "*", "abc"}, {"a-*", "a*", "a-b"},
		{"a--*", "a*", "a--b"}, {"A*", "a*", "abc"}, {"**", "*", "*x"}, {"", "", ""},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, inner, outer, slug string) {
		for _, p := range []string{inner, outer} {
			err := artifact.ValidateSlugPattern(p)
			if err != nil {
				if !errors.Is(err, artifact.ErrInvalidSlug) {
					t.Errorf("ValidateSlugPattern(%q) = %v, want invalid_slug", p, err)
				}
				continue
			}
			prefix, wild := strings.CutSuffix(p, "*")
			if wild && (prefix == "" || len(prefix) > artifact.SlugMaxLen) {
				t.Errorf("ValidateSlugPattern(%q) = nil, want an error for a prefix of %d characters", p, len(prefix))
			}
			if !wild && artifact.ValidateSlug(p, nil) != nil {
				t.Errorf("ValidateSlugPattern(%q) = nil, want an error for what is neither a slug nor a prefix", p)
			}
			if strings.Trim(prefix, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.Contains(prefix, "--") || strings.HasPrefix(prefix, "-") {
				t.Errorf("ValidateSlugPattern(%q) = nil, want an error for what no slug starts with", p)
			}
			if got := artifact.MatchSlug([]string{p}, slug); got != matches(p, slug) {
				t.Errorf("MatchSlug([%q], %q) = %v, want %v", p, slug, got, !got)
			}
			if !artifact.SlugPatternsWithin([]string{p}, []string{p}) {
				t.Errorf("SlugPatternsWithin([%q], [%q]) = false, want true", p, p)
			}
		}
		if !artifact.MatchSlug(nil, slug) {
			t.Errorf("MatchSlug(nil, %q) = false, want true without a restriction", slug)
		}
		if artifact.SlugPatternsWithin([]string{inner}, []string{outer}) &&
			artifact.MatchSlug([]string{inner}, slug) && !artifact.MatchSlug([]string{outer}, slug) {
			t.Errorf("SlugPatternsWithin([%q], [%q]) = true, but only the first matches %q", inner, outer, slug)
		}
	})
}

func matches(pattern, slug string) bool {
	if prefix, wild := strings.CutSuffix(pattern, "*"); wild {
		return strings.HasPrefix(slug, prefix)
	}
	return pattern == slug
}

func FuzzPageTitle(f *testing.F) {
	for _, s := range []string{"<title>Q3 report</title>", "<html><head><title>\n  Team\tnotes </title>", "<title></title>",
		"<title>a&amp;b</title>", "<svg><title>x</title></svg>", "", "<title>" + strings.Repeat("é", 300)} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		title := artifact.PageTitle(data)
		if title == "" {
			return
		}
		if err := artifact.ValidateName(title); err != nil {
			t.Errorf("PageTitle(%q) = %q, an invalid name: %v", data, title, err)
		}
		if title != strings.TrimSpace(title) || strings.Contains(title, "  ") {
			t.Errorf("PageTitle(%q) = %q, want collapsed whitespace", data, title)
		}
	})
}

func FuzzCheckPage(f *testing.F) {
	const policy = "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; img-src 'self' data: https:"
	for _, s := range []string{
		`<script src="https://cdn.tailwindcss.com"></script><img src="/logo.png"><link rel=stylesheet href="a.css">`,
		`<style>@import url("https://fonts.bunny.net/css"); .x{background:url(img/bg.png)}</style>`,
		`<iframe src="//www.youtube.com/embed/x"></iframe><a href="missing.html">`, "", "<style>url(",
	} {
		f.Add("index.html", []byte(s))
	}
	f.Add("css/site.css", []byte(`@font-face{src:url(../fonts/a.woff2)} body{background:url("data:image/png;base64,AA")}`))
	f.Fuzz(func(t *testing.T, page string, data []byte) {
		got := artifact.CheckPage(policy, true, page, data)
		if again := artifact.CheckPage(policy, true, page, data); !slices.Equal(got, again) {
			t.Errorf("CheckPage(%q) differs between runs: %q, then %q", data, got, again)
		}
		seen := map[string]bool{}
		for _, w := range got {
			if seen[w] {
				t.Errorf("CheckPage(%q) repeats %q", data, w)
			}
			seen[w] = true
		}
	})
}

func FuzzParseKeepVersions(f *testing.F) {
	for _, s := range []string{"1", "20", "10000", "10001", "0", "-1", "+5", "05", " 5", "5 ", "five", "", "1e3", "0x10"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := artifact.ParseKeepVersions(s)
		if err != nil {
			return
		}
		if n < 1 || n > artifact.MaxKeepVersions {
			t.Errorf("ParseKeepVersions(%q) = %d, want 1 to %d", s, n, artifact.MaxKeepVersions)
		}
		if got := fmt.Sprint(n); got != s {
			t.Errorf("ParseKeepVersions(%q) = %d, which is written %q", s, n, got)
		}
	})
}

func FuzzLinkCookie(f *testing.F) {
	token := artifact.LinkTokenPrefix + "abcdefgh_" + strings.Repeat("q", 52)
	for _, s := range []string{token, token + ".", token + ".proof", token + ".a.b", "kzs_short", "", ".", "..", "kzs_.x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cookie string) {
		tok, proof, hasProof := artifact.SplitLinkCookie(cookie)
		if hasProof != strings.Contains(cookie, ".") {
			t.Errorf("SplitLinkCookie(%q) hasProof = %t, want it exactly when the cookie holds a dot", cookie, hasProof)
		}
		rejoined := tok
		if hasProof {
			rejoined += "." + proof
		}
		if rejoined != cookie || strings.Contains(tok, ".") {
			t.Errorf("SplitLinkCookie(%q) = %q, %q, %t, which does not make the cookie again", cookie, tok, proof, hasProof)
		}
		if artifact.IsLinkCookie(cookie) != artifact.IsLinkToken(tok) {
			t.Errorf("IsLinkCookie(%q) = %t, but its token %q is a link token: %t",
				cookie, artifact.IsLinkCookie(cookie), tok, artifact.IsLinkToken(tok))
		}
		// A token with a proof is the same link's cookie, told apart from the
		// token alone.
		if artifact.IsLinkToken(cookie) {
			withProof := cookie + "." + proof
			if gotTok, _, has := artifact.SplitLinkCookie(withProof); gotTok != cookie || !has || !artifact.IsLinkCookie(withProof) {
				t.Errorf("SplitLinkCookie(%q) = %q, %t; want the token %q with a proof", withProof, gotTok, has, cookie)
			}
		}
	})
}
