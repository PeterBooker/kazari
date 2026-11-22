package mcpserver_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

// plainPasswords stands in for user.Service's hashing.
type plainPasswords struct{}

func (plainPasswords) HashPassword(_ context.Context, password string) (string, error) {
	return "hash:" + password, nil
}

func (plainPasswords) VerifyPassword(_ context.Context, hash, password string) (bool, error) {
	return hash == "hash:"+password, nil
}

func TestShareLinkPasswordTool(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "team", "name": "Team", "description": "x", "visibility": "members",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>team</h1>"}},
	})
	created, res := ok[mcpserver.ShareLinkResult](t, cs, "create_share_link", map[string]any{"slug": "team", "password": "open sesame"})
	if !created.Password || !strings.Contains(firstLine(res), "and the password") || strings.Contains(text(res), "open sesame") {
		t.Errorf("create_share_link with a password = %+v, %q; want password true, said once, never repeated", created, firstLine(res))
	}
	if msg := fails(t, cs, "create_share_link", map[string]any{"slug": "team", "password": "short"}); !strings.HasPrefix(msg, "invalid_argument: ") {
		t.Errorf("create_share_link with a short password = %q, want invalid_argument", msg)
	}
	list, _ := ok[api.ShareLinkList](t, cs, "list_share_links", map[string]any{"slug": "team"})
	if len(list.Items) != 1 || !list.Items[0].Password {
		t.Errorf("list_share_links = %+v, want one link with password true", list.Items)
	}
}

func TestShareLinkTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "team", "name": "Team", "description": "x", "visibility": "members",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>team</h1>"}},
	})

	created, res := ok[mcpserver.ShareLinkResult](t, cs, "create_share_link", map[string]any{
		"slug": "team", "label": "Acme review", "expires_in": "14d",
	})
	u, err := url.Parse(created.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "team.art.test" || u.Path != "/.kazari/share" || !artifact.IsLinkToken(u.Query().Get("token")) {
		t.Fatalf("url = %q", created.URL)
	}
	if line := firstLine(res); !strings.Contains(line, created.URL) || !strings.Contains(line, created.Prefix) ||
		strings.Contains(line, "Note:") {
		t.Fatalf("summary = %q", line)
	}
	if created.Label != "Acme review" || created.Slug != "team" || created.VersionURL != "" {
		t.Fatalf("share link = %+v", created)
	}

	list, res := ok[api.ShareLinkList](t, cs, "list_share_links", map[string]any{"slug": "team"})
	if len(list.Items) != 1 || list.Items[0].Prefix != created.Prefix || strings.Contains(text(res), u.Query().Get("token")) {
		t.Fatalf("list = %+v", list)
	}

	if done, _ := ok[api.OK](t, cs, "revoke_share_link", map[string]any{"slug": "team", "prefix": created.Prefix}); !done.OK {
		t.Fatal("revoke returned ok false")
	}
	list, res = ok[api.ShareLinkList](t, cs, "list_share_links", map[string]any{"slug": "team"})
	if list.Items[0].Active || !strings.Contains(firstLine(res), "0 active") {
		t.Fatalf("after revoke: %+v, %q", list.Items[0], firstLine(res))
	}

	msg := fails(t, cs, "revoke_share_link", map[string]any{"slug": "team", "prefix": "zzzzzzzz"})
	if !strings.Contains(msg, "share_link_not_found") || !strings.Contains(msg, "list_share_links") {
		t.Fatalf("unknown prefix: %q", msg)
	}
	if msg := fails(t, cs, "create_share_link", map[string]any{"slug": "team", "expires_in": "soon"}); !strings.Contains(msg, "invalid_argument") {
		t.Fatalf("bad expires_in: %q", msg)
	}

	// A link on a public page works, and the summary says it is not needed.
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "open", "name": "Open", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>open</h1>"}},
	})
	_, res = ok[mcpserver.ShareLinkResult](t, cs, "create_share_link", map[string]any{"slug": "open"})
	if line := firstLine(res); !strings.Contains(line, "open is public") {
		t.Fatalf("public summary = %q", line)
	}

	// Creating a link is a write.
	ro := e.connect(t, readOnly)
	if offered(t, ro, "create_share_link") {
		t.Fatalf("create_share_link is offered to a reader")
	}

	pinned, res := ok[mcpserver.ShareLinkResult](t, cs, "create_share_link", map[string]any{"slug": "team", "version": 1})
	if v, err := url.Parse(pinned.VersionURL); err != nil || v.Host != "team--v1.art.test" || v.Path != "/.kazari/share" ||
		v.RawQuery != strings.SplitN(pinned.URL, "?", 2)[1] || !strings.Contains(firstLine(res), pinned.VersionURL) {
		t.Fatalf("pinned share link = %+v, %q", pinned, firstLine(res))
	}
	if msg := fails(t, cs, "create_share_link", map[string]any{"slug": "team", "version": 9}); !strings.HasPrefix(msg, "version_not_found: ") {
		t.Errorf("share link to a missing version = %q", msg)
	}
}

func TestSetVisibilityMembers(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "x"}},
	})
	out, _ := ok[mcpserver.WriteResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "members"})
	if out.Visibility != "members" {
		t.Fatalf("visibility = %q", out.Visibility)
	}
	// An empty visibility, as models send for optional fields, changes
	// nothing.
	upd, _ := ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "name": "Demo 2", "visibility": ""})
	if upd.Visibility != "members" || upd.Name != "Demo 2" {
		t.Fatalf("update with an empty visibility: %+v", upd)
	}
	got, res := ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo"})
	if got.Artifact.Visibility != "members" || !strings.Contains(firstLine(res), "(members)") {
		t.Fatalf("get_artifact = %+v, %q", got.Artifact, firstLine(res))
	}
}
