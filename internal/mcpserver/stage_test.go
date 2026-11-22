package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

func TestStagedVersionTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "x",
		"files": []map[string]any{{"path": "index.html", "content": "<p>one</p>"}},
	})

	staged, res := ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{
		"slug": "demo", "stage": true,
		"files": []map[string]any{{"path": "index.html", "content": "<p>two</p>"}},
	})
	if !staged.Staged || staged.Version != 2 || staged.PublishedVersion != 1 || staged.LatestVersion != 2 {
		t.Errorf("staged update_artifact = %+v, want version 2 staged, 1 published", staged)
	}
	want := "Staged version 2 at https://demo--v2.art.test/, which only people signed in to this instance can open; " +
		"https://demo.art.test/ still serves version 1 until publish_version 2"
	if got := firstLine(res); got != want {
		t.Errorf("staged update_artifact summary = %q, want %q", got, want)
	}

	edited, res := ok[mcpserver.EditFileResult](t, cs, "edit_file", map[string]any{
		"slug": "demo", "path": "index.html", "stage": true,
		"edits": []map[string]any{{"old_text": "two", "new_text": "three"}},
	})
	if !edited.Staged || edited.Version != 3 || edited.PublishedVersion != 1 {
		t.Errorf("staged edit_file = %+v, want version 3 staged, 1 published", edited.WriteResult)
	}
	if got := firstLine(res); !strings.HasPrefix(got, "Staged version 3 at https://demo--v3.art.test/") {
		t.Errorf("staged edit_file summary = %q", got)
	}

	if msg := fails(t, cs, "edit_file", map[string]any{
		"slug": "demo", "path": "index.html", "edits": []map[string]any{{"old_text": "three", "new_text": "four"}},
	}); !strings.HasPrefix(msg, "published_not_latest: ") {
		t.Errorf("publishing edit_file over a staged version = %q, want published_not_latest", msg)
	}
	if msg := fails(t, cs, "create_share_link", map[string]any{"slug": "demo", "version": 3}); !strings.HasPrefix(msg, "invalid_argument: ") {
		t.Errorf("create_share_link for a staged version = %q, want invalid_argument", msg)
	}

	detail, _ := ok[api.ArtifactDetail](t, cs, "get_artifact", map[string]any{"slug": "demo"})
	var got []bool
	for _, v := range detail.Versions {
		got = append(got, v.Staged)
	}
	if len(got) != 3 || !got[0] || !got[1] || got[2] {
		t.Errorf("get_artifact versions 3, 2, 1 staged = %v, want [true true false]", got)
	}

	published, _ := ok[mcpserver.WriteResult](t, cs, "publish_version", map[string]any{"slug": "demo", "version": 3})
	if published.PublishedVersion != 3 || published.Staged {
		t.Errorf("publish_version of a staged version = %+v, want version 3 published", published)
	}
	ok[mcpserver.ShareLinkResult](t, cs, "create_share_link", map[string]any{"slug": "demo", "version": 3})
}
