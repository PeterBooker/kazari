package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/api"
)

func TestRunShare(t *testing.T) {
	team := demo
	team.Slug, team.Visibility = "team", "members"
	link := api.ShareLink{Prefix: "abcdefgh", Label: "Acme", CreatedBy: "peter", CreatedAt: t0, ExpiresAt: t0.Add(72 * time.Hour), Active: true}
	revoked := t0.Add(time.Hour)
	used := t0.Add(time.Minute)
	var body string
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts/team/share-links": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = strings.TrimSpace(string(b))
			reply(w, http.StatusCreated, api.CreatedShareLink{URL: "https://team.art.test/.kazari/share?token=kzs_x", ShareLink: link, Artifact: team})
		},
		"POST /v1/artifacts/demo/share-links": replyWith(http.StatusCreated, api.CreatedShareLink{URL: "https://demo.art.test/.kazari/share?token=kzs_y", ShareLink: link, Artifact: demo}),
		"GET /v1/artifacts/team/share-links": replyWith(http.StatusOK, api.ShareLinkList{Items: []api.ShareLink{
			link,
			{Prefix: "zyxwvuts", CreatedBy: "ann", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), RevokedAt: &revoked, LastUsedAt: &used, Uses: 2},
		}}),
		"DELETE /v1/artifacts/team/share-links/abcdefgh": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})
	run := func(args ...string) result {
		t.Helper()
		return runCLI(t, nil, append([]string{"--url", srv.URL, "--token", "t"}, args...)...)
	}

	r := run("share", "create", "team", "--label", "Acme", "--expires", "3d")
	if r.code != 0 || r.stdout != "https://team.art.test/.kazari/share?token=kzs_x\n" ||
		!strings.Contains(r.stderr, "share link abcdefgh opens team until 2026-10-04T14:00:00+02:00") || strings.Contains(r.stderr, "note:") {
		t.Fatalf("create: %+v", r)
	}
	if body != `{"label":"Acme","expires_in":"3d"}` {
		t.Fatalf("create body = %s", body)
	}
	if r := run("share", "create", "demo"); r.code != 0 || !strings.Contains(r.stderr, "note: demo is public") {
		t.Fatalf("create on a public artifact: %+v", r)
	}

	r = run("share", "list", "team")
	want := "PREFIX    LABEL  CREATED BY  CREATED                    EXPIRES                    USES  LAST USED                  STATE\n" +
		"abcdefgh  Acme   peter       2026-10-01T14:00:00+02:00  2026-10-04T14:00:00+02:00  0     -                          active\n" +
		"zyxwvuts  -      ann         2026-10-01T14:00:00+02:00  2026-10-01T15:00:00+02:00  2     2026-10-01T14:01:00+02:00  revoked\n"
	if r.code != 0 {
		t.Fatalf("list: %+v", r)
	}
	if diff := cmp.Diff(want, r.stdout); diff != "" {
		t.Errorf("list (-want +got):\n%s", diff)
	}

	if r := run("share", "revoke", "team", "abcdefgh"); r.code != 0 || r.stdout != "revoked share link abcdefgh of team\n" {
		t.Fatalf("revoke: %+v", r)
	}
	for _, args := range [][]string{{"share"}, {"share", "grant", "team"}, {"share", "revoke", "team"}} {
		if r := run(args...); r.code != 2 {
			t.Errorf("%v: code %d, want 2: %+v", args, r.code, r)
		}
	}
}
