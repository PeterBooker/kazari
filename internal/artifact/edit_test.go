package artifact_test

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

const page = `<html>
  <body>
    <h1>Q3 report</h1>
    <p>Revenue grew.</p>
    <p>Costs fell.</p>
  </body>
</html>
`

func anchor(text string, n int) string {
	return artifact.Anchor(n, strings.Split(text, "\n")[n-1])
}

func TestApplyEditsText(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		edits []artifact.Edit
		want  string
		match []string
		lines [][2]int
	}{
		{
			name:  "exact",
			in:    page,
			edits: []artifact.Edit{{OldText: "<h1>Q3 report</h1>", NewText: "<h1>Q4 report</h1>"}},
			want:  strings.Replace(page, "Q3", "Q4", 1),
			match: []string{artifact.MatchExact},
			lines: [][2]int{{3, 3}},
		},
		{
			name: "in order, later lines shift",
			in:   page,
			edits: []artifact.Edit{
				{OldText: "<p>Costs fell.</p>", NewText: "<p>Costs fell.</p>\n    <p>Margin rose.</p>"},
				{OldText: "    <h1>Q3 report</h1>\n", NewText: ""},
			},
			want:  "<html>\n  <body>\n    <p>Revenue grew.</p>\n    <p>Costs fell.</p>\n    <p>Margin rose.</p>\n  </body>\n</html>\n",
			match: []string{artifact.MatchExact, artifact.MatchExact},
			lines: [][2]int{{4, 5}, {3, 2}},
		},
		{
			name:  "replace all",
			in:    "a x b x c x\n",
			edits: []artifact.Edit{{OldText: "x", NewText: "yy", ReplaceAll: true}},
			want:  "a yy b yy c yy\n",
			match: []string{artifact.MatchExact},
			lines: [][2]int{{1, 1}},
		},
		{
			name:  "trailing whitespace ignored",
			in:    "one  \ntwo\t\nthree\n",
			edits: []artifact.Edit{{OldText: "one\ntwo\n", NewText: "1\n2\n"}},
			want:  "1\n2\nthree\n",
			match: []string{artifact.MatchTrailingWhitespace},
			lines: [][2]int{{1, 2}},
		},
		{
			name:  "indentation ignored, new text re-indented",
			in:    page,
			edits: []artifact.Edit{{OldText: "<p>Revenue grew.</p>\n<p>Costs fell.</p>", NewText: "<p>Revenue grew.</p>\n  <small>see notes</small>\n<p>Costs fell.</p>"}},
			want:  strings.Replace(page, "    <p>Revenue grew.</p>\n", "    <p>Revenue grew.</p>\n      <small>see notes</small>\n", 1),
			match: []string{artifact.MatchIndentation},
			lines: [][2]int{{4, 6}},
		},
		{
			name: "a later replace_all moves an earlier edit",
			in:   "x\nA\nx\nx\n",
			edits: []artifact.Edit{
				{OldText: "A", NewText: "B\nC"},
				{OldText: "x", NewText: "y\ny", ReplaceAll: true},
			},
			want:  "y\ny\nB\nC\ny\ny\ny\ny\n",
			match: []string{artifact.MatchExact, artifact.MatchExact},
			lines: [][2]int{{3, 4}, {1, 2}},
		},
		{
			name:  "repeated lines, overlapping prefixes",
			in:    "a \na \nb \na \na \na \nb \n",
			edits: []artifact.Edit{{OldText: "a\na\nb\n", NewText: "X\n", ReplaceAll: true}},
			want:  "X\na \nX\n",
			match: []string{artifact.MatchTrailingWhitespace},
			lines: [][2]int{{1, 1}},
		},
		{
			name:  "last line without a line break",
			in:    "a\nb",
			edits: []artifact.Edit{{OldText: " b \n", NewText: "c\n"}},
			want:  "a\nc",
			match: []string{artifact.MatchIndentation},
			lines: [][2]int{{2, 2}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, outs, err := artifact.ApplyEdits("index.html", c.in, c.edits, 0)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Errorf("content (-want +got):\n%s", diff)
			}
			for i, o := range outs {
				if o.Match != c.match[i] || o.StartLine != c.lines[i][0] || o.EndLine != c.lines[i][1] {
					t.Errorf("edit %d = %s lines %d-%d, want %s lines %d-%d", i+1, o.Match, o.StartLine, o.EndLine,
						c.match[i], c.lines[i][0], c.lines[i][1])
				}
			}
		})
	}
}

