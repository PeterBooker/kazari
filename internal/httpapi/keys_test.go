package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/user"
)

const day = 24 * time.Hour

func artifactOptions(defaultVisibility string) artifact.Options {
	return artifact.Options{DefaultVisibility: artifact.Visibility(defaultVisibility)}
}

func (e *env) works(key string) bool {
	e.t.Helper()
	return e.send(req{method: http.MethodGet, url: e.u("/v1/me"), token: key}).StatusCode == http.StatusOK
}

func TestIssueOwnKey(t *testing.T) {
	e := newEnv(t, config{})
	laptop, laptopKey := e.key(user.IssueKeyRequest{Handle: "alice", Label: "laptop"})

	var issued api.IssuedKey
	e.call(http.MethodPost, e.u("/v1/me/keys"), laptopKey, api.IssueKeyRequest{Label: "weekly report",
		Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"}, ExpiresIn: "90d"}, http.StatusCreated, &issued)
	want := api.APIKey{Prefix: issued.APIKey.Prefix, Label: "weekly report", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*"}, IssuedBy: laptop.Prefix, ExpiresAt: new(t0.Add(90 * day)), CreatedAt: t0}
	if diff := cmp.Diff(want, issued.APIKey); diff != "" {
		t.Errorf("issued key (-want +got):\n%s", diff)
	}
	if !strings.HasPrefix(issued.Key, "kzk_"+issued.APIKey.Prefix+"_") || !e.works(issued.Key) {
		t.Errorf("issued key string %q does not work", issued.Key)
	}
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), issued.Key, nil, http.StatusOK, &me)
	if me.Handle != "alice" || me.Admin || !cmp.Equal(me.Slugs, []string{"weekly-*"}) || !cmp.Equal(me.Scopes, []string{auth.ScopeRead, auth.ScopeWrite}) {
		t.Errorf("GET /me with the issued key = %+v", me)
	}

	// With no body the new key is as wide as the one that issued it.
	var plain api.IssuedKey
	e.call(http.MethodPost, e.u("/v1/me/keys"), issued.Key, nil, http.StatusCreated, &plain)
	if k := plain.APIKey; !cmp.Equal(k.Scopes, []string{auth.ScopeRead, auth.ScopeWrite}) || !cmp.Equal(k.Slugs, []string{"weekly-*"}) ||
		!k.ExpiresAt.Equal(t0.Add(90*day)) || k.IssuedBy != issued.APIKey.Prefix {
		t.Errorf("key issued with a narrow key = %+v", k)
	}

	for name, tc := range map[string]struct {
		req    api.IssueKeyRequest
		status int
	}{
		"a scope the key lacks":  {api.IssueKeyRequest{Scopes: []string{auth.ScopeDelete}}, http.StatusForbidden},
		"wider slugs":            {api.IssueKeyRequest{Slugs: []string{"*"}}, http.StatusUnprocessableEntity},
		"other slugs":            {api.IssueKeyRequest{Slugs: []string{"other"}}, http.StatusForbidden},
		"a later expiry":         {api.IssueKeyRequest{ExpiresIn: "91d"}, http.StatusForbidden},
		"the admin scope":        {api.IssueKeyRequest{Scopes: []string{auth.ScopeAdmin}}, http.StatusUnprocessableEntity},
		"an unknown scope":       {api.IssueKeyRequest{Scopes: []string{"artifacts:everything"}}, http.StatusUnprocessableEntity},
		"a malformed pattern":    {api.IssueKeyRequest{Slugs: []string{"Weekly"}}, http.StatusUnprocessableEntity},
		"a malformed expiry":     {api.IssueKeyRequest{ExpiresIn: "soon"}, http.StatusUnprocessableEntity},
		"a negative expiry":      {api.IssueKeyRequest{ExpiresIn: "-1h"}, http.StatusUnprocessableEntity},
		"an expiry of zero days": {api.IssueKeyRequest{ExpiresIn: "0d"}, http.StatusUnprocessableEntity},
	} {
		resp := e.jsonReq(http.MethodPost, e.u("/v1/me/keys"), issued.Key, tc.req)
		if resp.StatusCode != tc.status {
			raw, _ := io.ReadAll(resp.Body)
			t.Errorf("%s: status %d, want %d: %s", name, resp.StatusCode, tc.status, raw)
		}
	}

	var keys []api.APIKey
	e.call(http.MethodGet, e.u("/v1/me/keys"), laptopKey, nil, http.StatusOK, &keys)
	byPrefix := map[string]api.APIKey{}
	for _, k := range keys {
		byPrefix[k.Prefix] = k
	}
	if k := byPrefix[issued.APIKey.Prefix]; k.IssuedBy != laptop.Prefix || !cmp.Equal(k.Slugs, []string{"weekly-*"}) {
		t.Errorf("listed key = %+v, want its issuer and slugs", k)
	}
	if len(keys) != 4 { // alice's from newEnv, laptop, and the two issued
		t.Errorf("%d keys listed, want 4: the refused requests issued some", len(keys))
	}
}

func TestOwnKeysNeedAnAPIKey(t *testing.T) {
	e := newEnv(t, config{auth: oauthAs("oauth-token-of-alice", "alice")})
	k, _ := e.key(user.IssueKeyRequest{Handle: "alice"})
	const token = "oauth-token-of-alice"
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), token, nil, http.StatusOK, &me)
	if me.Source != auth.SourceOAuth {
		t.Fatalf("GET /me = %+v, want an OAuth principal", me)
	}
	for name, resp := range map[string]*http.Response{
		"issue":  e.jsonReq(http.MethodPost, e.u("/v1/me/keys"), token, api.IssueKeyRequest{Scopes: []string{auth.ScopeRead}}),
		"revoke": e.send(req{method: http.MethodDelete, url: e.u("/v1/me/keys/" + k.Prefix), token: token}),
		"rotate": e.jsonReq(http.MethodPost, e.u("/v1/me/keys/"+k.Prefix+"/rotate"), token, nil),
	} {
		if got := e.apiErr(resp, http.StatusForbidden, api.CodeForbidden); !strings.Contains(got.Message, "an API key is required") {
			t.Errorf("%s with an OAuth token: message %q", name, got.Message)
		}
	}
	keys, err := e.users.ListKeys(e.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.RevokedAt != nil || key.ExpiresAt != nil {
			t.Errorf("a refused request changed key %s: %+v", key.Prefix, key)
		}
	}
	if len(keys) != 2 {
		t.Errorf("%d keys, want 2: an OAuth token minted one", len(keys))
	}
	e.expect(e.send(req{method: http.MethodPost, url: e.u("/v1/me/keys")}), http.StatusUnauthorized, nil)
}

