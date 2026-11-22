package artifact_test

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
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

func FuzzParseAnchor(f *testing.F) {
	for _, s := range []string{"12:3fa0", " 12:3FA0 ", "12:3fa0|<h1>Title</h1>", "1:0000", "0:3fa0", "-1:3fa0", "12:3fa", "12:3fa00",
		"12:3fag", "12", ":3fa0", "12:3fa0:1", "99999999999999999999:3fa0", "+1:3fa0", "1 :3fa0", ""} {
		f.Add(s, 12, "<h1>Title</h1>")
	}
	f.Fuzz(func(t *testing.T, s string, n int, line string) {
		num, hash, err := artifact.ParseAnchor(s)
		if err == nil {
			if num < 1 || len(hash) != 4 || strings.Trim(hash, "0123456789abcdef") != "" {
				t.Errorf("ParseAnchor(%q) = %d, %q, want a line from 1 and four lower-case hex digits", s, num, hash)
			}
			canonical := fmt.Sprintf("%d:%s", num, hash)
			if n2, h2, err := artifact.ParseAnchor(canonical); err != nil || n2 != num || h2 != hash {
				t.Errorf("ParseAnchor(%q) = %d, %q, %v, want %d, %q", canonical, n2, h2, err, num, hash)
			}
		} else if num != 0 || hash != "" {
			t.Errorf("ParseAnchor(%q) = %d, %q with an error, want 0 and \"\"", s, num, hash)
		}

		if n < 1 {
			return
		}
		a := artifact.Anchor(n, line)
		for _, in := range []string{a, a + "|" + line} {
			got, h, err := artifact.ParseAnchor(in)
			if err != nil || got != n || fmt.Sprintf("%d:%s", got, h) != a {
				t.Errorf("ParseAnchor(%q) = %d, %q, %v, want the parts of %q", in, got, h, err, a)
			}
		}
	})
}

var elided = regexp.MustCompile(`^\.\.\. \d+ lines not shown \.\.\.$`)

func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// excerptError reports an anchor of excerpt that does not name its line of text.
func excerptError(text, excerpt string) error {
	if text == "" || excerpt == "" {
		if text != excerpt {
			return fmt.Errorf("excerpt %q of %q", excerpt, text)
		}
		return nil
	}
	all := lines(text)
	for row := range strings.SplitSeq(excerpt, "\n") {
		if elided.MatchString(row) {
			continue
		}
		n, hash, err := artifact.ParseAnchor(row)
		if err != nil {
			return fmt.Errorf("row %q: %w", row, err)
		}
		if n > len(all) {
			return fmt.Errorf("row %q names line %d of %d", row, n, len(all))
		}
		if want := artifact.Anchor(n, all[n-1]); want != fmt.Sprintf("%d:%s", n, hash) {
			return fmt.Errorf("row %q: line %d has the anchor %s", row, n, want)
		}
	}
	return nil
}

// outcomeError checks what holds for the outcome of any edit to got.
func outcomeError(got string, o artifact.EditOutcome) error {
	if total := len(lines(got)); o.StartLine < 1 || o.EndLine < o.StartLine-1 || o.EndLine > total {
		return fmt.Errorf("lines %d-%d of %d", o.StartLine, o.EndLine, total)
	}
	if o.Replacements < 1 {
		return fmt.Errorf("%d replacements", o.Replacements)
	}
	return excerptError(got, o.Excerpt)
}