func TestApplyEditsOutcomeExcerpt(t *testing.T) {
	got, outs, err := artifact.ApplyEdits("index.html", page, []artifact.Edit{{OldText: "Revenue grew.", NewText: "Revenue grew 4%."}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	o := outs[0]
	if o.Replacements != 1 || o.StartLine != 4 {
		t.Fatalf("outcome = %+v", o)
	}
	want := strings.Join([]string{
		anchor(got, 2) + "|  <body>",
		anchor(got, 3) + "|    <h1>Q3 report</h1>",
		anchor(got, 4) + "|    <p>Revenue grew 4%.</p>",
		anchor(got, 5) + "|    <p>Costs fell.</p>",
		anchor(got, 6) + "|  </body>",
	}, "\n")
	if o.Excerpt != want {
		t.Errorf("excerpt:\n%s\nwant:\n%s", o.Excerpt, want)
	}
}

func TestApplyEditsTextErrors(t *testing.T) {
	cases := []struct {
		name  string
		edits []artifact.Edit
		code  *artifact.Error
		has   []string
	}{
		{"ambiguous", []artifact.Edit{{OldText: "<p>", NewText: "<p class=x>"}}, artifact.ErrEditAmbiguous,
			[]string{"matches 2 places", "lines 4, 5", "replace_all"}},
		{"not found, closest named", []artifact.Edit{{OldText: "<h1>Q3 reprot</h1>", NewText: "x"}}, artifact.ErrEditNotFound,
			[]string{"closest text is lines 3-3", anchor(page, 3) + "|    <h1>Q3 report</h1>", `start "` + anchor(page, 3) + `"`}},
		{"not found, nothing similar", []artifact.Edit{{OldText: "zzzzqqqq", NewText: "x"}}, artifact.ErrEditNotFound,
			[]string{"nothing similar"}},
		{"same text", []artifact.Edit{{OldText: "a", NewText: "a"}}, artifact.ErrInvalidArgument, []string{"the same"}},
		{"neither mode", []artifact.Edit{{NewText: "a"}}, artifact.ErrInvalidArgument, []string{"old_text", "start anchor"}},
		{"both modes", []artifact.Edit{{OldText: "a", Start: "1:0000", NewText: "b"}}, artifact.ErrInvalidArgument, []string{"one of old_text, start, after or before"}},
		{"insert and replace", []artifact.Edit{{Start: anchor(page, 1), After: anchor(page, 1), NewText: "b"}}, artifact.ErrInvalidArgument,
			[]string{"one of"}},
		{"insert without text", []artifact.Edit{{After: anchor(page, 1)}}, artifact.ErrInvalidArgument, []string{"insert needs new_text"}},
		{"insert with end", []artifact.Edit{{Before: anchor(page, 1), End: anchor(page, 2), NewText: "b"}}, artifact.ErrInvalidArgument,
			[]string{"end applies only with start"}},
		{"mixed", []artifact.Edit{{OldText: "a", NewText: "b"}, {Start: anchor(page, 1), NewText: "b"}}, artifact.ErrInvalidArgument,
			[]string{"separate calls"}},
		{"end without start", []artifact.Edit{{OldText: "a", End: "1:0000", NewText: "b"}}, artifact.ErrInvalidArgument,
			[]string{"end applies to anchor edits"}},
		{"none", nil, artifact.ErrInvalidArgument, []string{"at least one edit"}},
		{"too many edits", make([]artifact.Edit, artifact.MaxEdits+1), artifact.ErrInvalidArgument, []string{"at most 100"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := artifact.ApplyEdits("index.html", page, c.edits, 0)
			if !errors.Is(err, c.code) {
				t.Fatalf("err = %v, want %s", err, c.code.Code)
			}
			for _, s := range c.has {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
		})
	}
}

func TestApplyEditsReplacementCap(t *testing.T) {
	in := strings.Repeat("a", artifact.MaxReplacements+5)
	_, _, err := artifact.ApplyEdits("big.txt", in, []artifact.Edit{{OldText: "a", NewText: "b", ReplaceAll: true}}, 0)
	if !errors.Is(err, artifact.ErrInvalidArgument) || !strings.Contains(err.Error(), "more than 10000 places") {
		t.Errorf("err = %v", err)
	}
	got, outs, err := artifact.ApplyEdits("big.txt", in[:artifact.MaxReplacements], []artifact.Edit{{OldText: "a", NewText: "b", ReplaceAll: true}}, 0)
	if err != nil || got != strings.Repeat("b", artifact.MaxReplacements) || outs[0].Replacements != artifact.MaxReplacements {
		t.Errorf("at the cap: %v, %d replacements", err, outs[0].Replacements)
	}
}

// allocated returns the bytes f allocates, freed or not, which is what a copy
// per match costs even when the collector keeps up.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A refused whitespace-tolerant edit must cost about the file and the new
// text, not matches x new text.
func TestApplyEditsFuzzyRefusalMemory(t *testing.T) {
	const limit = 1 << 20
	const budget = 4 << 20
	large := strings.Repeat("A", 64<<10) + "\nB" // 26 MiB over 400 matches
	small := strings.Repeat("A", 1<<10) + "\nB"  // 10 MiB over the cap
	// Each new line takes the file's indentation, so one copy is what grows.
	lines := strings.Repeat("a\n", 2000)
	wide, narrow := strings.Repeat(" ", 64<<10), strings.Repeat(" ", 16<<10)
	cases := []struct {
		name string
		in   string
		edit artifact.Edit
		code *artifact.Error
		has  string
	}{
		{"indentation, ambiguous", strings.Repeat("x\n", 400),
			artifact.Edit{OldText: " x", NewText: large}, artifact.ErrEditAmbiguous, "matches 400 places"},
		{"indentation, over the limit", strings.Repeat("x\n", 400),
			artifact.Edit{OldText: " x", NewText: large, ReplaceAll: true}, artifact.ErrTooLarge, "over the 1 MiB file limit"},
		{"crlf, ambiguous", strings.Repeat("x\r\n", 400),
			artifact.Edit{OldText: "x ", NewText: large}, artifact.ErrEditAmbiguous, "matches 400 places"},
		{"crlf, over the limit", strings.Repeat("x\r\n", 400),
			artifact.Edit{OldText: "x ", NewText: large, ReplaceAll: true}, artifact.ErrTooLarge, "over the 1 MiB file limit"},
		{"over the replacement cap", strings.Repeat("x\n", artifact.MaxReplacements+5),
			artifact.Edit{OldText: " x", NewText: small, ReplaceAll: true}, artifact.ErrInvalidArgument, "more than 10000 places"},
		{"long indentation, one match", wide + "x\n" + wide + "y\n",
			artifact.Edit{OldText: "x\ny", NewText: lines}, artifact.ErrTooLarge, "over the 1 MiB file limit"},
		{"long indentation, replace_all", strings.Repeat(narrow+"x\n"+narrow+"y\n", 8),
			artifact.Edit{OldText: "x\ny", NewText: lines, ReplaceAll: true}, artifact.ErrTooLarge, "over the 1 MiB file limit"},
		{"long indentation, crlf", wide + "x\r\n" + wide + "y\r\n",
			artifact.Edit{OldText: "x\ny", NewText: lines}, artifact.ErrTooLarge, "over the 1 MiB file limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			got := allocated(func() {
				_, _, err = artifact.ApplyEdits("a.txt", c.in, []artifact.Edit{c.edit}, limit)
			})
			if !errors.Is(err, c.code) || !strings.Contains(err.Error(), c.has) {
				t.Errorf("err = %v, want %s with %q", err, c.code.Code, c.has)
			}
			if got > budget {
				t.Errorf("allocated %d bytes, more than %d", got, budget)
			}
		})
	}
}

