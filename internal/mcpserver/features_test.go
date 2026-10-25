package mcpserver_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kazari/internal/api"
	"kazari/internal/artifact"
	"kazari/internal/auth"
	"kazari/internal/mcpserver"
)

func (e *env) connectWith(t *testing.T, p auth.Principal, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := mcpserver.NewServer(e.deps, e.opts, p).Connect(e.ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, opts)
	if err := mcp.AddSendingCustomMethod[*skillsGetParams, *skillsGetResult](c, "skills/get"); err != nil {
		t.Fatal(err)
	}
	if err := mcp.AddSendingCustomMethod[*skillsListParams, *skillsListResult](c, "skills/list"); err != nil {
		t.Fatal(err)
	}
	cs, err := c.Connect(e.ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

const editPage = "<html>\n  <body>\n    <h1>Q3</h1>\n    <p>Up.</p>\n  </body>\n</html>\n"

func TestEditFile(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": editPage}, {"path": "app.js", "content": "run()"}}})

	rf, res := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html", "anchors": true})
	if !rf.Anchored || rf.Version != 1 || rf.LatestVersion != 1 || firstLine(res) != artifact.AnchorLines(editPage) {
		t.Fatalf("read_file anchors = %+v %q", rf, firstLine(res))
	}

	out, res := ok[api.EditResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html", "expected_version": 1,
		"edits": []map[string]any{{"old_text": "<h1>Q3</h1>", "new_text": "<h1>Q4</h1>"}}})
	if out.Version != 2 || out.Edits[0].Match != artifact.MatchExact ||
		firstLine(res) != "Published https://demo.art.test/ (version 2): edited index.html; edit 1 lines 3-3 (exact)." {
		t.Fatalf("edit_file = %+v %q", out, firstLine(res))
	}

	out, res = ok[api.EditResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"edits": []map[string]any{{"old_text": "<h1>Q4</h1>\n<p>Up.</p>", "new_text": "<h1>Q4</h1>\n<p>Up 4%.</p>\n<p>Costs flat.</p>"}}})
	if !strings.Contains(firstLine(res), "lines 3-5 (indentation)") {
		t.Errorf("fuzzy edit summary = %q", firstLine(res))
	}
	if !strings.Contains(out.Edits[0].Excerpt, "|    <p>Costs flat.</p>") {
		t.Errorf("excerpt = %q", out.Edits[0].Excerpt)
	}

	var start string
	for _, l := range strings.Split(out.Edits[0].Excerpt, "\n") {
		if strings.HasPrefix(l, "5:") {
			start, _, _ = strings.Cut(l, "|")
		}
	}
	out, _ = ok[api.EditResult](t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html", "expected_version": 3,
		"edits": []map[string]any{{"old_text": "", "start": start, "new_text": ""}}})
	if out.Version != 4 || out.Edits[0].Match != artifact.MatchAnchor || out.Edits[0].EndLine != 4 {
		t.Errorf("anchor edit = %+v", out)
	}
	rf, res = ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html"})
	if want := "<html>\n  <body>\n    <h1>Q4</h1>\n    <p>Up 4%.</p>\n  </body>\n</html>\n"; firstLine(res) != want || rf.Anchored {
		t.Errorf("index.html = %q", firstLine(res))
	}

	msg := fails(t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html", "expected_version": 1,
		"edits": []map[string]any{{"old_text": "Q4", "new_text": "Q1"}}})
	if !strings.HasPrefix(msg, "version_conflict: latest version is 4, expected 1\nNext step: Someone published since you read it") {
		t.Errorf("stale edit = %q", msg)
	}
	msg = fails(t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"edits": []map[string]any{{"start": start, "new_text": "x"}}})
	if !strings.HasPrefix(msg, "anchor_mismatch: edit 1: anchor") || !strings.Contains(msg, "Next step: The file changed") {
		t.Errorf("stale anchor = %q", msg)
	}
	if msg := fails(t, cs, "edit_file", map[string]any{"slug": "demo", "path": "index.html",
		"edits": []map[string]any{{"old_text": "Q4", "start": start, "new_text": "x"}}}); !strings.Contains(msg, "oneOf") {
		t.Errorf("edit with both kinds = %q", msg)
	}

	tools, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "edit_file" {
			continue
		}
		js, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(js), `"oneOf":[{"properties":{"old_text":{"minLength":1,"type":"string"}},"required":["old_text"]}`) {
			t.Errorf("edit_file schema = %s", js)
		}
		if !strings.Contains(tool.Description, "anchor_mismatch") || !strings.Contains(tool.Description, "100") {
			t.Errorf("edit_file description lacks the anchor rules: %s", tool.Description)
		}
	}
}

