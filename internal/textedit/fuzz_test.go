package textedit_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/textedit"
)

func FuzzParseAnchor(f *testing.F) {
	for _, s := range []string{"12:3fa0", " 12:3FA0 ", "12:3fa0|<h1>Title</h1>", "1:0000", "0:3fa0", "-1:3fa0", "12:3fa", "12:3fa00",
		"12:3fag", "12", ":3fa0", "12:3fa0:1", "99999999999999999999:3fa0", "+1:3fa0", "1 :3fa0", ""} {
		f.Add(s, 12, "<h1>Title</h1>")
	}
	f.Fuzz(func(t *testing.T, s string, n int, line string) {
		num, hash, err := textedit.ParseAnchor(s)
		if err == nil {
			if num < 1 || len(hash) != 4 || strings.Trim(hash, "0123456789abcdef") != "" {
				t.Errorf("ParseAnchor(%q) = %d, %q, want a line from 1 and four lower-case hex digits", s, num, hash)
			}
			canonical := fmt.Sprintf("%d:%s", num, hash)
			if n2, h2, err := textedit.ParseAnchor(canonical); err != nil || n2 != num || h2 != hash {
				t.Errorf("ParseAnchor(%q) = %d, %q, %v, want %d, %q", canonical, n2, h2, err, num, hash)
			}
		} else if num != 0 || hash != "" {
			t.Errorf("ParseAnchor(%q) = %d, %q with an error, want 0 and \"\"", s, num, hash)
		}

		if n < 1 {
			return
		}
		a := textedit.Anchor(n, line)
		for _, in := range []string{a, a + "|" + line} {
			got, h, err := textedit.ParseAnchor(in)
			if err != nil || got != n || fmt.Sprintf("%d:%s", got, h) != a {
				t.Errorf("ParseAnchor(%q) = %d, %q, %v, want the parts of %q", in, got, h, err, a)
			}
		}
	})
}

// coded reports whether err is one of the package's errors, which carry a
// code.
func coded(err error) bool {
	for _, s := range []error{textedit.ErrInvalid, textedit.ErrTooLarge, textedit.ErrNotFound, textedit.ErrAmbiguous,
		textedit.ErrAnchorMismatch} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
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
		n, hash, err := textedit.ParseAnchor(row)
		if err != nil {
			return fmt.Errorf("row %q: %w", row, err)
		}
		if n > len(all) {
			return fmt.Errorf("row %q names line %d of %d", row, n, len(all))
		}
		if want := textedit.Anchor(n, all[n-1]); want != fmt.Sprintf("%d:%s", n, hash) {
			return fmt.Errorf("row %q: line %d has the anchor %s", row, n, want)
		}
	}
	return nil
}

// outcomeError checks what holds for the outcome of any edit to got.
func outcomeError(got string, o textedit.Outcome) error {
	if total := len(lines(got)); o.StartLine < 1 || o.EndLine < o.StartLine-1 || o.EndLine > total {
		return fmt.Errorf("lines %d-%d of %d", o.StartLine, o.EndLine, total)
	}
	if o.Replacements < 1 {
		return fmt.Errorf("%d replacements", o.Replacements)
	}
	return excerptError(got, o.Excerpt)
}

