package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

func TestRunOwnKeys(t *testing.T) {
	expires := t0.Add(90 * 24 * time.Hour)
	overlapEnd := t0.Add(24 * time.Hour)
	var body string
	record := func(status int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = strings.TrimSpace(string(b))
			reply(w, status, v)
		}
	}
	agent := api.APIKey{Prefix: "agentpfx", Label: "weekly", Scopes: []string{"artifacts:read", "artifacts:write"},
		Slugs: []string{"weekly-*"}, IssuedBy: "laptoppf", ExpiresAt: &expires, CreatedAt: t0}
	old := agent
	old.Prefix, old.ExpiresAt = "oldagent", &overlapEnd
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/me": record(200, api.Me{Handle: "peter", Scopes: []string{"artifacts:read"}, Source: "api_key",
			Slugs: []string{"weekly-*", "report"}, ExpiresAt: &expires}),
		"GET /v1/me/keys":                  record(200, []api.APIKey{agent}),
		"POST /v1/me/keys":                 record(201, api.IssuedKey{Key: "kzk_agentpfx_SECRET", APIKey: agent}),
		"DELETE /v1/me/keys/agentpfx":      record(204, nil),
		"POST /v1/me/keys/oldagent/rotate": record(201, api.IssuedKey{Key: "kzk_agentpfx_SECRET", APIKey: agent, Replaces: &old}),
	})
	run := func(args ...string) result {
		body = ""
		return runCLI(t, nil, append([]string{"--url", srv.URL, "--token", "kzk_mine"}, args...)...)
	}

	for _, tt := range []struct {
		args       []string
		wantOut    string
		wantStderr string
		wantBody   string
	}{
		{args: []string{"me"}, wantOut: "handle:   peter\nadmin:    false\nscopes:   artifacts:read\nsource:   api_key\n" +
			"slugs:    weekly-*, report\nexpires:  2026-12-30T14:00:00+02:00\nstorage:  0 B used (no quota)\n"},
		{args: []string{"keys", "list"}, wantOut: "PREFIX    LABEL   SCOPES                          SLUGS     ISSUED BY  CREATED                    LAST USED  EXPIRES                    REVOKED\n" +
			"agentpfx  weekly  artifacts:read,artifacts:write  weekly-*  laptoppf   2026-10-01T14:00:00+02:00  -          2026-12-30T14:00:00+02:00  -\n"},
		{args: []string{"keys", "issue", "--label", "weekly", "--scopes", "artifacts:read, artifacts:write", "--slugs", "weekly-*, report", "--expires", "90d"},
			wantOut:    "kzk_agentpfx_SECRET\n",
			wantStderr: "issued key agentpfx (scopes artifacts:read,artifacts:write, restricted to weekly-*, expires 2026-12-30T14:00:00+02:00); the key is shown only once\n",
			wantBody:   `{"label":"weekly","scopes":["artifacts:read","artifacts:write"],"slugs":["weekly-*","report"],"expires_in":"90d"}`},
		{args: []string{"keys", "issue"}, wantOut: "kzk_agentpfx_SECRET\n",
			wantStderr: "issued key agentpfx (scopes artifacts:read,artifacts:write, restricted to weekly-*, expires 2026-12-30T14:00:00+02:00); the key is shown only once\n",
			wantBody:   `{}`},
		{args: []string{"keys", "rotate", "oldagent"}, wantOut: "kzk_agentpfx_SECRET\n",
			wantStderr: "issued key agentpfx (scopes artifacts:read,artifacts:write, restricted to weekly-*, expires 2026-12-30T14:00:00+02:00) " +
				"in place of oldagent, which works until 2026-10-02T14:00:00+02:00; the key is shown only once\n",
			wantBody: `{}`},
		{args: []string{"keys", "rotate", "oldagent", "--overlap", "0"}, wantOut: "kzk_agentpfx_SECRET\n",
			wantStderr: "issued key agentpfx (scopes artifacts:read,artifacts:write, restricted to weekly-*, expires 2026-12-30T14:00:00+02:00) " +
				"in place of oldagent, which works until 2026-10-02T14:00:00+02:00; the key is shown only once\n",
			wantBody: `{"overlap":"0"}`},
		{args: []string{"keys", "revoke", "agentpfx"}, wantOut: "revoked agentpfx\n"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			r := run(tt.args...)
			if r.code != 0 {
				t.Fatalf("%+v", r)
			}
			if diff := cmp.Diff(tt.wantOut, r.stdout); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantStderr, r.stderr); diff != "" {
				t.Errorf("stderr (-want +got):\n%s", diff)
			}
			if body != tt.wantBody {
				t.Errorf("body = %s, want %s", body, tt.wantBody)
			}
		})
	}
	// "kazari keys" alone still lists.
	if r := run("keys"); r.code != 0 || !strings.Contains(r.stdout, "agentpfx") {
		t.Errorf("keys without a subcommand: %+v", r)
	}
	// The issued key is printed once, and never the token the client holds.
	r := run("--json", "keys", "rotate", "oldagent")
	if strings.Count(r.stdout, "kzk_agentpfx_SECRET") != 1 || strings.Contains(r.stdout+r.stderr, "kzk_mine") {
		t.Errorf("%+v", r)
	}
	for _, a := range f.auth {
		if a != "Bearer kzk_mine" {
			t.Errorf("Authorization = %q", a)
		}
	}
}

