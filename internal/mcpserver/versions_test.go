package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/mcpserver"
)

func TestShareLinkForDeletedVersion(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, member)
	ok[mcpserver.WriteResult](t, cs, "create_artifact", map[string]any{
		"slug": "demo", "name": "Demo", "description": "x", "visibility": "members",
		"files": []map[string]any{{"path": "index.html", "content": "<p>one</p>"}},
	})
	for _, body := range []string{"<p>two</p>", "<p>three</p>"} {
		ok[mcpserver.WriteResult](t, cs, "update_artifact", map[string]any{
			"slug": "demo", "files": []map[string]any{{"path": "index.html", "content": body}},
		})
	}
	if _, err := e.deps.Artifacts.DeleteVersion(e.ctx, artifact.Actor{Handle: member.Handle}, "demo", 2); err != nil {
		t.Fatal(err)
	}
	msg := fails(t, cs, "create_share_link", map[string]any{"slug": "demo", "version": 2})
	if !strings.HasPrefix(msg, "version_not_found: ") || !strings.Contains(msg, "deleted") {
		t.Errorf("create_share_link for a deleted version = %q, want version_not_found saying it was deleted", msg)
	}
}