type skillsListParams struct {
	mcp.ParamsBase
}

type skillsListResult struct {
	mcp.ResultBase
	TTLMs      int          `json:"ttlMs"`
	CacheScope string       `json:"cacheScope"`
	Skills     []skillEntry `json:"skills"`
}

type skillsGetParams struct {
	mcp.ParamsBase
	URI string `json:"uri"`
}

type skillsGetResult struct {
	mcp.ResultBase
	Skill skillEntry `json:"skill"`
}

type skillEntry struct {
	URI         string            `json:"uri"`
	Frontmatter map[string]string `json:"frontmatter"`
	Resources   []struct {
		URI    string `json:"uri"`
		Digest string `json:"digest"`
		Size   int    `json:"size"`
	} `json:"resources"`
}

func TestSkill(t *testing.T) {
	e := newEnv(t)
	e.opts.MCPURL = "https://mcp.art.test/"
	cs := e.connectWith(t, member, nil)

	if _, ok := cs.InitializeResult().Capabilities.Extensions["io.modelcontextprotocol/skills"]; !ok {
		t.Errorf("skills extension not declared: %+v", cs.InitializeResult().Capabilities)
	}
	if !strings.Contains(cs.InitializeResult().Instructions, "skill://kazari/SKILL.md") {
		t.Errorf("instructions do not point to the skill")
	}

	rr, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "skill://kazari/SKILL.md"})
	if err != nil {
		t.Fatal(err)
	}
	md := rr.Contents[0].Text
	if rr.Contents[0].MIMEType != "text/markdown" || md != mcpserver.Skill(e.deps.Links, e.opts) {
		t.Fatalf("skill resource differs from mcpserver.Skill")
	}
	for _, s := range []string{"---\nname: kazari\ndescription: \"Publish, edit and retire", "\n---\n\n# Kazari\n", "art.test", "edit_file",
		"anchors: true", "https://cdn.jsdelivr.net", "25 MiB", "claude mcp add --transport http kazari https://mcp.art.test/",
		"| `version_conflict` | Someone published"} {
		if !strings.Contains(md, s) {
			t.Errorf("skill lacks %q", s)
		}
	}

	list, err := mcp.CallCustomMethod[*skillsListParams, *skillsListResult](e.ctx, cs, "skills/list", &skillsListParams{})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(md))
	if len(list.Skills) != 1 || list.TTLMs != 300000 || list.CacheScope != "private" {
		t.Fatalf("skills/list = %+v", list)
	}
	s := list.Skills[0]
	if s.URI != "skill://kazari/SKILL.md" || s.Frontmatter["name"] != "kazari" || !strings.HasPrefix(s.Frontmatter["description"], "Publish") ||
		len(s.Resources) != 1 || s.Resources[0].Digest != "sha256:"+hex.EncodeToString(sum[:]) || s.Resources[0].Size != len(md) {
		t.Errorf("skills/list entry = %+v", s)
	}
	got, err := mcp.CallCustomMethod[*skillsGetParams, *skillsGetResult](e.ctx, cs, "skills/get", &skillsGetParams{URI: s.URI})
	if err != nil || got.Skill.Resources[0].Digest != s.Resources[0].Digest {
		t.Errorf("skills/get = %+v, %v", got, err)
	}
	_, err = mcp.CallCustomMethod[*skillsGetParams, *skillsGetResult](e.ctx, cs, "skills/get", &skillsGetParams{URI: "skill://other/SKILL.md"})
	var werr *jsonrpc.Error
	if !errors.As(err, &werr) || werr.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("skills/get of an unknown skill: err = %v", err)
	}

	res := call(t, cs, "get_guide", map[string]any{})
	if res.IsError || text(res) != md || res.StructuredContent != nil {
		t.Errorf("get_guide = %q, structured %v", text(res), res.StructuredContent)
	}
}

