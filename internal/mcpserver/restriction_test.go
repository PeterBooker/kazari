package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

var narrow = auth.Principal{Handle: "alice", Scopes: auth.MemberScopes, Source: auth.SourceAPIKey, KeyID: "abcdefgh",
	Slugs: []string{"mine-*"}}

const restrictedTo = "forbidden: this credential is restricted to slugs matching mine-*"

// slugTool is a call of a tool that takes a slug, valid in everything but
// the slug. creates says the tool makes the artifact; wantErr is the error
// code a slug the credential reaches ends in, "" for success.
type slugTool struct {
	args    map[string]any
	creates bool
	wantErr string
}

// slugTools has a case for every tool whose input has a slug.
// TestRestrictionOnEveryTool fails for a tool that is missing here, so a
// new tool cannot go without the check.
func slugTools(pageURL string) map[string]slugTool {
	file := []map[string]any{{"path": "index.html", "content": "<h1>changed</h1>"}}
	return map[string]slugTool{
		"create_artifact":   {args: map[string]any{"name": "n", "description": "d", "files": file}, creates: true},
		"import_page":       {args: map[string]any{"url": pageURL}, creates: true},
		"update_artifact":   {args: map[string]any{"files": file, "name": "renamed"}},
		"edit_file":         {args: map[string]any{"path": "index.html", "edits": []map[string]any{{"old_text": "<h1>", "new_text": "<h2>"}}}},
		"get_artifact":      {args: map[string]any{"include_files": true}},
		"read_file":         {args: map[string]any{"path": "index.html", "anchors": true}},
		"publish_version":   {args: map[string]any{"version": 1}},
		"mark_stale":        {args: map[string]any{"grace": "72h"}},
		"unmark_stale":      {args: map[string]any{}},
		"set_visibility":    {args: map[string]any{"visibility": "unlisted"}},
		"create_share_link": {args: map[string]any{"label": "x"}},
		"list_share_links":  {args: map[string]any{}},
		"revoke_share_link": {args: map[string]any{"prefix": "aaaaaaaa"}, wantErr: api.CodeLinkNotFound},
		"delete_artifact":   {args: map[string]any{"hard": true}},
	}
}

func withSlug(args map[string]any, slug string) map[string]any {
	out := map[string]any{"slug": slug}
	maps.Copy(out, args)
	return out
}

func TestRestrictionOnEveryTool(t *testing.T) {
	e := newEnv(t)
	fetched := 0
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched++
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<title>Source</title><h1>hi</h1>")
	}))
	t.Cleanup(page.Close)
	alice := artifact.Actor{Handle: "alice"}
	seed := func(slug string) {
		t.Helper()
		if _, _, err := e.deps.Artifacts.Create(e.ctx, alice, artifact.CreateRequest{Slug: slug,
			Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("<h1>" + slug + "</h1>")}}}); err != nil {
			t.Fatal(err)
		}
	}
	seed("other-page")

	cs := e.connect(t, narrow)
	tools, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := slugTools(page.URL + "/")
	seen := 0
	for _, tool := range tools.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if _, takesSlug := schema.Properties["slug"]; !takesSlug {
			continue
		}
		seen++
		c, ok := cases[tool.Name]
		if !ok {
			t.Errorf("tool %s takes a slug but has no case in slugTools", tool.Name)
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			outside := "other-page"
			if c.creates {
				outside = "other-" + strings.ReplaceAll(tool.Name, "_", "-")
			}
			if msg := fails(t, cs, tool.Name, withSlug(c.args, outside)); !strings.HasPrefix(msg, restrictedTo) || !strings.Contains(msg, "Next step:") {
				t.Errorf("slug outside the restriction: %q", msg)
			}
			inside := "mine-" + strings.ReplaceAll(tool.Name, "_", "-")
			if !c.creates {
				seed(inside)
			}
			res := call(t, cs, tool.Name, withSlug(c.args, inside))
			switch {
			case c.wantErr == "" && res.IsError:
				t.Errorf("slug inside the restriction: %s", text(res))
			case c.wantErr != "" && !strings.HasPrefix(text(res), c.wantErr+":"):
				t.Errorf("slug inside the restriction: %q, want %s", text(res), c.wantErr)
			}
		})
	}
	if seen != len(cases) {
		t.Errorf("slugTools has %d cases for %d tools that take a slug", len(cases), seen)
	}
	if fetched != 1 {
		t.Errorf("the page was fetched %d times, want once: a refused import_page must not fetch", fetched)
	}

	a, vs, err := e.deps.Artifacts.Get(e.ctx, alice, "other-page")
	if err != nil || len(vs) != 1 || a.Name != "" || a.Visibility != artifact.Public || a.StaleAt != nil || a.DeletedAt != nil {
		t.Errorf("the refused calls changed the artifact: %+v, %d versions, %v", a, len(vs), err)
	}
	for _, slug := range []string{"other-create-artifact", "other-import-page"} {
		if _, _, err := e.deps.Artifacts.Get(e.ctx, alice, slug); artifact.CodeOf(err) != api.CodeNotFound {
			t.Errorf("a refused call created %s: %v", slug, err)
		}
	}
}

