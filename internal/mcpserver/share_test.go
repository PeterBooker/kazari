package mcpserver_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
)

func TestShareLinkTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug": "team", "name": "Team", "description": "x", "visibility": "members",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>team</h1>"}},
	})

	created, res := ok[api.CreatedShareLink](t, cs, "create_share_link", map[string]any{
		"slug": "team", "label": "Acme review", "expires_in": "14d",
	})
	u, err := url.Parse(created.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "team.art.test" || u.Path != "/.kazari/share" || !artifact.IsLinkToken(u.Query().Get("token")) {
		t.Fatalf("url = %q", created.URL)
	}
	if line := firstLine(res); !strings.Contains(line, created.URL) || !strings.Contains(line, created.ShareLink.Prefix) ||
		strings.Contains(line, "Note:") {
		t.Fatalf("summary = %q", line)
	}
	if created.ShareLink.Label != "Acme review" || !created.ShareLink.Active {
		t.Fatalf("share link = %+v", created.ShareLink)
	}

	list, res := ok[api.ShareLinkList](t, cs, "list_share_links", map[string]any{"slug": "team"})
	if len(list.Items) != 1 || list.Items[0].Prefix != created.ShareLink.Prefix || strings.Contains(text(res), u.Query().Get("token")) {
		t.Fatalf("list = %+v", list)
	}

	if done, _ := ok[api.OK](t, cs, "revoke_share_link", map[string]any{"slug": "team", "prefix": created.ShareLink.Prefix}); !done.OK {
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
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug": "open", "name": "Open", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>open</h1>"}},
	})
	_, res = ok[api.CreatedShareLink](t, cs, "create_share_link", map[string]any{"slug": "open"})
	if line := firstLine(res); !strings.Contains(line, "open is public") {
		t.Fatalf("public summary = %q", line)
	}

	// Creating a link is a write.
	ro := e.connect(t, readOnly)
	if msg := fails(t, ro, "create_share_link", map[string]any{"slug": "team"}); !strings.Contains(msg, "forbidden") {
		t.Fatalf("read-only create: %q", msg)
	}
}

func TestSetVisibilityMembers(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[api.PublishResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "x"}},
	})
	out, _ := ok[api.PublishResult](t, cs, "set_visibility", map[string]any{"slug": "demo", "visibility": "members"})
	if out.Artifact.Visibility != "members" {
		t.Fatalf("visibility = %q", out.Artifact.Visibility)
	}
	// An empty visibility, as models send for optional fields, changes
	// nothing.
	upd, _ := ok[api.PublishResult](t, cs, "update_artifact", map[string]any{"slug": "demo", "name": "Demo 2", "visibility": ""})
	if upd.Artifact.Visibility != "members" || upd.Artifact.Name != "Demo 2" {
		t.Fatalf("update with an empty visibility: %+v", upd.Artifact)
	}
	got, res := ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo"})
	if got.Artifact.Visibility != "members" || !strings.Contains(firstLine(res), "(members)") {
		t.Fatalf("get_artifact = %+v, %q", got.Artifact, firstLine(res))
	}
}
