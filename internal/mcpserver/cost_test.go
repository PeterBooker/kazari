package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

func TestWriteResultsAreSummaries(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	res := call(t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "A long description.",
		"metadata": map[string]any{"team": "web"}, "files": []map[string]any{{"path": "index.html", "content": editPage}}})
	js, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(js, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"url", "version", "slug", "name", "visibility", "published_version", "latest_version"} {
		if _, has := fields[k]; !has {
			t.Errorf("create result lacks %s: %s", k, js)
		}
	}
	for _, k := range []string{"artifact", "description", "owner", "metadata", "created_at", "note"} {
		if _, has := fields[k]; has {
			t.Errorf("create result has %s: %s", k, js)
		}
	}
	if kz, _ := res.Meta["kazari"].(map[string]any); kz["description"] != "A long description." {
		t.Errorf("_meta for the preview = %v", res.Meta)
	}

	off, _ := ok[mcpserver.WriteResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "unpublished"})
	if off.UnpublishedAt == nil || !strings.Contains(off.Note, "offline until set_visibility") {
		t.Errorf("unpublished result = %+v", off)
	}
	on, _ := ok[mcpserver.WriteResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "public"})
	if on.Note != "" {
		t.Errorf("note on a page that is online = %q", on.Note)
	}
}

func TestEditFileInsert(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": editPage}}})
	lines := strings.Split(editPage, "\n")
	out, _ := ok[mcpserver.EditFileResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html", "expected_version": 1,
		"edits": []map[string]any{
			{"after": artifact.Anchor(3, lines[2]), "new_text": "    <p>Intro.</p>"},
			{"before": artifact.Anchor(5, lines[4]), "new_text": "    <p>End.</p>"},
		}})
	if out.Version != 2 || out.LatestVersion != 2 || out.Edits[0].StartLine != 4 || out.Edits[1].StartLine != 6 ||
		out.Edits[1].ExcerptOf != 1 {
		t.Errorf("insert = %+v", out)
	}
	rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html"})
	want := "<html>\n  <body>\n    <h1>Q3</h1>\n    <p>Intro.</p>\n    <p>Up.</p>\n    <p>End.</p>\n  </body>\n</html>\n"
	if rf.Content != want {
		t.Errorf("index.html = %q", rf.Content)
	}
}

// TestGuidanceAllowsBlindEdits keeps the instructions and the skill from
// sending every edit through a read: the model usually knows the text and the
// version already, from what it published.
func TestGuidanceAllowsBlindEdits(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	instructions := cs.InitializeResult().Instructions
	skill := mcpserver.Skill(e.deps.Links, e.opts)
	for name, s := range map[string]string{"instructions": instructions, "skill": skill} {
		flat := strings.ReplaceAll(s, "`", "")
		if strings.Contains(flat, "read_file with anchors: true, then edit_file") {
			t.Errorf("the %s require a read before every edit", name)
		}
		if !strings.Contains(flat, "Read first only when you have not seen the current text") {
			t.Errorf("the %s do not say when to skip the read", name)
		}
	}
}

const sessionCostPath = "testdata/session-cost.txt"

// TestSessionCost measures what a short session sends the model: three pages
// published, each changed with text edits, then one anchor insert after a
// windowed read. A change to a result's size shows as a diff of the golden
// file; refresh it with go test ./internal/mcpserver -run TestSessionCost -update.
func TestSessionCost(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	var b strings.Builder
	total := 0
	step := func(label, tool string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res := call(t, cs, tool, args)
		if res.IsError {
			t.Fatalf("%s: %s", label, text(res))
		}
		n := 0
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				n += len(tc.Text)
			}
		}
		if res.StructuredContent != nil {
			js, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			n += len(js)
		}
		total += n
		fmt.Fprintf(&b, "%-40s %6d\n", label, n)
		return res
	}

	slugs := []string{"notes", "agenda", "links"}
	for _, slug := range slugs {
		step("create_artifact "+slug, "create_artifact", map[string]any{"slug": slug, "name": "Team " + slug,
			"description": "The team's " + slug + " for the autumn planning week, kept as a small static page.",
			"files":       []map[string]any{{"path": "index.html", "content": costPage(slug)}}})
	}
	for _, slug := range slugs {
		step("edit_file "+slug+" (text)", "edit_file", map[string]any{"slug": slug, "path": "index.html", "expected_version": 1,
			"edits": []map[string]any{
				{"old_text": "<h1>Team " + slug + "</h1>", "new_text": "<h1>Team " + slug + ", week 47</h1>"},
				{"old_text": "body { font-family: system-ui, sans-serif; margin: 2rem; }",
					"new_text": "body {\n  font-family: Georgia, 'Times New Roman', serif;\n  margin: 2rem auto;\n  max-width: 40rem;\n" +
						"  line-height: 1.5;\n  color: #222;\n}"},
			}})
	}
	res := step("read_file notes (window, anchors)", "read_file", map[string]any{"slug": "notes", "path": "index.html",
		"start_line": 21, "end_line": 23, "anchors": true})
	var rf mcpserver.ReadFileResult
	js, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(js, &rf); err != nil {
		t.Fatal(err)
	}
	after, _, _ := strings.Cut(rf.Content, "|")
	step("edit_file notes (insert)", "edit_file", map[string]any{"slug": "notes", "path": "index.html", "expected_version": 2,
		"edits": []map[string]any{{"after": after, "new_text": "    <li>Item 0: the first of the week</li>"}}})
	fmt.Fprintf(&b, "%-40s %6d\n", "total", total)

	path := filepath.FromSlash(sessionCostPath)
	if *updateExample {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("the session's results changed size; if that is intended, run go test ./internal/mcpserver -run TestSessionCost -update\ngot:\n%swant:\n%s",
			b.String(), want)
	}
}

const toolsCostPath = "testdata/tools-list.txt"

// TestToolsListCost measures what tools/list sends a model at the start of
// every session, for a member on an instance with the default settings: each
// tool's description and input schema, and the instructions. A change shows
// as a diff of the golden file; refresh it with
// go test ./internal/mcpserver -run TestToolsListCost -update.
func TestToolsListCost(t *testing.T) {
	e := newEnv(t)
	e.opts = exampleOptions()
	cs := e.connect(t, member)
	res, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	total := 0
	for _, tool := range res.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		n := len(tool.Description) + len(schema)
		total += n
		fmt.Fprintf(&b, "%-20s %6d %6d\n", tool.Name, len(tool.Description), len(schema))
	}
	instructions := len(cs.InitializeResult().Instructions)
	fmt.Fprintf(&b, "%-20s %6d\n%-20s %6d\n", "instructions", instructions, "total", total+instructions)

	path := filepath.FromSlash(toolsCostPath)
	if *updateExample {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("tools/list changed size; if that is intended, run go test ./internal/mcpserver -run TestToolsListCost -update\ngot:\n%swant:\n%s",
			b.String(), want)
	}
}

func costPage(slug string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Team %s</title>\n", slug)
	b.WriteString("<style>\nbody { font-family: system-ui, sans-serif; margin: 2rem; }\nli { margin: 0.25rem 0; }\n</style>\n")
	fmt.Fprintf(&b, "</head>\n<body>\n<main>\n  <h1>Team %s</h1>\n  <p>What the team needs for the planning week.</p>\n  <ul>\n", slug)
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&b, "    <li>Item %d: something to bring up on day %d</li>\n", i, (i-1)%5+1)
	}
	b.WriteString("  </ul>\n</main>\n</body>\n</html>\n")
	return b.String()
}