func FuzzApplyText(f *testing.F) {
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
		edit := textedit.Edit{OldText: old, NewText: repl, ReplaceAll: all}
		got, outs, err := textedit.Apply("index.html", content, []textedit.Edit{edit}, int64(limit))
		valid := utf8.ValidString(content) && utf8.ValidString(old) && utf8.ValidString(repl)
		if err != nil {
			if !coded(err) {
				t.Errorf("Apply(%q, %+v) = %v, want an error with a code", content, edit, err)
			}
			if errors.Is(err, textedit.ErrTooLarge) && limit == 0 {
				t.Errorf("Apply(%q, %+v) = %v, want no limit", content, edit, err)
			}
			if valid && !utf8.ValidString(err.Error()) {
				t.Errorf("Apply(%q, %+v) = %q, want valid UTF-8", content, edit, err)
			}
			return
		}
		if len(outs) != 1 {
			t.Fatalf("Apply(%q, %+v) gave %d outcomes, want 1", content, edit, len(outs))
		}
		o := outs[0]
		if limit > 0 && len(got) > int(limit) {
			t.Errorf("Apply(%q, %+v) is %d bytes, want at most %d", content, edit, len(got), limit)
		}
		if valid && !(utf8.ValidString(got) && utf8.ValidString(o.Excerpt)) {
			t.Errorf("Apply(%q, %+v) = %q with the excerpt %q, want valid UTF-8", content, edit, got, o.Excerpt)
		}
		if err := outcomeError(got, o); err != nil {
			t.Errorf("Apply(%q, %+v) = %q, %+v: %v", content, edit, got, o, err)
		}
		if !all && o.Replacements != 1 {
			t.Errorf("Apply(%q, %+v) replaced %d places, want 1", content, edit, o.Replacements)
		}
		switch o.Match {
		case textedit.MatchExact:
			if n := strings.Count(content, old); o.Replacements != n {
				t.Errorf("Apply(%q, %+v) replaced %d places, want %d", content, edit, o.Replacements, n)
			}
			if want := strings.ReplaceAll(content, old, repl); got != want {
				t.Errorf("Apply(%q, %+v) = %q, want %q", content, edit, got, want)
			}
			if span := strings.Join(lines(got)[o.StartLine-1:max(o.EndLine, o.StartLine-1)], "\n") + "\n"; repl != "" && !strings.Contains(span, repl) {
				t.Errorf("Apply(%q, %+v) = %q with lines %d-%d, which do not hold the new text", content, edit, got, o.StartLine, o.EndLine)
			}
		case textedit.MatchTrailingWhitespace, textedit.MatchIndentation:
			if strings.Contains(content, old) {
				t.Errorf("Apply(%q, %+v) matched by %s, want an exact match", content, edit, o.Match)
			}
		default:
			t.Errorf("Apply(%q, %+v) matched by %q", content, edit, o.Match)
		}
	})
}

func FuzzApplyAnchor(f *testing.F) {
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
		anchors := strings.Split(strings.TrimSuffix(textedit.AnchorLines(content), "\n"), "\n")
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
		edit := textedit.Edit{Start: anchors[first], End: anchors[last], NewText: repl}

		same := edit
		same.NewText = content[start:end]
		got, _, err := textedit.Apply("index.html", content, []textedit.Edit{same}, 0)
		if err != nil || got != content {
			t.Errorf("Apply(%q, %+v) = %q, %v, want the content unchanged", content, same, got, err)
		}

		got, outs, err := textedit.Apply("index.html", content, []textedit.Edit{edit}, 0)
		if err != nil {
			t.Fatalf("Apply(%q, %+v) = %v, want nil for anchors read from the content", content, edit, err)
		}
		before, after := content[:start], content[end:]
		if len(got) < len(before)+len(after) || !strings.HasPrefix(got, before) || !strings.HasSuffix(got, after) {
			t.Fatalf("Apply(%q, %+v) = %q, want the lines around the edit kept", content, edit, got)
		}
		// The new text takes the line breaks of the lines it replaces.
		mid := strings.ReplaceAll(got[len(before):len(got)-len(after)], "\r", "")
		want := strings.ReplaceAll(repl, "\r", "")
		if mid != want && (repl == "" || strings.HasSuffix(repl, "\n") || mid != want+"\n") {
			t.Errorf("Apply(%q, %+v) put %q in place of the lines, want the new text", content, edit, got[len(before):len(got)-len(after)])
		}
		if len(outs) != 1 || outs[0].Match != textedit.MatchAnchor || outs[0].StartLine != first+1 {
			t.Fatalf("Apply(%q, %+v) = %+v, want one anchor outcome from line %d", content, edit, outs, first+1)
		}
		if err := outcomeError(got, outs[0]); err != nil {
			t.Errorf("Apply(%q, %+v) = %q, %+v: %v", content, edit, got, outs[0], err)
		}
	})
}