func FuzzApplyEditsText(f *testing.F) {
	seeds := []struct {
		content, old, new string
		all               bool
		limit             uint16
	}{
		{page, "<h1>Q3 report</h1>", "<h1>Q4 report</h1>", false, 0},
		{page, "    <h1>Q3 report</h1>\n", "", false, 0},
		{"a x b x c x\n", "x", "yy", true, 0},
		{"a x b x c x\n", "x", "yy", false, 0},
		{page, "<p>Revenue grew.</p>   \n<p>Costs fell.</p>", "<p>Both.</p>", false, 0},
		{"\t<ul>\r\n\t\t<li>a</li>\r\n\t</ul>\r\n", "<ul>\n  <li>a</li>\n</ul>\n", "<ol>\n  <li>b</li>\n</ol>\n", false, 0},
		{page, "<p>Costs fell.</p>", strings.Repeat("x", 200), false, 100},
		{"café " + strings.Repeat("é", 200) + "\n", "café", "thé", false, 0},
		{page, "missing", "x", false, 0},
		{"", "a", "b", false, 0},
	}
	for _, s := range seeds {
		f.Add(s.content, s.old, s.new, s.all, s.limit)
	}
	f.Fuzz(func(t *testing.T, content, old, repl string, all bool, limit uint16) {
		edit := artifact.Edit{OldText: old, NewText: repl, ReplaceAll: all}
		got, outs, err := artifact.ApplyEdits("index.html", content, []artifact.Edit{edit}, int64(limit))
		valid := utf8.ValidString(content) && utf8.ValidString(old) && utf8.ValidString(repl)
		if err != nil {
			if artifact.CodeOf(err) == "" {
				t.Errorf("ApplyEdits(%q, %+v) = %v, want an error with a code", content, edit, err)
			}
			if errors.Is(err, artifact.ErrTooLarge) && limit == 0 {
				t.Errorf("ApplyEdits(%q, %+v) = %v, want no limit", content, edit, err)
			}
			if valid && !utf8.ValidString(err.Error()) {
				t.Errorf("ApplyEdits(%q, %+v) = %q, want valid UTF-8", content, edit, err)
			}
			return
		}
		if len(outs) != 1 {
			t.Fatalf("ApplyEdits(%q, %+v) gave %d outcomes, want 1", content, edit, len(outs))
		}
		o := outs[0]
		if limit > 0 && len(got) > int(limit) {
			t.Errorf("ApplyEdits(%q, %+v) is %d bytes, want at most %d", content, edit, len(got), limit)
		}
		if valid && !(utf8.ValidString(got) && utf8.ValidString(o.Excerpt)) {
			t.Errorf("ApplyEdits(%q, %+v) = %q with the excerpt %q, want valid UTF-8", content, edit, got, o.Excerpt)
		}
		if err := outcomeError(got, o); err != nil {
			t.Errorf("ApplyEdits(%q, %+v) = %q, %+v: %v", content, edit, got, o, err)
		}
		if !all && o.Replacements != 1 {
			t.Errorf("ApplyEdits(%q, %+v) replaced %d places, want 1", content, edit, o.Replacements)
		}
		switch o.Match {
		case artifact.MatchExact:
			if n := strings.Count(content, old); o.Replacements != n {
				t.Errorf("ApplyEdits(%q, %+v) replaced %d places, want %d", content, edit, o.Replacements, n)
			}
			if want := strings.ReplaceAll(content, old, repl); got != want {
				t.Errorf("ApplyEdits(%q, %+v) = %q, want %q", content, edit, got, want)
			}
			if span := strings.Join(lines(got)[o.StartLine-1:max(o.EndLine, o.StartLine-1)], "\n") + "\n"; repl != "" && !strings.Contains(span, repl) {
				t.Errorf("ApplyEdits(%q, %+v) = %q with lines %d-%d, which do not hold the new text", content, edit, got, o.StartLine, o.EndLine)
			}
		case artifact.MatchTrailingWhitespace, artifact.MatchIndentation:
			if strings.Contains(content, old) {
				t.Errorf("ApplyEdits(%q, %+v) matched by %s, want an exact match", content, edit, o.Match)
			}
		default:
			t.Errorf("ApplyEdits(%q, %+v) matched by %q", content, edit, o.Match)
		}
	})
}

func FuzzApplyEditsAnchor(f *testing.F) {
	f.Add(page, "    <p>Costs fell 2%.</p>\n    <p>Margin rose.</p>", uint(4), uint(0))
	f.Add(page, "", uint(2), uint(1))
	f.Add(page, `<html lang="en">`, uint(0), uint(0))
	f.Add("a\r\nb\r\nc", "x\ny", uint(1), uint(1))
	f.Add("one line", "two\nlines\n", uint(0), uint(0))
	f.Add("\n\n\n", "x", uint(1), uint(0))
	f.Fuzz(func(t *testing.T, content, repl string, from, span uint) {
		all := lines(content)
		if len(all) == 0 {
			return
		}
		anchors := strings.Split(strings.TrimSuffix(artifact.AnchorLines(content), "\n"), "\n")
		if len(anchors) != len(all) {
			t.Fatalf("AnchorLines(%q) has %d lines, want %d", content, len(anchors), len(all))
		}
		first := int(from % uint(len(all)))
		last := first + int(span%uint(len(all)-first))
		start, end := 0, 0
		for i, l := range all[:last+1] {
			if i < first {
				start += len(l) + 1
			}
			end += len(l) + 1
		}
		end = min(end, len(content))
		edit := artifact.Edit{Start: anchors[first], End: anchors[last], NewText: repl}

		same := edit
		same.NewText = content[start:end]
		got, _, err := artifact.ApplyEdits("index.html", content, []artifact.Edit{same}, 0)
		if err != nil || got != content {
			t.Errorf("ApplyEdits(%q, %+v) = %q, %v, want the content unchanged", content, same, got, err)
		}

		got, outs, err := artifact.ApplyEdits("index.html", content, []artifact.Edit{edit}, 0)
		if err != nil {
			t.Fatalf("ApplyEdits(%q, %+v) = %v, want nil for anchors read from the content", content, edit, err)
		}
		before, after := content[:start], content[end:]
		if len(got) < len(before)+len(after) || !strings.HasPrefix(got, before) || !strings.HasSuffix(got, after) {
			t.Fatalf("ApplyEdits(%q, %+v) = %q, want the lines around the edit kept", content, edit, got)
		}
		// The new text takes the line breaks of the lines it replaces.
		mid := strings.ReplaceAll(got[len(before):len(got)-len(after)], "\r", "")
		want := strings.ReplaceAll(repl, "\r", "")
		if mid != want && (repl == "" || strings.HasSuffix(repl, "\n") || mid != want+"\n") {
			t.Errorf("ApplyEdits(%q, %+v) put %q in place of the lines, want the new text", content, edit, got[len(before):len(got)-len(after)])
		}
		if len(outs) != 1 || outs[0].Match != artifact.MatchAnchor || outs[0].StartLine != first+1 {
			t.Fatalf("ApplyEdits(%q, %+v) = %+v, want one anchor outcome from line %d", content, edit, outs, first+1)
		}
		if err := outcomeError(got, outs[0]); err != nil {
			t.Errorf("ApplyEdits(%q, %+v) = %q, %+v: %v", content, edit, got, outs[0], err)
		}
	})
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
