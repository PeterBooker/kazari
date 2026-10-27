package artifact_test

import (
	"errors"
	"strings"
	"testing"

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
		{"both modes", []artifact.Edit{{OldText: "a", Start: "1:0000", NewText: "b"}}, artifact.ErrInvalidArgument, []string{"not both"}},
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
		}, artifact.ErrInvalidArgument, []string{"edits 1 and 2 overlap", "lines 3-5 and 5-5"}},
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
		if !errors.Is(err, artifact.ErrTooLarge) || !strings.Contains(err.Error(), "more than the 26214400-byte file limit") {
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
	for _, in := range []string{"", "a", "a\n", "a\n\nb", "a\r\nb\r\n"} {
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
	if _, v, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 2,
		Edits: []artifact.Edit{{OldText: "Q4", NewText: "Q1"}}}); err != nil || v.Number != 3 {
		t.Fatalf("edit after rollback = %v, %v", v, err)
	}

	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []artifact.Edit{{OldText: "Q1", NewText: "Q2"}}}); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("edit of a deleted artifact: err = %v, want not_found", err)
	}
}