func TestRevokeOwnKey(t *testing.T) {
	e := newEnv(t, config{})
	laptop, laptopKey := e.key(user.IssueKeyRequest{Handle: "alice"})
	var agent, sub api.IssuedKey
	e.call(http.MethodPost, e.u("/v1/me/keys"), laptopKey, api.IssueKeyRequest{Slugs: []string{"weekly-*"}}, http.StatusCreated, &agent)
	e.call(http.MethodPost, e.u("/v1/me/keys"), agent.Key, nil, http.StatusCreated, &sub)
	_, bobKey := e.key(user.IssueKeyRequest{Handle: "bob"})
	bob, _ := e.key(user.IssueKeyRequest{Handle: "bob"})

	del := func(token, prefix string) *http.Response {
		return e.send(req{method: http.MethodDelete, url: e.u("/v1/me/keys/" + prefix), token: token})
	}
	e.apiErr(del(agent.Key, laptop.Prefix), http.StatusForbidden, api.CodeForbidden)
	e.apiErr(del(laptopKey, bob.Prefix), http.StatusNotFound, api.CodeNotFound)
	e.apiErr(del(bobKey, laptop.Prefix), http.StatusNotFound, api.CodeNotFound)
	e.apiErr(del(laptopKey, "zzzzzzzz"), http.StatusNotFound, api.CodeNotFound)
	if !e.works(laptopKey) || !e.works(agent.Key) || !e.works(sub.Key) {
		t.Fatal("a refused revoke revoked a key")
	}

	e.expect(del(laptopKey, laptop.Prefix), http.StatusNoContent, nil)
	for name, key := range map[string]string{"the key": laptopKey, "a key issued with it": agent.Key, "a key issued with that": sub.Key} {
		if e.works(key) {
			t.Errorf("%s works after the revoke", name)
		}
	}
	if !e.works(e.alice) || !e.works(bobKey) {
		t.Error("the revoke reached a key that was not issued with the revoked one")
	}
}