func TestPreview(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	tools, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	linked := map[string]bool{}
	for _, tool := range tools.Tools {
		if ui, ok := tool.Meta["ui"].(map[string]any); ok && ui["resourceUri"] == "ui://kazari/preview.html" {
			linked[tool.Name] = true
		}
	}
	for _, name := range []string{"create_artifact", "update_artifact", "edit_file", "publish_version"} {
		if !linked[name] {
			t.Errorf("%s is not linked to the preview", name)
		}
	}
	if len(linked) != 4 {
		t.Errorf("tools linked to the preview = %v", linked)
	}

	read := func(cs *mcp.ClientSession) (*mcp.ResourceContents, map[string]any) {
		t.Helper()
		rr, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "ui://kazari/preview.html"})
		if err != nil {
			t.Fatal(err)
		}
		c := rr.Contents[0]
		ui, _ := c.Meta["ui"].(map[string]any)
		return c, ui
	}
	c, ui := read(cs)
	if c.MIMEType != "text/html;profile=mcp-app" || !strings.Contains(c.Text, `"embed":false`) ||
		!strings.Contains(c.Text, `"versionURL":"https://{slug}--v{version}.art.test/"`) || ui["prefersBorder"] != true || ui["csp"] != nil {
		t.Errorf("preview with frame-ancestors 'none' = %s %v", c.MIMEType, ui)
	}
	if strings.Contains(c.Text, "KAZARI_CONFIG") {
		t.Errorf("preview config placeholder not replaced")
	}

	e.opts.CSP = "default-src 'self'; frame-ancestors https://*.example-host.test"
	c, ui = read(e.connect(t, member))
	frames, _ := json.Marshal(ui["csp"])
	if !strings.Contains(c.Text, `"embed":true`) || string(frames) != `{"frameDomains":["https://*.art.test"]}` {
		t.Errorf("preview with framing allowed: csp %s", frames)
	}
}

func TestHardDeleteConfirmation(t *testing.T) {
	e := newEnv(t)
	answer := map[string]any{}
	asked := 0
	cs := e.connectWith(t, member, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked++
			if !strings.Contains(req.Params.Message, `"demo"`) {
				t.Errorf("question = %q", req.Params.Message)
			}
			if answer["action"] != "accept" {
				return &mcp.ElicitResult{Action: answer["action"].(string)}, nil
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": answer["confirm"]}}, nil
		},
	})
	create := func(cs *mcp.ClientSession) {
		ok[api.PublishResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
			"files": []map[string]any{{"path": "index.html", "content": "x"}}})
	}
	create(cs)

	for _, a := range []map[string]any{{"action": "decline"}, {"action": "accept", "confirm": false}} {
		answer = a
		out, res := ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "demo", "hard": true})
		if out.OK || !strings.HasPrefix(firstLine(res), "Not purged") {
			t.Errorf("%v: delete = %+v %q", a, out, firstLine(res))
		}
	}
	ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo"})

	before := asked
	answer = map[string]any{"action": "accept", "confirm": true}
	if out, _ := ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "demo"}); !out.OK || asked != before {
		t.Errorf("soft delete = %+v, asked %d times", out, asked-before)
	}
	out, res := ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "demo", "hard": true})
	if !out.OK || firstLine(res) != "Purged demo permanently." || asked != before+1 {
		t.Errorf("confirmed purge = %+v %q, asked %d", out, firstLine(res), asked-before)
	}

	plain := e.connect(t, member)
	ok[api.PublishResult](t, plain, "create_artifact", map[string]any{"slug": "other", "name": "x", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}})
	if out, _ := ok[api.OK](t, plain, "delete_artifact", map[string]any{"slug": "other", "hard": true}); !out.OK {
		t.Errorf("purge without elicitation = %+v", out)
	}
}

func TestTextsUsePublicLinks(t *testing.T) {
	e := newEnv(t)
	e.deps.Links.URL = func(sub string) string { return "http://" + sub + ".localhost:8080/" }
	e.opts.BaseDomain = "localhost"
	e.opts.CSP = ""
	cs := e.connect(t, member)
	if in := cs.InitializeResult().Instructions; !strings.Contains(in, "http://<slug>.localhost:8080/") {
		t.Errorf("instructions = %q", in)
	}
	if md := mcpserver.Skill(e.deps.Links, e.opts); !strings.Contains(md, "http://<slug>--v<N>.localhost:8080/") {
		t.Errorf("skill lacks the development URL shape")
	}
	rr, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "ui://kazari/preview.html"})
	if err != nil {
		t.Fatal(err)
	}
	c := rr.Contents[0]
	frames, _ := json.Marshal(c.Meta["ui"].(map[string]any)["csp"])
	if !strings.Contains(c.Text, `"versionURL":"http://{slug}--v{version}.localhost:8080/"`) ||
		string(frames) != `{"frameDomains":["http://*.localhost:8080"]}` {
		t.Errorf("preview versionURL or frames wrong: %s", frames)
	}
}
