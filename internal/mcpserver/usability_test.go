package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/mcpserver"
)

// After a rollback, the read says which version it shows and what an edit
// of it needs, and base_version lets the edit change the published text.
func TestEditAfterRollback(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": editPage}}})
	ok[mcpserver.EditFileResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"edits": []map[string]any{{"old_text": "Q3", "new_text": "Bad"}}})
	ok[mcpserver.WriteResult](t, cs, "publish_version", map[string]any{"slug": "demo", "version": 1})

	_, res := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html"})
	if got, want := firstLine(res), "index.html, version 1 (latest 2; edit_file and update_artifact need base_version: 1 "+
		"to change this text), 6 lines"; got != want {
		t.Errorf("read header after a rollback = %q, want %q", got, want)
	}
	_, res = ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html", "version": 2})
	if got := firstLine(res); !strings.Contains(got, "version 2 (the latest; version 1 is published, so edit_file and "+
		"update_artifact need base_version: 2") {
		t.Errorf("read header of the latest after a rollback = %q", got)
	}

	if msg := fails(t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"edits": []map[string]any{{"old_text": "Q3", "new_text": "Q4"}}}); !strings.HasPrefix(msg, "published_not_latest: ") {
		t.Errorf("edit after a rollback without base_version = %q", msg)
	}
	out, _ := ok[mcpserver.EditFileResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"base_version": 1, "expected_version": 2, "edits": []map[string]any{{"old_text": "Q3", "new_text": "Q4"}}})
	if out.Version != 3 || out.PublishedVersion != 3 || out.VersionURL != "https://demo--v3.art.test/" {
		t.Errorf("edit on base 1 = %+v", out)
	}
	rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html"})
	if !strings.Contains(rf.Content, "<h1>Q4</h1>") {
		t.Errorf("index.html after the edit on base 1 = %q", rf.Content)
	}
}

func TestWriteResultWarnsAndSaysUnchanged(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	out, res := ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "report.html", "content": "<p>hi</p>"}}})
	if len(out.Warnings) != 1 || out.VersionURL != "https://demo--v1.art.test/" {
		t.Errorf("create without index.html = %+v", out)
	}
	if lines := strings.Split(firstLine(res), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "Warning: there is no index.html") {
		t.Errorf("create summary = %q, want a warning on its own line", firstLine(res))
	}

	out, res = ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{"slug": "demo",
		"files": []map[string]any{{"path": "report.html", "content": "<p>hi</p>"}}})
	if !out.Unchanged || out.Version != 1 || out.LatestVersion != 1 {
		t.Errorf("update with the same files = %+v", out)
	}
	if got := firstLine(res); !strings.HasPrefix(got, "No change: the files match version 1; no version was added.") {
		t.Errorf("unchanged summary = %q", got)
	}
}

// One update_artifact call can edit several files and add another, making
// one version.
func TestUpdateWithEditsPerFile(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": editPage}, {"path": "app.css", "content": "h1 { color: red }"}}})
	out, res := ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "expected_version": 1,
		"files": []map[string]any{
			{"path": "index.html", "edits": []map[string]any{{"old_text": "Q3", "new_text": "Q4"}}},
			{"path": "/app.css", "edits": []map[string]any{{"old_text": "red", "new_text": "blue"}}},
			{"path": "empty.txt", "content": ""},
		}})
	if out.Version != 2 || len(out.Edited) != 2 || out.Edited[0].Path != "index.html" || out.Edited[1].Path != "app.css" {
		t.Errorf("update with edits = %+v", out)
	}
	if got := firstLine(res); !strings.Contains(got, "edited index.html (edit 1 lines 3-3 (exact))") {
		t.Errorf("update summary = %q", got)
	}
	rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "app.css"})
	if rf.Content != "h1 { color: blue }" {
		t.Errorf("app.css = %q", rf.Content)
	}

	if msg := fails(t, cs, "update_artifact", map[string]any{"slug": "demo",
		"files": []map[string]any{{"path": "new.html"}}}); !strings.HasPrefix(msg, `invalid_argument: file "new.html" has no content`) {
		t.Errorf("file entry without content = %q", msg)
	}
	if msg := fails(t, cs, "update_artifact", map[string]any{"slug": "demo",
		"files": []map[string]any{{"path": "nope.html", "edits": []map[string]any{{"old_text": "a", "new_text": "b"}}}}}); !strings.HasPrefix(msg, "file_not_found: ") ||
		!strings.Contains(msg, "update_artifact") {
		t.Errorf("edits of a missing file = %q", msg)
	}
}

func TestVisibilityWords(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}})

	if msg := fails(t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "private"}); !strings.Contains(msg, `did you mean "members"?`) {
		t.Errorf("set_visibility private = %q", msg)
	}
	if msg := fails(t, cs, "create_artifact", map[string]any{"slug": "other", "name": "x", "description": "x", "visibility": "hidden",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}}); !strings.Contains(msg, `did you mean "members"?`) {
		t.Errorf("create_artifact hidden = %q", msg)
	}
	_, res := ok[mcpserver.WriteResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "members"})
	if got, want := firstLine(res), "demo is now members-only: people who sign in to this instance, or who hold a share link, "+
		"can open it at https://demo.art.test/ (version 1)."; got != want {
		t.Errorf("set_visibility members = %q, want %q", got, want)
	}
	off, res := ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "visibility": "unpublished"})
	if off.UnpublishedAt == nil || !strings.HasPrefix(firstLine(res), "Unpublished https://demo.art.test/") {
		t.Errorf("update_artifact unpublished = %+v, %q", off, firstLine(res))
	}
	on, _ := ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "visibility": "public", "name": "Back"})
	if on.UnpublishedAt != nil || on.Visibility != "public" || on.Name != "Back" {
		t.Errorf("update_artifact public = %+v", on)
	}
}

func TestSlugErrorsSuggest(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "q3-report", "name": "Q3", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}})

	if msg := fails(t, cs, "create_artifact", map[string]any{"slug": "Q4_Report", "name": "Q4", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}}); !strings.Contains(msg, `"q4-report" would do`) {
		t.Errorf("invalid slug = %q, want a suggestion", msg)
	}
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_artifact", map[string]any{"slug": "q3-reprot"}, `not_found: artifact "q3-reprot" not found; did you mean "q3-report"?`},
		{"read_file", map[string]any{"slug": "q3-repor", "path": "index.html"}, `not_found: artifact "q3-repor" not found; did you mean "q3-report"?`},
		{"get_artifact", map[string]any{"slug": "budget"}, `not_found: artifact "budget" not found` + "\n"},
	}
	for _, c := range cases {
		if msg := fails(t, cs, c.tool, c.args); !strings.HasPrefix(msg, c.want) {
			t.Errorf("%s %v = %q, want prefix %q", c.tool, c.args, msg, c.want)
		}
	}
}