// Indentation the old and new text share leaves each match, so an edit whose
// result is small costs about that result however long the new text is.
func TestApplyEditsFuzzyStrippedNewText(t *testing.T) {
	indent := strings.Repeat(" ", 64<<10)
	var got string
	var err error
	spent := allocated(func() {
		got, _, err = artifact.ApplyEdits("a.txt", strings.Repeat("x\n", 400), []artifact.Edit{
			{OldText: indent + "x", NewText: indent + "z", ReplaceAll: true}}, 1<<20)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("z\n", 400); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if spent > 4<<20 {
		t.Errorf("allocated %d bytes, more than %d", spent, 4<<20)
	}
}

// The limit applies to the result, so what a matched line loses counts
// wherever it comes in the file, and so does whatever the match does to the
// new text.
func TestApplyEditsFuzzyAtTheLimit(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		edit  artifact.Edit
		limit int64
		want  string
	}{
		{"growth before the shrinking match", "x\n" + strings.Repeat(" ", 10) + "x\n",
			artifact.Edit{OldText: "\tx", NewText: strings.Repeat("y", 5), ReplaceAll: true}, 12,
			strings.Repeat("y", 5) + "\n" + strings.Repeat("y", 5) + "\n"},
		{"new text over the limit loses its last break", "x",
			artifact.Edit{OldText: " x\n", NewText: "abc\n"}, 3, "abc"},
		{"new text over the limit loses indentation", "x\ny\n",
			artifact.Edit{OldText: "  x\n  y\n", NewText: "  a\n  b\n"}, 4, "a\nb\n"},
		{"crlf counted", "x\r\nx\r\n",
			artifact.Edit{OldText: "x ", NewText: "a\nb", ReplaceAll: true}, 12, "a\r\nb\r\na\r\nb\r\n"},
		{"each match's indentation counted", "  x\n    y\n\tx\n\t\ty\n",
			artifact.Edit{OldText: "x\ny\n", NewText: "a\n\n b\n", ReplaceAll: true}, 21,
			"  a\n\n     b\n\ta\n\n\t\t b\n"},
		{"indentation and crlf counted", "  x\r\n  y\r\n",
			artifact.Edit{OldText: "x\ny\n", NewText: "a\nb\n"}, 10, "  a\r\n  b\r\n"},
		{"a dropped last break leaves no crlf", "  x\r\n  y",
			artifact.Edit{OldText: "x\ny\n", NewText: "a\nb\r\n"}, 9, "  a\r\n  b\r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := artifact.ApplyEdits("a.txt", c.in, []artifact.Edit{c.edit}, c.limit)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if _, _, err := artifact.ApplyEdits("a.txt", c.in, []artifact.Edit{c.edit}, c.limit-1); !errors.Is(err, artifact.ErrTooLarge) {
				t.Errorf("one byte under: err = %v", err)
			}
		})
	}
}

