package mcpserver_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/mcpserver"
)

// The slips a model makes most often in edit_file's arguments are read as
// meant.
func TestEditFileForgivesCommonSlips(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"old_string and new_string", map[string]any{"expected_version": 1,
			"edits": []map[string]any{{"old_string": "Q3", "new_string": "Q4"}}}},
		{"edits at the top level", map[string]any{"expected_version": 1, "old_text": "Q3", "new_text": "Q4"}},
		{"one edit as an object", map[string]any{"edits": map[string]any{"old_text": "Q3", "new_text": "Q4"}}},
		{"expected_version as a string", map[string]any{"expected_version": "1",
			"edits": []map[string]any{{"old_text": "Q3", "new_text": "Q4"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			cs := e.connect(t, member)
			ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
				"files": []map[string]any{{"path": "index.html", "content": editPage}}})
			args := map[string]any{"slug": "demo", "path": "index.html"}
			maps.Copy(args, c.args)
			out, _ := ok[mcpserver.EditFileResult](t, cs, "edit_file", args)
			if out.Version != 2 {
				t.Errorf("edit_file = %+v, want version 2", out)
			}
		})
	}
}

func TestEditFileWithoutNewTextDeletes(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": editPage}}})
	ok[mcpserver.EditFileResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"edits": []map[string]any{{"old_text": "    <p>Up.</p>\n"}}})
	if rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html"}); strings.Contains(rf.Content, "Up.") {
		t.Errorf("index.html after an edit without new_text = %q", rf.Content)
	}
}

func TestFilesAsAMap(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	out, _ := ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": map[string]any{"index.html": "<p>hi</p>", "app.js": map[string]any{"content": "run()"}}})
	if out.Version != 1 {
		t.Errorf("create with files as a map = %+v", out)
	}
	if rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "app.js"}); rf.Content != "run()" {
		t.Errorf("app.js = %q", rf.Content)
	}
}

// A call the schema refuses is invalid_argument with the shape the tool
// takes, not the SDK's text about anonymous schemas.
func TestSchemaErrorsNameTheShape(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"edit without a kind", "edit_file", map[string]any{"slug": "demo", "path": "index.html",
			"edits": []map[string]any{{"new_text": "x"}}}, "Next step: Send {slug, path, edits: [{old_text, new_text}]"},
		{"unknown field", "update_artifact", map[string]any{"slug": "demo", "bogus": 1}, "Next step: Send {slug, files: [{path, content}]"},
		{"wrong type", "read_file", map[string]any{"slug": "demo", "path": "index.html", "anchors": "yes"},
			"Next step: Check the argument names and types"},
	}
	for _, c := range cases {
		msg := fails(t, cs, c.tool, c.args)
		if !strings.HasPrefix(msg, "invalid_argument: the arguments do not match the input schema of "+c.tool) ||
			!strings.Contains(msg, c.want) || strings.Contains(msg, "anonymous schema") {
			t.Errorf("%s: %s = %q, want invalid_argument with %q", c.name, c.tool, msg, c.want)
		}
	}
}
