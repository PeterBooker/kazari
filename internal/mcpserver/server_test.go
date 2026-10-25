package mcpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kazari/internal/api"
	"kazari/internal/artifact"
	"kazari/internal/auth"
	"kazari/internal/blob"
	"kazari/internal/mcpserver"
	"kazari/internal/present"
	"kazari/internal/ratelimit"
	"kazari/internal/sqlite"
	"kazari/internal/upload"
)

const testCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'"

var (
	member   = auth.Principal{Handle: "alice", Scopes: auth.MemberScopes, Source: auth.SourceAPIKey}
	readOnly = auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead}, Source: auth.SourceAPIKey}

	// pngBytes is not valid UTF-8.
	pngBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0xff, 0xfe}
)

type env struct {
	ctx  context.Context
	deps mcpserver.Deps
	opts mcpserver.Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES ('alice', 'member', 0, ?)`,
		time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	reserved := []string{"api", "mcp", "admin"}
	arts := artifact.New(sqlite.NewArtifactStore(db), blobs, nil, artifact.Options{ReservedSlugs: reserved})
	ups := upload.New(sqlite.NewUploadStore(db), upload.Options{Dir: filepath.Join(dir, "uploads"), MaxChunkBytes: 1 << 20})
	return &env{
		ctx: ctx,
		deps: mcpserver.Deps{
			Artifacts: arts,
			Uploads:   ups,
			Logger:    slog.New(slog.DiscardHandler),
			Links:     present.Links{URL: func(sub string) string { return "https://" + sub + ".art.test/" }},
		},
		opts: mcpserver.Options{
			BaseDomain:    "art.test",
			CSP:           testCSP,
			ReservedSlugs: reserved,
			APIBaseURL:    "https://api.art.test/v1",
		},
	}
}

func (e *env) connect(t *testing.T, p auth.Principal) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := mcpserver.NewServer(e.deps, e.opts, p).Connect(e.ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(e.ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func ok[Out any](t *testing.T, cs *mcp.ClientSession, name string, args any) (Out, *mcp.CallToolResult) {
	t.Helper()
	res := call(t, cs, name, args)
	var out Out
	if res.IsError {
		t.Fatalf("%s failed: %s", name, text(res))
	}
	js, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(js, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, js, err)
	}
	return out, res
}

func fails(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res := call(t, cs, name, args)
	if !res.IsError {
		t.Fatalf("%s succeeded, want an error: %s", name, text(res))
	}
	return text(res)
}

func firstLine(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	tc, _ := res.Content[0].(*mcp.TextContent)
	if tc == nil {
		return ""
	}
	return tc.Text
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestListTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	res, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"begin_upload", "create_artifact", "delete_artifact", "edit_file", "get_artifact", "get_guide",
		"list_artifacts", "mark_stale", "publish_version", "read_file", "set_visibility", "unmark_stale", "update_artifact",
		"upload_chunk"}
	var got []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		byName[tool.Name] = tool
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", tool.Name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	create := byName["create_artifact"].Description
	for _, s := range []string{"https://<slug>.art.test/", "3 to 63", "\"--\"", "api, mcp, admin", "25 MiB", "50 MiB",
		"2000 files", "index.html", "relative links", "https://cdn.jsdelivr.net", "https://fonts.gstatic.com",
		"kazari.spa", "kazari.indexable", "kazari.csp", "2 MiB", "begin_upload"} {
		if !strings.Contains(create, s) {
			t.Errorf("create_artifact description lacks %q", s)
		}
	}
	if d := byName["update_artifact"].Description; !strings.Contains(d, "version_conflict") || !strings.Contains(d, "latest_version") {
		t.Errorf("update_artifact description lacks the expected_version rule")
	}
	chunk := byName["upload_chunk"].Description
	for _, s := range []string{"offset 0", "bytes_received", "current=", "1 MiB", "base64"} {
		if !strings.Contains(chunk, s) {
			t.Errorf("upload_chunk description lacks %q", s)
		}
	}
	if d := byName["set_visibility"].Description; !strings.Contains(d, "unpublished") {
		t.Errorf("set_visibility description lacks unpublished")
	}
	if a := byName["get_artifact"].Annotations; !a.ReadOnlyHint {
		t.Errorf("get_artifact is not read-only")
	}
	if a := byName["delete_artifact"].Annotations; a.DestructiveHint == nil || !*a.DestructiveHint || !a.IdempotentHint {
		t.Errorf("delete_artifact annotations = %+v", a)
	}
}

func TestToolsRoundTrip(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	const html = "<!doctype html><title>Demo</title><img src=\"assets/logo.png\">"

	created, res := ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug":        "demo",
		"name":        "Demo",
		"description": "A demo page",
		"metadata":    map[string]string{"team": "docs"},
		"label":       "first",
		"files": []map[string]any{
			{"path": "index.html", "content": html},
			{"path": "assets/logo.png", "content": b64(pngBytes), "encoding": "base64"},
		},
	})
	if created.URL != "https://demo.art.test/" || created.Version != 1 || created.Artifact.Owner != "alice" {
		t.Fatalf("create = %+v", created)
	}
	if got := firstLine(res); got != "Published https://demo.art.test/ (version 1)" {
		t.Errorf("create summary = %q", got)
	}

	detail, _ := ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo", "include_files": true})
	if len(detail.Versions) != 1 || len(detail.Versions[0].Files) != 2 {
		t.Fatalf("get = %+v", detail)
	}
	if f := detail.Versions[0].Files[0]; f.Path != "assets/logo.png" || f.ContentType != "image/png" || f.Hash != sha(pngBytes) {
		t.Errorf("first file = %+v", f)
	}

	rf, res := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "index.html"})
	if rf.Encoding != "utf8" || rf.Size != int64(len(html)) || text(res) != html {
		t.Errorf("read_file index.html = %+v, %q", rf, text(res))
	}
	rf, res = ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "demo", "path": "assets/logo.png", "version": 1})
	if rf.Encoding != "base64" || rf.ContentType != "image/png" {
		t.Errorf("read_file logo = %+v", rf)
	}
	er, isRes := res.Content[0].(*mcp.EmbeddedResource)
	if !isRes || !bytes.Equal(er.Resource.Blob, pngBytes) || er.Resource.URI != "kazari://artifacts/demo/files/assets/logo.png" {
		t.Errorf("read_file logo content = %#v", res.Content[0])
	}

	list, _ := ok[api.ArtifactList](t, cs, "list_artifacts", map[string]any{"mine": "created"})
	if len(list.Items) != 1 || list.Items[0].Slug != "demo" {
		t.Errorf("list = %+v", list)
	}
	list, _ = ok[api.ArtifactList](t, cs, "list_artifacts", map[string]any{"metadata": map[string]string{"team": "other"}})
	if len(list.Items) != 0 {
		t.Errorf("list by other team = %+v", list)
	}

	updated, _ := ok[api.PublishResult](t, cs, "update_artifact", map[string]any{
		"slug":             "demo",
		"expected_version": 1,
		"metadata":         map[string]any{"team": nil, "topic": "otters"},
		"files": []map[string]any{
			{"path": "assets/logo.png", "delete": true},
			{"path": "about.html", "content": "<p>about</p>"},
		},
	})
	if updated.Version != 2 || updated.Artifact.LatestVersion != 2 || updated.Artifact.Metadata["topic"] != "otters" {
		t.Fatalf("update = %+v", updated)
	}
	if _, has := updated.Artifact.Metadata["team"]; has {
		t.Errorf("metadata team not deleted: %v", updated.Artifact.Metadata)
	}
	msg := fails(t, cs, "update_artifact", map[string]any{
		"slug": "demo", "expected_version": 1,
		"files": []map[string]any{{"path": "index.html", "content": "late"}},
	})
	if !strings.Contains(msg, "version_conflict") {
		t.Errorf("conflict error = %q", msg)
	}
	detail, _ = ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo", "include_files": true})
	var paths []string
	for _, f := range detail.Versions[0].Files {
		paths = append(paths, f.Path)
	}
	if detail.Versions[0].Number != 2 || !slices.Equal(paths, []string{"about.html", "index.html"}) {
		t.Errorf("v2 files = %v (version %d)", paths, detail.Versions[0].Number)
	}

	rolled, res := ok[api.PublishResult](t, cs, "publish_version", map[string]any{"slug": "demo", "version": 1})
	if rolled.Version != 1 || rolled.Artifact.PublishedVersion != 1 || rolled.Artifact.LatestVersion != 2 {
		t.Errorf("publish_version = %+v", rolled)
	}
	if got := firstLine(res); got != "Published https://demo.art.test/ (version 1)" {
		t.Errorf("publish_version summary = %q", got)
	}
	if msg := fails(t, cs, "publish_version", map[string]any{"slug": "demo", "version": 9}); !strings.HasPrefix(msg, "version_not_found: ") {
		t.Errorf("publish missing version = %q", msg)
	}

	stale, _ := ok[api.StaleResult](t, cs, "mark_stale", map[string]any{"slug": "demo", "grace": "3d"})
	if got := stale.ExpiresAt.Sub(stale.StaleAt); got != 72*time.Hour {
		t.Errorf("stale grace = %v", got)
	}
	unstale, _ := ok[api.PublishResult](t, cs, "unmark_stale", map[string]any{"slug": "demo"})
	if unstale.Artifact.StaleAt != nil || unstale.Artifact.ExpiresAt != nil {
		t.Errorf("unmark_stale = %+v", unstale.Artifact)
	}

	off, res := ok[api.PublishResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "unpublished"})
	if off.Artifact.UnpublishedAt == nil || !strings.HasPrefix(firstLine(res), "Unpublished https://demo.art.test/") {
		t.Errorf("unpublish = %+v, %q", off.Artifact, firstLine(res))
	}
	on, _ := ok[api.PublishResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "public"})
	if on.Artifact.UnpublishedAt != nil || on.Artifact.Visibility != "public" {
		t.Errorf("republish = %+v", on.Artifact)
	}

	if done, _ := ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "demo"}); !done.OK {
		t.Errorf("soft delete = %+v", done)
	}
	detail, _ = ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo"})
	if detail.Artifact.DeletedAt == nil {
		t.Errorf("soft-deleted artifact has no deleted_at")
	}
	if done, _ := ok[api.OK](t, cs, "delete_artifact", map[string]any{"slug": "demo", "hard": true}); !done.OK {
		t.Errorf("hard delete = %+v", done)
	}
	if msg := fails(t, cs, "get_artifact", map[string]any{"slug": "demo"}); !strings.HasPrefix(msg, "gone: ") {
		t.Errorf("get after purge = %q", msg)
	}
}

func TestToolErrors(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"create_artifact", map[string]any{"slug": "api", "name": "x", "description": "x",
			"files": []map[string]any{{"path": "index.html", "content": "x"}}}, "invalid_slug: "},
		{"create_artifact", map[string]any{"slug": "bad", "name": "x", "description": "x",
			"files": []map[string]any{{"path": "a.bin", "content": "%%%", "encoding": "base64"}}}, "invalid_argument: "},
		{"create_artifact", map[string]any{"slug": "bad", "name": "x", "description": "x",
			"files": []map[string]any{{"path": "../x.html", "content": "x"}}}, "invalid_path: "},
		{"get_artifact", map[string]any{"slug": "nope"}, "not_found: "},
		{"mark_stale", map[string]any{"slug": "nope", "grace": "soon"}, "invalid_argument: "},
		{"set_visibility", map[string]any{"slug": "nope", "visibility": "secret"}, "invalid_argument: "},
		{"upload_chunk", map[string]any{"upload_id": "missing", "path": "a", "offset": 0, "content": "x"}, "upload_not_found: "},
	}
	for _, c := range cases {
		if msg := fails(t, cs, c.tool, c.args); !strings.HasPrefix(msg, c.want) {
			t.Errorf("%s %v = %q, want prefix %q", c.tool, c.args, msg, c.want)
		}
	}
}

func TestReadOnlyPrincipal(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, readOnly)
	msg := fails(t, cs, "create_artifact", map[string]any{"slug": "demo", "name": "x", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "x"}}})
	if first, next, _ := strings.Cut(msg, "\n"); first != `forbidden: scope "artifacts:write" required` ||
		!strings.HasPrefix(next, "Next step: Retrying will not help.") {
		t.Errorf("create as reader = %q", msg)
	}
	if msg := fails(t, cs, "delete_artifact", map[string]any{"slug": "demo"}); !strings.Contains(msg, `scope "artifacts:delete" required`) {
		t.Errorf("delete as reader = %q", msg)
	}
	if msg := fails(t, cs, "get_artifact", map[string]any{"slug": "demo"}); !strings.HasPrefix(msg, "not_found: ") {
		t.Errorf("get as reader = %q", msg)
	}
}

func TestChunkedPublish(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)

	data := make([]byte, 3<<20)
	r := rand.NewChaCha8([32]byte{1})
	r.Read(data)

	up, res := ok[api.Upload](t, cs, "begin_upload", map[string]any{})
	if up.UploadID == "" || up.MaxChunkBytes != 1<<20 || up.MaxTotalBytes != upload.DefaultMaxTotalBytes {
		t.Fatalf("begin_upload = %+v", up)
	}
	if !strings.Contains(firstLine(res), up.UploadID) {
		t.Errorf("begin_upload summary = %q", firstLine(res))
	}

	chunk := func(offset int64, b []byte) map[string]any {
		return map[string]any{"upload_id": up.UploadID, "path": "data.bin", "offset": offset,
			"content": b64(b), "encoding": "base64", "content_type": "application/octet-stream"}
	}
	const n = 1 << 20
	got, _ := ok[api.ChunkResult](t, cs, "upload_chunk", chunk(0, data[:n]))
	if got.BytesReceived != n {
		t.Fatalf("first chunk = %+v", got)
	}
	msg := fails(t, cs, "upload_chunk", chunk(12345, data[n:2*n]))
	if !strings.HasPrefix(msg, "upload_offset_mismatch: ") || !strings.Contains(msg, "current=1048576") {
		t.Fatalf("mismatch = %q", msg)
	}
	if got, _ := ok[api.ChunkResult](t, cs, "upload_chunk", chunk(0, data[:n])); got.BytesReceived != n {
		t.Fatalf("retry = %+v", got)
	}
	for off := int64(n); off < int64(len(data)); off += n {
		got, _ := ok[api.ChunkResult](t, cs, "upload_chunk", chunk(off, data[off:off+n]))
		if got.BytesReceived != off+n {
			t.Fatalf("chunk at %d = %+v", off, got)
		}
	}
	ok[api.ChunkResult](t, cs, "upload_chunk", map[string]any{"upload_id": up.UploadID, "path": "index.html",
		"offset": 0, "content": "staged"})

	pub, _ := ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug": "bundle", "name": "Bundle", "description": "A large bundle", "upload_id": up.UploadID,
		"files": []map[string]any{{"path": "index.html", "content": "inline wins"}},
	})
	if pub.Version != 1 || pub.Artifact.SizeBytes != int64(len(data))+int64(len("inline wins")) {
		t.Fatalf("create from upload = %+v", pub)
	}

	rf, res := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "bundle", "path": "data.bin"})
	if rf.Size != int64(len(data)) || rf.Hash != sha(data) || rf.Encoding != "url" ||
		rf.URL != "https://api.art.test/v1/artifacts/bundle/files/data.bin?version=1" {
		t.Errorf("read_file data.bin = %+v", rf)
	}
	if !strings.Contains(text(res), rf.URL) {
		t.Errorf("read_file text = %q", text(res))
	}
	if _, res := ok[mcpserver.ReadFileResult](t, cs, "read_file", map[string]any{"slug": "bundle", "path": "index.html"}); text(res) != "inline wins" {
		t.Errorf("index.html = %q", text(res))
	}

	if msg := fails(t, cs, "upload_chunk", chunk(0, data[:10])); !strings.HasPrefix(msg, "upload_not_found: ") {
		t.Errorf("chunk after publish = %q", msg)
	}
}

func TestUploadCleanedUpOnFailure(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	up, _ := ok[api.Upload](t, cs, "begin_upload", map[string]any{})
	ok[api.ChunkResult](t, cs, "upload_chunk", map[string]any{"upload_id": up.UploadID, "path": "index.html", "offset": 0, "content": "x"})
	if msg := fails(t, cs, "create_artifact", map[string]any{"slug": "admin", "name": "x", "description": "x", "upload_id": up.UploadID}); !strings.HasPrefix(msg, "invalid_slug: ") {
		t.Fatalf("create = %q", msg)
	}
	if msg := fails(t, cs, "create_artifact", map[string]any{"slug": "fine", "name": "x", "description": "x", "upload_id": up.UploadID}); !strings.HasPrefix(msg, "upload_not_found: ") {
		t.Errorf("reuse after failure = %q", msg)
	}

	up, _ = ok[api.Upload](t, cs, "begin_upload", map[string]any{})
	ok[api.ChunkResult](t, cs, "upload_chunk", map[string]any{"upload_id": up.UploadID, "path": "index.html", "offset": 0, "content": "x"})
	if msg := fails(t, cs, "create_artifact", map[string]any{"slug": "fine", "name": "x", "description": "x", "upload_id": up.UploadID,
		"files": []map[string]any{{"path": "a.bin", "content": "!!", "encoding": "base64"}}}); !strings.HasPrefix(msg, "invalid_argument: ") {
		t.Fatalf("create with bad inline file = %q", msg)
	}
	if msg := fails(t, cs, "upload_chunk", map[string]any{"upload_id": up.UploadID, "path": "index.html", "offset": 1, "content": "y"}); !strings.HasPrefix(msg, "upload_not_found: ") {
		t.Errorf("chunk after failed create = %q", msg)
	}
}

func TestResourcesAndPrompt(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "d",
		"files": []map[string]any{
			{"path": "index.html", "content": "<h1>hi</h1>"},
			{"path": "assets/logo.png", "content": b64(pngBytes), "encoding": "base64"},
		},
	})

	tmpls, err := cs.ListResourceTemplates(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, rt := range tmpls.ResourceTemplates {
		uris = append(uris, rt.URITemplate)
	}
	slices.Sort(uris)
	if !slices.Equal(uris, []string{"kazari://artifacts/{slug}", "kazari://artifacts/{slug}/files/{+path}"}) {
		t.Errorf("templates = %v", uris)
	}

	rr, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "kazari://artifacts/demo"})
	if err != nil {
		t.Fatal(err)
	}
	var d api.ArtifactDetail
	if c := rr.Contents[0]; c.MIMEType != "application/json" || json.Unmarshal([]byte(c.Text), &d) != nil {
		t.Fatalf("artifact resource = %+v", c)
	}
	if d.Artifact.Slug != "demo" || len(d.Versions) != 1 || len(d.Versions[0].Files) != 2 {
		t.Errorf("artifact resource detail = %+v", d)
	}

	rr, err = cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "kazari://artifacts/demo/files/assets/logo.png"})
	if err != nil {
		t.Fatal(err)
	}
	if c := rr.Contents[0]; c.MIMEType != "image/png" || !bytes.Equal(c.Blob, pngBytes) || c.Text != "" {
		t.Errorf("logo resource = %+v", c)
	}
	rr, err = cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "kazari://artifacts/demo/files/index.html"})
	if err != nil {
		t.Fatal(err)
	}
	if c := rr.Contents[0]; !strings.HasPrefix(c.MIMEType, "text/html") || c.Text != "<h1>hi</h1>" {
		t.Errorf("index resource = %+v", c)
	}
	if _, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: "kazari://artifacts/demo/files/missing.html"}); err == nil {
		t.Errorf("missing file resource: no error")
	}

	pr, err := cs.GetPrompt(e.ctx, &mcp.GetPromptParams{Name: "publish-page", Arguments: map[string]string{"topic": "sea otters"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Messages) != 1 || pr.Messages[0].Role != "user" {
		t.Fatalf("prompt = %+v", pr)
	}
	body := pr.Messages[0].Content.(*mcp.TextContent).Text
	for _, s := range []string{"sea otters", "index.html", "relative links", "https://cdnjs.cloudflare.com",
		"https://cdn.jsdelivr.net", "https://unpkg.com", "https://fonts.googleapis.com", "No server-side code",
		"begin_upload", "2 MiB", "slug", "report the url"} {
		if !strings.Contains(body, s) {
			t.Errorf("prompt lacks %q", s)
		}
	}
}

func TestWriteRateLimit(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(0, 0)
	e.deps.Writes = ratelimit.New(1, func() time.Time { return now })
	p := member
	p.KeyID = "abcd1234"
	cs := e.connect(t, p)
	ok[api.Upload](t, cs, "begin_upload", map[string]any{})
	if msg := fails(t, cs, "begin_upload", map[string]any{}); msg != "rate_limited: too many writes; retry in 60s\n"+
		"Next step: Wait as long as the message says, then repeat the call." {
		t.Errorf("second write = %q", msg)
	}
	if msg := fails(t, cs, "delete_artifact", map[string]any{"slug": "nope"}); !strings.HasPrefix(msg, "rate_limited: ") {
		t.Errorf("delete over the limit = %q", msg)
	}
	ok[api.ArtifactList](t, cs, "list_artifacts", map[string]any{})
	other := member
	other.KeyID = "efgh5678"
	ok[api.Upload](t, e.connect(t, other), "begin_upload", map[string]any{})
}

func TestToolPanicRecovered(t *testing.T) {
	e := newEnv(t)
	e.deps.Uploads = nil // begin_upload now dereferences a nil service
	cs := e.connect(t, member)
	if msg := fails(t, cs, "begin_upload", map[string]any{}); !strings.HasPrefix(msg, "internal: internal error\nNext step: ") {
		t.Errorf("panicking tool = %q", msg)
	}
	ok[api.ArtifactList](t, cs, "list_artifacts", map[string]any{})
}