func TestRestrictionOnResourcesAndListing(t *testing.T) {
	e := newEnv(t)
	alice := artifact.Actor{Handle: "alice"}
	for _, slug := range []string{"mine-a", "other-a", "mine-b"} {
		if _, _, err := e.deps.Artifacts.Create(e.ctx, alice, artifact.CreateRequest{Slug: slug,
			Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("<h1>" + slug + "</h1>")}}}); err != nil {
			t.Fatal(err)
		}
	}
	cs := e.connect(t, narrow)

	for _, uri := range []string{"kazari://artifacts/other-a", "kazari://artifacts/other-a/files/index.html", "kazari://artifacts/missing-other"} {
		_, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: uri})
		if err == nil || !strings.Contains(err.Error(), "restricted to slugs matching mine-*") {
			t.Errorf("ReadResource(%s) err = %v, want the restriction's forbidden", uri, err)
		}
	}
	for _, uri := range []string{"kazari://artifacts/mine-a", "kazari://artifacts/mine-a/files/index.html"} {
		if _, err := cs.ReadResource(e.ctx, &mcp.ReadResourceParams{URI: uri}); err != nil {
			t.Errorf("ReadResource(%s): %v", uri, err)
		}
	}

	list, _ := ok[api.ArtifactList](t, cs, "list_artifacts", map[string]any{})
	var got []string
	for _, a := range list.Items {
		got = append(got, a.Slug)
	}
	if diff := cmp.Diff([]string{"mine-b", "mine-a"}, got); diff != "" {
		t.Errorf("list_artifacts (-want +got):\n%s", diff)
	}
}

func TestInstructionsNameTheRestriction(t *testing.T) {
	e := newEnv(t)
	p := narrow
	p.Slugs = []string{"weekly-*", "report"}
	got := e.connect(t, p).InitializeResult().Instructions
	plain := e.connect(t, member).InitializeResult().Instructions
	line, found := strings.CutPrefix(got, plain)
	if !found || !strings.HasPrefix(line, "\nThis connection is restricted to artifacts whose slug matches weekly-*, report") ||
		strings.Count(line, "\n") != 1 {
		t.Errorf("instructions of a restricted connection add %q, want one line naming the patterns", line)
	}
	if strings.Contains(plain, "restricted") {
		t.Errorf("instructions of an unrestricted connection mention a restriction: %s", plain)
	}
}

func TestShareScopeOnTools(t *testing.T) {
	e := newEnv(t)
	alice := artifact.Actor{Handle: "alice"}
	if _, _, err := e.deps.Artifacts.Create(e.ctx, alice, artifact.CreateRequest{Slug: "demo",
		Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("<h1>demo</h1>")}}}); err != nil {
		t.Fatal(err)
	}
	link, _, err := e.deps.Artifacts.CreateShareLink(e.ctx, alice, "demo", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<title>Source</title><h1>hi</h1>")
	}))
	t.Cleanup(page.Close)
	file := []map[string]any{{"path": "index.html", "content": "<h1>x</h1>"}}

	writer := auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete}, Source: auth.SourceAPIKey}
	cs := e.connect(t, writer)
	for _, name := range []string{"set_visibility", "create_share_link", "revoke_share_link"} {
		if offered(t, cs, name) {
			t.Errorf("%s is offered without the share scope", name)
		}
	}
	for name, args := range map[string]map[string]any{
		"create_artifact":   {"slug": "new-page", "name": "n", "description": "d", "visibility": "public", "files": file},
		"update_artifact":   {"slug": "demo", "visibility": "unlisted"},
		"import_page":       {"slug": "copy", "url": page.URL + "/", "visibility": "members"},
	} {
		if msg := fails(t, cs, name, args); !strings.HasPrefix(msg, `forbidden: scope "artifacts:share" required`) {
			t.Errorf("%s without the share scope: %q", name, msg)
		}
	}
	a, _, err := e.deps.Artifacts.Get(e.ctx, alice, "demo")
	if err != nil || a.Visibility != artifact.Public {
		t.Fatalf("a refused call changed the visibility: %+v, %v", a, err)
	}
	if links, err := e.deps.Artifacts.ListShareLinks(e.ctx, alice, "demo"); err != nil || len(links) != 1 || links[0].RevokedAt != nil {
		t.Errorf("a refused call changed the share links: %+v, %v", links, err)
	}

	// Without the scope the same calls go through once visibility is left
	// out or empty, which models send for an optional field.
	res, _ := ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{"slug": "new-page", "name": "n", "description": "d", "visibility": "", "files": file})
	if res.Visibility != "public" {
		t.Errorf("visibility of a new page = %q, want the instance default", res.Visibility)
	}
	ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "visibility": "", "name": "renamed"})
	ok[mcpserver.WriteResult](t, cs, "import_page", map[string]any{"slug": "copy", "url": page.URL + "/"})
	ok[api.ShareLinkList](t, cs, "list_share_links", map[string]any{"slug": "demo"})

	sharer := auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeShare}, Source: auth.SourceAPIKey}
	cs = e.connect(t, sharer)
	if res, _ := ok[mcpserver.WriteResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "members"}); res.Visibility != "members" {
		t.Errorf("set_visibility with the share scope = %+v", res)
	}
	ok[api.CreatedShareLink](t, cs, "create_share_link", map[string]any{"slug": "demo"})
	ok[api.OK](t, cs, "revoke_share_link", map[string]any{"slug": "demo", "prefix": link.Prefix})
	if offered(t, cs, "update_artifact") {
		t.Errorf("update_artifact is offered with the share scope alone")
	}
}

// No tool issues, rotates or revokes a key: an agent never mints a credential.
func TestNoToolTouchesKeys(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	tools, err := cs.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "key") || strings.Contains(tool.Name, "token") || strings.Contains(tool.Name, "grant") {
			t.Errorf("tool %s looks like it manages credentials", tool.Name)
		}
		raw, _ := json.Marshal(tool.OutputSchema)
		if strings.Contains(string(raw), fmt.Sprintf("%q", "key")) {
			t.Errorf("tool %s returns a field named key", tool.Name)
		}
	}
}