func TestRotateOwnKey(t *testing.T) {
	e := newEnv(t, config{})
	laptop, laptopKey := e.key(user.IssueKeyRequest{Handle: "alice", Label: "laptop"})
	agent, agentKey := e.key(user.IssueKeyRequest{Handle: "alice", Label: "weekly", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*"}, ExpiresIn: 30 * day})

	var rotated api.IssuedKey
	e.call(http.MethodPost, e.u("/v1/me/keys/"+agent.Prefix+"/rotate"), laptopKey, nil, http.StatusCreated, &rotated)
	want := api.APIKey{Prefix: rotated.APIKey.Prefix, Label: "weekly", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*"}, ExpiresAt: new(t0.Add(30 * day)), CreatedAt: t0}
	if diff := cmp.Diff(want, rotated.APIKey); diff != "" {
		t.Errorf("new key (-want +got):\n%s", diff)
	}
	if rotated.Replaces == nil || rotated.Replaces.Prefix != agent.Prefix || !rotated.Replaces.ExpiresAt.Equal(t0.Add(user.DefaultRotateOverlap)) {
		t.Errorf("replaces = %+v, want the old key ending after the default overlap", rotated.Replaces)
	}
	if !e.works(rotated.Key) || !e.works(agentKey) {
		t.Error("inside the overlap both keys must work")
	}

	var now api.IssuedKey
	e.call(http.MethodPost, e.u("/v1/me/keys/"+rotated.APIKey.Prefix+"/rotate"), laptopKey, api.RotateKeyRequest{Overlap: "0"}, http.StatusCreated, &now)
	if e.works(rotated.Key) || !e.works(now.Key) {
		t.Errorf("overlap 0: old works = %t, new works = %t; want false, true", e.works(rotated.Key), e.works(now.Key))
	}
	var week api.IssuedKey
	e.call(http.MethodPost, e.u("/v1/me/keys/"+now.APIKey.Prefix+"/rotate"), now.Key, api.RotateKeyRequest{Overlap: "7d"}, http.StatusCreated, &week)
	if !week.Replaces.ExpiresAt.Equal(t0.Add(7 * day)) {
		t.Errorf("overlap 7d: old key ends at %v", week.Replaces.ExpiresAt)
	}

	rotate := func(token, prefix, overlap string) *http.Response {
		return e.jsonReq(http.MethodPost, e.u("/v1/me/keys/"+prefix+"/rotate"), token, api.RotateKeyRequest{Overlap: overlap})
	}
	e.apiErr(rotate(week.Key, laptop.Prefix, ""), http.StatusForbidden, api.CodeForbidden)
	e.apiErr(rotate(laptopKey, week.APIKey.Prefix, "8d"), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(rotate(laptopKey, week.APIKey.Prefix, "later"), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(rotate(laptopKey, rotated.APIKey.Prefix, ""), http.StatusNotFound, api.CodeNotFound)
	e.apiErr(rotate(laptopKey, "zzzzzzzz", ""), http.StatusNotFound, api.CodeNotFound)

	var events api.AuditList
	e.call(http.MethodGet, e.a("/v1/audit"), adminToken, nil, http.StatusOK, &events)
	rotations := 0
	for _, ev := range events.Events {
		if ev.Action == audit.KeyRotate {
			rotations++
		}
		for _, secret := range []string{laptopKey, agentKey, rotated.Key, now.Key, week.Key} {
			if _, s, ok := user.ParseKey(secret); ok && strings.Contains(strings.Join(mapValues(ev.Details), " "), s) {
				t.Errorf("audit event holds a key's secret: %+v", ev)
			}
		}
	}
	if rotations != 3 {
		t.Errorf("%d key.rotate events, want 3", rotations)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestAdminIssuesRestrictedKey(t *testing.T) {
	e := newEnv(t, config{maxKeyTTL: 365 * day})
	var issued api.IssuedKey
	e.call(http.MethodPost, e.a("/v1/users/alice/keys"), adminToken, api.IssueKeyRequest{Label: "weekly-report",
		Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"}, ExpiresIn: "90d"}, http.StatusCreated, &issued)
	want := api.APIKey{Prefix: issued.APIKey.Prefix, Label: "weekly-report", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*"}, ExpiresAt: new(t0.Add(90 * day)), CreatedAt: t0}
	if diff := cmp.Diff(want, issued.APIKey); diff != "" {
		t.Errorf("issued key (-want +got):\n%s", diff)
	}
	e.call(http.MethodPost, e.a("/v1/users/alice/keys"), adminToken, api.IssueKeyRequest{ExpiresIn: "2160h"}, http.StatusCreated, &issued)
	if !issued.APIKey.ExpiresAt.Equal(t0.Add(90 * day)) {
		t.Errorf("expires_in 2160h: expires %v", issued.APIKey.ExpiresAt)
	}
	e.call(http.MethodPost, e.a("/v1/users/alice/keys"), adminToken, nil, http.StatusCreated, &issued)
	if !issued.APIKey.ExpiresAt.Equal(t0.Add(365 * day)) {
		t.Errorf("no expiry asked under a limit: expires %v, want the limit", issued.APIKey.ExpiresAt)
	}
	for name, body := range map[string]api.IssueKeyRequest{
		"an expiry over the limit": {ExpiresIn: "366d"},
		"a malformed pattern":      {Slugs: []string{"a--b"}},
		"too many patterns":        {Slugs: strings.Fields(strings.Repeat("page ", artifact.MaxSlugPatterns+1))},
		"a malformed expiry":       {ExpiresIn: "90 days"},
	} {
		if name == "too many patterns" {
			for i := range body.Slugs {
				body.Slugs[i] += "-" + string(rune('a'+i))
			}
		}
		resp := e.jsonReq(http.MethodPost, e.a("/v1/users/alice/keys"), adminToken, body)
		if got := e.apiErr(resp, http.StatusUnprocessableEntity, api.CodeInvalidArgument); got.Message == "" {
			t.Errorf("%s: empty message", name)
		}
	}

	var rotated api.IssuedKey
	e.call(http.MethodPost, e.a("/v1/users/alice/keys/"+issued.APIKey.Prefix+"/rotate"), adminToken,
		api.RotateKeyRequest{Overlap: "1h"}, http.StatusCreated, &rotated)
	if rotated.Replaces.Prefix != issued.APIKey.Prefix || !rotated.Replaces.ExpiresAt.Equal(t0.Add(time.Hour)) || !e.works(rotated.Key) {
		t.Errorf("admin rotation = %+v", rotated)
	}
	e.apiErr(e.jsonReq(http.MethodPost, e.a("/v1/users/bob/keys/"+issued.APIKey.Prefix+"/rotate"), adminToken, nil),
		http.StatusNotFound, api.CodeNotFound)
	e.apiErr(e.jsonReq(http.MethodPost, e.a("/v1/users/alice/keys/"+issued.APIKey.Prefix+"/rotate"), e.alice, nil),
		http.StatusForbidden, api.CodeForbidden)
}