func TestRunAdminKeysAndGrants(t *testing.T) {
	expires := t0.Add(90 * 24 * time.Hour)
	var body string
	record := func(status int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = strings.TrimSpace(string(b))
			reply(w, status, v)
		}
	}
	key := api.APIKey{Prefix: "newnewne", Scopes: []string{"artifacts:read"}, CreatedAt: t0}
	old := api.APIKey{Prefix: "pfx", Scopes: []string{"artifacts:read"}, CreatedAt: t0, ExpiresAt: &t0}
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/users/bob/keys":            record(201, api.IssuedKey{Key: "kzk_newnewne_SECRET", APIKey: key}),
		"POST /v1/users/bob/keys/pfx/rotate": record(201, api.IssuedKey{Key: "kzk_newnewne_SECRET", APIKey: key, Replaces: &old}),
		"GET /v1/users/bob/grants": record(200, []api.OAuthGrant{
			{ID: 17, ClientID: "https://claude.ai/oauth/client.json", Client: "Claude", User: "bob",
				Scopes: []string{"artifacts:read", "artifacts:write"}, KeyPrefix: "agentpfx", CreatedAt: t0, ExpiresAt: expires},
			{ID: 4, ClientID: "dcr_abc", User: "bob", Scopes: []string{"artifacts:read"}, CreatedAt: t0, ExpiresAt: expires},
		}),
		"DELETE /v1/grants/17": record(204, nil),
		"DELETE /v1/grants/99": record(404, api.ErrorResponse{Error: api.Error{Code: api.CodeNotFound, Message: "grant not found"}}),
	})
	run := func(args ...string) result {
		body = ""
		return runCLI(t, nil, append([]string{"--admin-url", srv.URL, "--admin-token", "adm", "admin"}, args...)...)
	}

	r := run("keys", "issue", "bob", "--slugs", "weekly-*", "--expires", "90d")
	if r.code != 0 || r.stdout != "kzk_newnewne_SECRET\n" || body != `{"slugs":["weekly-*"],"expires_in":"90d"}` {
		t.Errorf("keys issue: %+v, body %s", r, body)
	}
	r = run("keys", "rotate", "bob", "pfx", "--overlap", "1h")
	if r.code != 0 || r.stdout != "kzk_newnewne_SECRET\n" || body != `{"overlap":"1h"}` ||
		r.stderr != "issued key newnewne (scopes artifacts:read) in place of pfx, which works until 2026-10-01T14:00:00+02:00 for bob; the key is shown only once\n" {
		t.Errorf("keys rotate: %+v, body %s", r, body)
	}
	r = run("grants", "list", "bob")
	want := "ID  CLIENT   SCOPES                          KEY       LAST REFRESH               EXPIRES\n" +
		"17  Claude   artifacts:read,artifacts:write  agentpfx  2026-10-01T14:00:00+02:00  2026-12-30T14:00:00+02:00\n" +
		"4   dcr_abc  artifacts:read                  -         2026-10-01T14:00:00+02:00  2026-12-30T14:00:00+02:00\n"
	if diff := cmp.Diff(want, r.stdout); r.code != 0 || diff != "" {
		t.Errorf("grants list: %+v (-want +got):\n%s", r, diff)
	}
	if r = run("grants", "revoke", "17"); r.code != 0 || r.stdout != "revoked grant 17\n" {
		t.Errorf("grants revoke: %+v", r)
	}
	if r = run("grants", "revoke", "seventeen"); r.code != exitUsage {
		t.Errorf("grants revoke with a word: %+v", r)
	}
	if r = run("grants", "revoke", "99"); r.code != exitNotFound {
		t.Errorf("grants revoke of an unknown grant: %+v", r)
	}
}