func TestApplyEditsAnchors(t *testing.T) {
	got, outs, err := artifact.ApplyEdits("index.html", page, []artifact.Edit{
		{Start: anchor(page, 5), NewText: "    <p>Costs fell 2%.</p>\n    <p>Margin rose.</p>"},
		{Start: anchor(page, 3), End: anchor(page, 4), NewText: ""},
		{Start: anchor(page, 1) + "|<html>", NewText: `<html lang="en">`},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "<html lang=\"en\">\n  <body>\n    <p>Costs fell 2%.</p>\n    <p>Margin rose.</p>\n  </body>\n</html>\n"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("content (-want +got):\n%s", diff)
	}
	lines := [][2]int{{3, 4}, {3, 2}, {1, 1}}
	for i, o := range outs {
		if o.Match != artifact.MatchAnchor || o.StartLine != lines[i][0] || o.EndLine != lines[i][1] {
			t.Errorf("edit %d = %s lines %d-%d, want lines %d-%d", i+1, o.Match, o.StartLine, o.EndLine, lines[i][0], lines[i][1])
		}
	}
}

func TestApplyEditsAnchorErrors(t *testing.T) {
	moved := strings.Replace(page, "<html>\n", "<!doctype html>\n<html>\n", 1)
	cases := []struct {
		name  string
		edits []artifact.Edit
		code  *artifact.Error
		has   []string
	}{
		{"changed line, moved nearby", []artifact.Edit{{Start: anchor(page, 3), NewText: "x"}}, artifact.ErrAnchorMismatch,
			[]string{"line 3 is now " + anchor(moved, 3) + "|  <body>", "moved to " + anchor(moved, 4), "read it again with anchors"}},
		{"past the end", []artifact.Edit{{Start: "99:0000", NewText: "x"}}, artifact.ErrAnchorMismatch, []string{"past the end", "8 lines"}},
		{"malformed", []artifact.Edit{{Start: "three", NewText: "x"}}, artifact.ErrInvalidArgument, []string{"<line>:<hash>"}},
		{"end before start", []artifact.Edit{{Start: anchor(moved, 4), End: anchor(moved, 2), NewText: "x"}}, artifact.ErrInvalidArgument,
			[]string{"before start"}},
		{"overlap", []artifact.Edit{
			{Start: anchor(moved, 3), End: anchor(moved, 5), NewText: "x"},
			{Start: anchor(moved, 5), NewText: "y"},
		}, artifact.ErrInvalidArgument, []string{"edits 1 and 2 overlap", "lines 3-5 and lines 5-5"}},
		{"insert inside a replacement", []artifact.Edit{
			{Start: anchor(moved, 3), End: anchor(moved, 5), NewText: "x"},
			{After: anchor(moved, 4), NewText: "y"},
		}, artifact.ErrInvalidArgument, []string{"edits 1 and 2 overlap", "lines 3-5 and after line 4"}},
		{"insert at a changed line", []artifact.Edit{{Before: anchor(page, 3), NewText: "x"}}, artifact.ErrAnchorMismatch,
			[]string{"line 3 is now"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := artifact.ApplyEdits("index.html", moved, c.edits, 0)
			if !errors.Is(err, c.code) {
				t.Fatalf("err = %v, want %s", err, c.code.Code)
			}
			for _, s := range c.has {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
		})
	}
}

func TestApplyEditsReview(t *testing.T) {
	t.Run("size limit before building", func(t *testing.T) {
		in := strings.Repeat("a", 1000)
		_, _, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{
			{OldText: "a", NewText: strings.Repeat("b", 1<<20), ReplaceAll: true}}, 25<<20)
		if !errors.Is(err, artifact.ErrTooLarge) || !strings.Contains(err.Error(), "over the 25 MiB file limit") {
			t.Errorf("err = %v", err)
		}
		_, _, err = artifact.ApplyEdits("index.html", "x\n", []artifact.Edit{
			{Start: anchor("x\n", 1), NewText: strings.Repeat("y", 101)}}, 100)
		if !errors.Is(err, artifact.ErrTooLarge) {
			t.Errorf("anchor edit over the limit: err = %v", err)
		}
	})
	t.Run("long lines clipped in excerpts", func(t *testing.T) {
		in := "<html>" + strings.Repeat("<p>x</p>", 20000) + "</html>"
		got, outs, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{{OldText: "<html>", NewText: "<html lang=en>"}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		ex := outs[0].Excerpt
		if len(ex) > 300 || !strings.HasPrefix(ex, artifact.Anchor(1, got)+"|<html lang=en>") || !strings.Contains(ex, "…[+") {
			t.Errorf("excerpt of a long line = %d bytes: %.200q", len(ex), ex)
		}
	})
	t.Run("indentation copied from after the first line's", func(t *testing.T) {
		in := "  <body>\n    <ul>\n      <li>a</li>  \n    </ul>\n  </body>\n"
		got, outs, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{
			{OldText: "<ul>\n      <li>a</li>\n    </ul>", NewText: "<ul>\n      <li>A</li>\n    </ul>"}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if want := "  <body>\n    <ul>\n      <li>A</li>\n    </ul>\n  </body>\n"; got != want || outs[0].Match != artifact.MatchIndentation {
			t.Errorf("got %q (%s), want %q", got, outs[0].Match, want)
		}
	})
	t.Run("crlf kept by fallback edits", func(t *testing.T) {
		in := "<ul>\r\n  <li>a</li>\r\n  <li>b</li>\r\n</ul>\r\n"
		got, _, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{
			{OldText: "<li>a</li>\n  <li>b</li>", NewText: "<li>A</li>\n  <li>B</li>"}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if want := "<ul>\r\n  <li>A</li>\r\n  <li>B</li>\r\n</ul>\r\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("crlf kept by anchor edits", func(t *testing.T) {
		in := "<ul>\r\n  <li>a</li>\r\n  <li>b</li>\r\n</ul>\r\n"
		got, _, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{
			{Start: artifact.Anchor(2, "  <li>a</li>\r"), NewText: "  <li>A</li>\n  <li>A2</li>"}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if want := "<ul>\r\n  <li>A</li>\r\n  <li>A2</li>\r\n  <li>b</li>\r\n</ul>\r\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("long old_text skips fuzzy matching", func(t *testing.T) {
		old := strings.Repeat("x\n", 2001)
		_, _, err := artifact.ApplyEdits("index.html", page, []artifact.Edit{{OldText: old, NewText: "y"}}, 0)
		if !errors.Is(err, artifact.ErrEditNotFound) || !strings.Contains(err.Error(), "more than 2000 lines") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestWriteAnchored(t *testing.T) {
	inputs := []string{"", "a", "a\n", "a\n\nb", "a\r\nb\r\n", "\n", "\n\n"}
	// Lines around the size of StreamAnchored's read buffer, which take the
	// path through Seek.
	for _, n := range []int{4095, 4096, 4097, 8192, 20000} {
		long := strings.Repeat("x", n)
		inputs = append(inputs, long, long+"\n", "a\n"+long+"\nb", long+"\r\n"+long+"\n\nb\n", strings.Repeat(long+"\n", 3))
	}
	inputs = append(inputs, strings.Repeat("é", 5000)+"\nb")
	for _, in := range inputs {
		var b strings.Builder
		if err := artifact.WriteAnchored(&b, in); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
		var want []string
		for i, l := range lines {
			if in != "" {
				want = append(want, artifact.Anchor(i+1, l)+"|"+l)
			}
		}
		w := strings.Join(want, "\n")
		if strings.HasSuffix(in, "\n") {
			w += "\n"
		}
		if b.String() != w || artifact.AnchorLines(in) != w {
			t.Errorf("WriteAnchored(%q) = %q, want %q", in, b.String(), w)
		}
	}
}

func TestAnchorableText(t *testing.T) {
	cut := strings.Repeat("x", 4095) + "é" // a rune across the reader's buffer
	for _, tc := range []struct {
		name, in string
		want     bool
	}{
		{"empty", "", true},
		{"text", "a\r\nb\n", true},
		{"rune across buffers", cut, true},
		{"nul", "a\x00b", false},
		{"nul past the buffer", cut + "\x00", false},
		{"invalid", "a\xffb", false},
		{"encoded replacement rune", "\uFFFD", true},
		{"cut rune at the end", cut[:len(cut)-1], false},
		{"surrogate", "\xed\xa0\x80", false},
	} {
		// iotest.OneByteReader lands reads anywhere in a rune.
		for _, r := range []io.Reader{strings.NewReader(tc.in), iotest.OneByteReader(strings.NewReader(tc.in))} {
			if got, err := artifact.AnchorableText(r); err != nil || got != tc.want {
				t.Errorf("%s: AnchorableText = %v, %v, want %v", tc.name, got, err, tc.want)
			}
		}
	}
	boom := errors.New("boom")
	if _, err := artifact.AnchorableText(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("failed read: err = %v", err)
	}
}

// failingSeeker fails its Read once more than limit bytes have been read, and
// its Seek when told to, however often it was rewound.
type failingSeeker struct {
	*strings.Reader
	limit    int
	read     int
	seekFail bool
}

func (f *failingSeeker) Read(p []byte) (int, error) {
	if f.read >= f.limit {
		return 0, errors.New("boom")
	}
	n, err := f.Reader.Read(p[:min(len(p), f.limit-f.read)])
	f.read += n
	return n, err
}

func (f *failingSeeker) Seek(off int64, whence int) (int64, error) {
	if f.seekFail {
		return 0, errors.New("boom")
	}
	return f.Reader.Seek(off, whence)
}

func TestStreamAnchoredFailures(t *testing.T) {
	long := strings.Repeat("x", 10000) + "\n"
	text := strings.Repeat(long, 3)

	t.Run("read fails on a short line", func(t *testing.T) {
		src := &failingSeeker{Reader: strings.NewReader("a\nb\nc\n"), limit: 2}
		if err := artifact.StreamAnchored(io.Discard, src); !errors.Is(err, artifact.ErrFileRead) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("read fails while hashing a long line", func(t *testing.T) {
		src := &failingSeeker{Reader: strings.NewReader(text), limit: 5000}
		if err := artifact.StreamAnchored(io.Discard, src); !errors.Is(err, artifact.ErrFileRead) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("read fails while copying a long line", func(t *testing.T) {
		src := &failingSeeker{Reader: strings.NewReader(text), limit: 10500}
		if err := artifact.StreamAnchored(io.Discard, src); !errors.Is(err, artifact.ErrFileRead) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("seek fails", func(t *testing.T) {
		src := &failingSeeker{Reader: strings.NewReader(text), limit: len(text) * 3, seekFail: true}
		if err := artifact.StreamAnchored(io.Discard, src); !errors.Is(err, artifact.ErrFileRead) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("write fails", func(t *testing.T) {
		boom := errors.New("gone")
		src := strings.NewReader(strings.Repeat("line\n", 100000))
		if err := artifact.StreamAnchored(failWriter{boom}, src); !errors.Is(err, boom) || errors.Is(err, artifact.ErrFileRead) {
			t.Errorf("err = %v", err)
		}
		if src.Len() == 0 {
			t.Errorf("read the whole file after the client went away")
		}
	})
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestAnchorLines(t *testing.T) {
	got := artifact.AnchorLines("a\n\nb\n")
	want := artifact.Anchor(1, "a") + "|a\n" + artifact.Anchor(2, "") + "|\n" + artifact.Anchor(3, "b") + "|b\n"
	if got != want {
		t.Errorf("AnchorLines = %q, want %q", got, want)
	}
	if artifact.AnchorLines("") != "" {
		t.Errorf("AnchorLines of empty content is not empty")
	}
	n, hash, err := artifact.ParseAnchor(strings.ToUpper(artifact.Anchor(12, "x")) + "|x")
	if err != nil || n != 12 || artifact.Anchor(12, "x") != "12:"+hash {
		t.Errorf("ParseAnchor = %d, %q, %v", n, hash, err)
	}
}

func TestServiceEdit(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", page, "app.js", "run()", "logo.png", "\x89PNG\x00")

	a, v, outs, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{
		Slug: "demo", Path: "index.html", Label: "q4",
		Edits: []artifact.Edit{{OldText: "Q3", NewText: "Q4"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Number != 2 || a.LatestVersion != 2 || a.PublishedVersion != 2 || v.Label != "q4" || outs[0].StartLine != 3 {
		t.Fatalf("edit = artifact %+v version %+v outcomes %+v", a, v, outs)
	}
	if got := e.read(t, alice, "demo", "index.html", 2); got != strings.Replace(page, "Q3", "Q4", 1) {
		t.Errorf("v2 index.html = %q", got)
	}
	if got := e.read(t, alice, "demo", "app.js", 2); got != "run()" {
		t.Errorf("v2 app.js = %q, want it carried over", got)
	}

	cases := []struct {
		name string
		req  artifact.EditRequest
		want *artifact.Error
	}{
		{"stale expected version", artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 1,
			Edits: []artifact.Edit{{OldText: "Q4", NewText: "Q1"}}}, artifact.ErrVersionConflict},
		{"binary file", artifact.EditRequest{Slug: "demo", Path: "logo.png",
			Edits: []artifact.Edit{{OldText: "PNG", NewText: "GIF"}}}, artifact.ErrInvalidArgument},
		{"missing file", artifact.EditRequest{Slug: "demo", Path: "nope.html",
			Edits: []artifact.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrFileNotFound},
		{"bad path", artifact.EditRequest{Slug: "demo", Path: "../x",
			Edits: []artifact.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrInvalidPath},
		{"missing artifact", artifact.EditRequest{Slug: "nope", Path: "index.html",
			Edits: []artifact.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrNotFound},
		{"text not found", artifact.EditRequest{Slug: "demo", Path: "index.html",
			Edits: []artifact.Edit{{OldText: "Q3", NewText: "Q1"}}}, artifact.ErrEditNotFound},
		{"unchanged", artifact.EditRequest{Slug: "demo", Path: "app.js",
			Edits: []artifact.Edit{{OldText: "run()", NewText: "x"}, {OldText: "x", NewText: "run()"}}}, artifact.ErrInvalidArgument},
	}
	for _, c := range cases {
		if _, _, _, err := e.svc.Edit(e.ctx, alice, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.want.Code)
		}
	}

	if _, err := e.svc.Publish(e.ctx, alice, "demo", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 2,
		Edits: []artifact.Edit{{OldText: "Q4", NewText: "Q1"}}}); !errors.Is(err, artifact.ErrPublishedNotLatest) {
		t.Fatalf("edit after rollback without a base: err = %v, want published_not_latest", err)
	}
	_, v, _, err = e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 2, BaseVersion: 2,
		Edits: []artifact.Edit{{OldText: "Q4", NewText: "Q1"}}})
	if err != nil || v.Number != 3 {
		t.Fatalf("edit after rollback on the latest = %v, %v", v, err)
	}
	if v.Label != "edit index.html (1 edit)" {
		t.Errorf("edit label = %q, want %q", v.Label, "edit index.html (1 edit)")
	}

	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []artifact.Edit{{OldText: "Q1", NewText: "Q2"}}}); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("edit of a deleted artifact: err = %v, want not_found", err)
	}
}

func TestApplyEditsInserts(t *testing.T) {
	got, outs, err := artifact.ApplyEdits("index.html", page, []artifact.Edit{
		{After: anchor(page, 3), NewText: "    <p>Intro.</p>"},
		{Start: anchor(page, 5), NewText: "    <p>Costs fell 2%.</p>"},
		{Before: anchor(page, 6) + "|  </body>", NewText: "    <p>End.</p>\n"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "<html>\n  <body>\n    <h1>Q3 report</h1>\n    <p>Intro.</p>\n    <p>Revenue grew.</p>\n" +
		"    <p>Costs fell 2%.</p>\n    <p>End.</p>\n  </body>\n</html>\n"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("content (-want +got):\n%s", diff)
	}
	for i, l := range [][2]int{{4, 4}, {6, 6}, {7, 7}} {
		if o := outs[i]; o.Match != artifact.MatchAnchor || o.StartLine != l[0] || o.EndLine != l[1] {
			t.Errorf("edit %d = %s lines %d-%d, want lines %d-%d", i+1, o.Match, o.StartLine, o.EndLine, l[0], l[1])
		}
	}

	cases := []struct {
		name, in string
		edits    func(in string) []artifact.Edit
		want     string
		line     int
	}{
		{"inserts at one place keep their order", page, func(in string) []artifact.Edit {
			return []artifact.Edit{
				{Before: anchor(in, 5), NewText: "B"},
				{After: anchor(in, 4), NewText: "A"},
				{Start: anchor(in, 5), NewText: "C"},
			}
		}, strings.Replace(page, "    <p>Costs fell.</p>\n", "B\nA\nC\n", 1), 5},
		{"before the first line", "a\n", func(in string) []artifact.Edit {
			return []artifact.Edit{{Before: anchor(in, 1), NewText: "x"}}
		}, "x\na\n", 1},
		{"after a last line without a break", "a\nb", func(in string) []artifact.Edit {
			return []artifact.Edit{{After: anchor(in, 2), NewText: "c"}}
		}, "a\nb\nc", 3},
		{"after a last line without a break, crlf", "a\r\nb", func(in string) []artifact.Edit {
			return []artifact.Edit{{After: anchor(in, 2), NewText: "c\nd"}}
		}, "a\r\nb\r\nc\r\nd", 3},
		{"crlf", "a\r\nb\r\n", func(in string) []artifact.Edit {
			return []artifact.Edit{{After: artifact.Anchor(1, "a\r"), NewText: "x"}}
		}, "a\r\nx\r\nb\r\n", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, outs, err := artifact.ApplyEdits("index.html", c.in, c.edits(c.in), 0)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want || outs[0].StartLine != c.line {
				t.Errorf("got %q at line %d, want %q at line %d", got, outs[0].StartLine, c.want, c.line)
			}
		})
	}
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestApplyEditsExcerptLongNewText(t *testing.T) {
	in := numbered(12)
	newText := "n1\nn2\nn3\nn4\nn5\nn6\n"
	t.Run("inner lines bare", func(t *testing.T) {
		got, outs, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{{OldText: "line 5\n", NewText: newText}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(got, "\n")
		full := func(n int) string { return anchor(got, n) + "|" + lines[n-1] }
		want := strings.Join([]string{full(3), full(4), full(5), anchor(got, 6), anchor(got, 7), anchor(got, 8),
			anchor(got, 9), full(10), full(11), full(12)}, "\n")
		if outs[0].Excerpt != want {
			t.Errorf("excerpt:\n%s\nwant:\n%s", outs[0].Excerpt, want)
		}
	})
	t.Run("reindented text in full", func(t *testing.T) {
		in := strings.ReplaceAll(in, "line", "  line")
		_, outs, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{{OldText: "    line 5\n", NewText: newText}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if outs[0].Match != artifact.MatchIndentation || strings.Count(outs[0].Excerpt, "|") != 10 {
			t.Errorf("%s excerpt:\n%s", outs[0].Match, outs[0].Excerpt)
		}
	})
	t.Run("middle left out", func(t *testing.T) {
		long := numbered(40)
		_, outs, err := artifact.ApplyEdits("index.html", in, []artifact.Edit{{OldText: "line 5\n", NewText: long}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		ex := outs[0].Excerpt
		if !strings.Contains(ex, "\n... 16 lines not shown ...\n") || strings.Count(ex, "|") != 6 || strings.Count(ex, "\n") != 28 {
			t.Errorf("excerpt:\n%s", ex)
		}
	})
}

func TestApplyEditsExcerptsShared(t *testing.T) {
	got, outs, err := artifact.ApplyEdits("index.html", numbered(20), []artifact.Edit{
		{OldText: "line 5\n", NewText: "five\n"},
		{OldText: "line 3\n", NewText: "three\n"},
		{OldText: "line 18\n", NewText: "eighteen\n"},
		{OldText: "line 8\n", NewText: "eight\n"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outs[1].ExcerptOf != 1 || outs[1].Excerpt != "" || outs[3].ExcerptOf != 1 || outs[3].Excerpt != "" {
		t.Fatalf("edits 2 and 4 = %+v, %+v; want both in edit 1's excerpt", outs[1], outs[3])
	}
	if outs[0].ExcerptOf != 0 || outs[2].ExcerptOf != 0 || !strings.HasPrefix(outs[2].Excerpt, anchor(got, 16)+"|") {
		t.Errorf("edits 1 and 3 = %+v, %+v", outs[0], outs[2])
	}
	first := strings.Split(outs[0].Excerpt, "\n")
	if len(first) != 10 || first[0] != anchor(got, 1)+"|line 1" || first[9] != anchor(got, 10)+"|line 10" {
		t.Errorf("shared excerpt:\n%s", outs[0].Excerpt)
	}
}
