package mcpserver_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

func pageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<!doctype html><html><head><title>Charity</title><link rel="stylesheet" href="/s.css"><script src="/t.js"></script></head>`+
				`<body><header class="top"><h1>Give</h1></header><main><p>`+strings.Repeat("words ", 50)+`</p><img src="img.png"><a href="/about">About</a></main></body></html>`)
		case "/s.css":
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, "body{background:url(bg.png)}")
		case "/img.png", "/bg.png":
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "PNG")
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestImportPage(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	srv := pageServer(t)

	res, raw := ok[api.PublishResult](t, cs, "import_page", map[string]any{"url": srv.URL + "/", "slug": "copy"})
	if res.Version != 1 || res.Artifact.Name != "Charity" || res.Import == nil {
		t.Fatalf("import = %+v", res)
	}
	imp := res.Import
	if imp.Source != srv.URL+"/" || imp.Fetched != 3 || imp.ScriptsRemoved != 1 || len(imp.Skipped) != 0 || len(imp.Notes) != 0 {
		t.Errorf("report = %+v", imp)
	}
	if len(imp.Outline) < 2 || imp.Outline[0].Tag != `<header class="top">` || imp.Outline[0].Heading != "Give" {
		t.Errorf("outline = %+v", imp.Outline)
	}
	if line := firstLine(raw); !strings.Contains(line, "Published https://copy.art.test/ (version 1) from "+srv.URL+"/: 4 files, 1 scripts removed.") {
		t.Errorf("summary = %q", line)
	}
	rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "copy", "path": "index.html"})
	if strings.Contains(rf.Content, "<script") || !strings.Contains(rf.Content, `href="assets/`) || !strings.Contains(rf.Content, `href="`+srv.URL+`/about"`) {
		t.Errorf("copy = %s", rf.Content)
	}

	if msg := toolErr(t, cs, "import_page", map[string]any{"url": srv.URL + "/missing", "slug": "copy-2"}); !strings.Contains(msg, "fetch_failed") ||
		!strings.Contains(msg, "404") || !strings.Contains(msg, "Next step") {
		t.Errorf("error = %q", msg)
	}
	if msg := toolErr(t, cs, "get_artifact", map[string]any{"slug": "copy-2"}); !strings.Contains(msg, "not_found") {
		t.Errorf("failed import left an artifact: %q", msg)
	}
}

// toolErr calls a tool that must fail and returns the error text.
func toolErr(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s succeeded: %s", name, text(res))
	}
	return text(res)
}

func TestFilesFromURL(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	srv := pageServer(t)

	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>hi</h1>"}, {"path": "data.json", "url": srv.URL + "/data.json"}}})
	rf, _ := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "data.json"})
	if rf.Content != `{"ok":true}` || rf.ContentType != "application/json" {
		t.Errorf("fetched file = %+v", rf)
	}

	ok[api.PublishResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "expected_version": 1,
		"files": []map[string]any{{"path": "s.css", "url": srv.URL + "/s.css", "content_type": "text/css; charset=utf-8"}}})
	rf, _ = ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "s.css"})
	if rf.Content != "body{background:url(bg.png)}" || rf.ContentType != "text/css; charset=utf-8" || rf.Version != 2 {
		t.Errorf("updated file = %+v", rf)
	}

	if msg := toolErr(t, cs, "update_artifact", map[string]any{"slug": "demo",
		"files": []map[string]any{{"path": "x.json", "url": srv.URL + "/missing"}}}); !strings.Contains(msg, "fetch_failed") {
		t.Errorf("missing url: %q", msg)
	}
	if msg := toolErr(t, cs, "update_artifact", map[string]any{"slug": "demo",
		"files": []map[string]any{{"path": "x.json", "url": srv.URL + "/data.json", "content": "{}"}}}); !strings.Contains(msg, "both url and content") {
		t.Errorf("url with content: %q", msg)
	}
	d, _ := ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo"})
	if d.Artifact.LatestVersion != 2 {
		t.Errorf("failed updates published: latest %d", d.Artifact.LatestVersion)
	}
}

func TestImportToolOnlyWhenFetchIsOn(t *testing.T) {
	e := newEnv(t)
	e.opts.Fetch = false
	cs := e.connect(t, member)
	tools, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "import_page" {
			t.Error("import_page offered with fetch off")
		}
	}
	if md := mcpserver.Skill(e.deps.Links, e.opts); strings.Contains(md, "import_page") || strings.Contains(md, "fetch_failed") {
		t.Error("skill mentions import with fetch off")
	}
	if md := mcpserver.Skill(e.deps.Links, exampleOptions()); !strings.Contains(md, "**Copy an existing page:**") || !strings.Contains(md, "`fetch_failed`") {
		t.Error("default skill lacks the import step")
	}
}
