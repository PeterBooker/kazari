package httpapi_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/user"
)

const testPassword = "correct horse battery"

// admin creates an admin and returns a key of theirs with every scope.
func (e *env) admin(handle string) string {
	e.t.Helper()
	if _, err := e.users.CreateUser(e.ctx, userReq(handle, "admin")); err != nil {
		e.t.Fatal(err)
	}
	_, key := e.key(user.IssueKeyRequest{Handle: handle, Scopes: auth.AllScopes})
	return key
}

// linkToken checks that raw is the apex address for purpose and returns the
// token it carries.
func linkToken(t *testing.T, raw, purpose string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	token := u.Query().Get("token")
	if u.Scheme != "https" || u.Host != ".art.test" || u.Path != "/account/"+purpose || len(u.Query()) != 1 || !user.IsLinkToken(token) {
		t.Fatalf("link = %q, want https://<apex>/account/%s?token=<token>", raw, purpose)
	}
	if strings.Contains(u.Path, token) {
		t.Fatalf("link %q carries its token in the path", raw)
	}
	return token
}

func TestAdminGuards(t *testing.T) {
	e := newEnv(t, config{})
	boss := e.admin("boss")
	disable, demote := api.UpdateUserRequest{Disabled: new(true)}, api.UpdateUserRequest{Role: new("member")}

	// The last admin stays one, whoever asks.
	for _, token := range []string{adminToken, boss} {
		for _, change := range []api.UpdateUserRequest{disable, demote} {
			resp := e.send(req{method: http.MethodPatch, url: e.a("/v1/users/boss"), token: token,
				contentType: "application/json", body: strings.NewReader(string(mustJSON(t, change)))})
			if token == boss {
				e.apiErr(resp, http.StatusForbidden, api.CodeForbidden)
				continue
			}
			e.apiErr(resp, http.StatusConflict, api.CodeLastAdmin)
		}
	}
	var u api.User
	e.call(http.MethodGet, e.a("/v1/users/boss"), adminToken, nil, http.StatusOK, &u)
	if u.Role != "admin" || u.DisabledAt != nil {
		t.Fatalf("the last admin after refused changes = %+v", u)
	}

	// With a second admin nobody still disables or demotes themselves, and
	// either may do it to the other.
	chief := e.admin("chief")
	for _, change := range []api.UpdateUserRequest{disable, demote} {
		e.apiErr(e.send(req{method: http.MethodPatch, url: e.a("/v1/users/chief"), token: chief,
			contentType: "application/json", body: strings.NewReader(string(mustJSON(t, change)))}), http.StatusForbidden, api.CodeForbidden)
	}
	e.call(http.MethodPatch, e.a("/v1/users/chief"), boss, demote, http.StatusOK, &u)
	if u.Role != "member" {
		t.Errorf("chief after a demotion by boss = %+v", u)
	}
	e.apiErr(e.send(req{method: http.MethodPatch, url: e.a("/v1/users/boss"), token: adminToken,
		contentType: "application/json", body: strings.NewReader(string(mustJSON(t, disable)))}), http.StatusConflict, api.CodeLastAdmin)
}

func TestAdminInvite(t *testing.T) {
	e := newEnv(t, config{local: true})

	resp := e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"carol","email":"Carol@Example.com","display_name":"Carol","quota_bytes":1048576}`)})
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite: status %d: %s", resp.StatusCode, raw)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var out api.Invited
	if err := jsonUnmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	token := linkToken(t, out.Link.URL, "invite")
	if n := strings.Count(string(raw), token); n != 1 {
		t.Errorf("the token appears %d times in the response, want once, in the link", n)
	}
	if out.User.Handle != "carol" || out.User.Email != "carol@example.com" || out.User.Role != "member" || out.User.QuotaBytes != 1<<20 ||
		!out.User.Invited || out.User.Password {
		t.Errorf("invited user = %+v", out.User)
	}
	l := out.Link
	if l.ID == 0 || l.User != "carol" || l.Purpose != "invite" || l.CreatedBy != "admin" || !l.CreatedAt.Equal(t0) ||
		!l.ExpiresAt.Equal(t0.Add(user.DefaultInviteTTL)) {
		t.Errorf("link = %+v", l)
	}
	if _, err := e.users.Redeem(e.ctx, token, user.RedeemRequest{Password: testPassword}); err != nil {
		t.Fatalf("the link in the response does not set a password: %v", err)
	}
	if _, err := e.users.CheckPassword(e.ctx, "carol", testPassword); err != nil {
		t.Errorf("sign-in after the invite: %v", err)
	}

	// An admin invites an admin; nobody else reaches the route.
	e.call(http.MethodPost, e.a("/v1/invites"), adminToken, api.InviteRequest{Handle: "dana", Role: "admin"}, http.StatusCreated, &out)
	if out.User.Role != "admin" {
		t.Errorf("invited admin = %+v", out.User)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), token: e.alice, contentType: "application/json",
		body: strings.NewReader(`{"handle":"eve","role":"admin"}`)}), http.StatusForbidden, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), contentType: "application/json",
		body: strings.NewReader(`{"handle":"eve"}`)}), http.StatusUnauthorized, api.CodeUnauthorized)
	if _, err := e.users.GetUser(e.ctx, "eve"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("a refused invite created the user: %v", err)
	}

	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"carol"}`)}), http.StatusConflict, api.CodeHandleTaken)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"X"}`)}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"fay","role":"owner"}`)}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
}

func TestAdminLinks(t *testing.T) {
	e := newEnv(t, config{local: true})
	var invited api.Invited
	e.call(http.MethodPost, e.a("/v1/invites"), adminToken, api.InviteRequest{Handle: "carol"}, http.StatusCreated, &invited)
	first := linkToken(t, invited.Link.URL, "invite")

	// A new invite link ends the one before it.
	var link api.CreatedLink
	resp := e.call(http.MethodPost, e.a("/v1/users/carol/links"), adminToken, api.LinkRequest{Purpose: "invite"}, http.StatusCreated, &link)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	second := linkToken(t, link.URL, "invite")
	if _, _, err := e.users.Link(e.ctx, first); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("the replaced invite still works: %v", err)
	}
	if _, _, err := e.users.Link(e.ctx, second); err != nil {
		t.Errorf("the new invite: %v", err)
	}

	e.call(http.MethodPost, e.a("/v1/users/alice/links"), adminToken, api.LinkRequest{Purpose: "reset", RevokeAll: true}, http.StatusCreated, &link)
	reset := linkToken(t, link.URL, "reset")
	if link.User != "alice" || link.Purpose != "reset" || !link.RevokeAll || !link.ExpiresAt.Equal(t0.Add(user.DefaultResetTTL)) {
		t.Errorf("reset link = %+v", link.Link)
	}
	// revoke_all means something for a reset alone.
	e.call(http.MethodPost, e.a("/v1/users/carol/links"), adminToken, api.LinkRequest{Purpose: "invite", RevokeAll: true}, http.StatusCreated, &link)
	if link.RevokeAll {
		t.Errorf("an invite link with revoke_all = %+v", link.Link)
	}
	third := linkToken(t, link.URL, "invite")

	for _, purpose := range []string{"setup", "", "other"} {
		e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/carol/links"), token: adminToken, contentType: "application/json",
			body: strings.NewReader(`{"purpose":"` + purpose + `"}`)}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/nobody/links"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"purpose":"reset"}`)}), http.StatusNotFound, api.CodeNotFound)

	// The list names the live links and never their tokens.
	resp = e.send(req{method: http.MethodGet, url: e.a("/v1/invites"), token: adminToken})
	raw, _ := io.ReadAll(resp.Body)
	var links []api.Link
	if err := jsonUnmarshal(raw, &links); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, %v: %s", resp.StatusCode, err, raw)
	}
	if len(links) != 2 {
		t.Fatalf("links = %+v, want carol's invite and alice's reset", links)
	}
	for _, secret := range []string{first, second, third, reset, "token", "url", user.LinkPrefix} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the list holds %q: %s", secret, raw)
		}
	}
	var resetID int64
	for _, l := range links {
		if l.Purpose == "reset" && l.User == "alice" {
			resetID = l.ID
		}
	}

	e.call(http.MethodDelete, e.a(fmt.Sprintf("/v1/invites/%d", resetID)), adminToken, nil, http.StatusNoContent, nil)
	if _, err := e.users.Redeem(e.ctx, reset, user.RedeemRequest{Password: testPassword}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("a revoked link still sets a password: %v", err)
	}
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a(fmt.Sprintf("/v1/invites/%d", resetID)), token: adminToken}), http.StatusNotFound, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a("/v1/invites/zero"), token: adminToken}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.call(http.MethodGet, e.a("/v1/invites"), adminToken, nil, http.StatusOK, &links)
	if len(links) != 1 || links[0].User != "carol" {
		t.Errorf("links after the revoke = %+v", links)
	}
}

func TestAdminSetupLink(t *testing.T) {
	e := newEnv(t, config{local: true, empty: true})
	var link api.CreatedLink
	resp := e.call(http.MethodPost, e.a("/v1/setup-link"), adminToken, nil, http.StatusCreated, &link)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	token := linkToken(t, link.URL, "setup")
	if link.User != "" || link.Purpose != "setup" || !link.ExpiresAt.Equal(t0.Add(user.DefaultSetupTTL)) {
		t.Errorf("setup link = %+v", link.Link)
	}
	u, err := e.users.Redeem(e.ctx, token, user.RedeemRequest{Handle: "peter", Password: testPassword})
	if err != nil || u.Role != user.RoleAdmin {
		t.Fatalf("Redeem = %+v, %v", u, err)
	}
	// Once a user exists there is nothing to set up.
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/setup-link"), token: adminToken}), http.StatusConflict, api.CodeSetupDone)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/setup-link")}), http.StatusUnauthorized, api.CodeUnauthorized)
}

func TestAdminLinksWithLocalAccountsOff(t *testing.T) {
	e := newEnv(t, config{empty: true})
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/setup-link"), token: adminToken}), http.StatusForbidden, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/invites"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"handle":"carol"}`)}), http.StatusForbidden, api.CodeForbidden)
	if _, err := e.users.GetUser(e.ctx, "carol"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("a refused invite created the user: %v", err)
	}
	e.member("alice", nil)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/alice/links"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"purpose":"reset"}`)}), http.StatusForbidden, api.CodeForbidden)
	var links []api.Link
	e.call(http.MethodGet, e.a("/v1/invites"), adminToken, nil, http.StatusOK, &links)
	if len(links) != 0 {
		t.Errorf("links = %+v", links)
	}
}

// fakeSessions stands in for signin.Service.
type fakeSessions struct {
	mu    sync.Mutex
	count int
	ended []string
}

func (f *fakeSessions) EndSessions(_ context.Context, handle string, keep int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, fmt.Sprintf("%s keep %d", handle, keep))
	f.count = 0
	return nil
}

func (f *fakeSessions) CountSessions(context.Context, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count, nil
}

func TestAdminSessions(t *testing.T) {
	sessions := &fakeSessions{count: 3}
	e := newEnv(t, config{sessions: sessions})
	var u api.User
	e.call(http.MethodGet, e.a("/v1/users/alice"), adminToken, nil, http.StatusOK, &u)
	if u.Sessions == nil || *u.Sessions != 3 {
		t.Errorf("sessions = %v, want 3", u.Sessions)
	}
	var users []api.User
	e.call(http.MethodGet, e.a("/v1/users"), adminToken, nil, http.StatusOK, &users)
	if len(users) != 1 || users[0].Sessions != nil {
		t.Errorf("the list counts sessions: %+v", users)
	}

	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a("/v1/users/nobody/sessions"), token: adminToken}), http.StatusNotFound, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.a("/v1/users/alice/sessions"), token: e.alice}), http.StatusForbidden, api.CodeForbidden)
	if len(sessions.ended) != 0 {
		t.Fatalf("refused requests ended sessions: %v", sessions.ended)
	}
	e.call(http.MethodDelete, e.a("/v1/users/alice/sessions"), adminToken, nil, http.StatusNoContent, nil)
	if len(sessions.ended) != 1 || sessions.ended[0] != "alice keep 0" {
		t.Errorf("EndSessions calls = %v, want every session of alice", sessions.ended)
	}
	// Her key is no session.
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), e.alice, nil, http.StatusOK, &me)
	e.call(http.MethodGet, e.a("/v1/users/alice"), adminToken, nil, http.StatusOK, &u)
	if u.Sessions == nil || *u.Sessions != 0 {
		t.Errorf("sessions after the sign-out = %v, want 0", u.Sessions)
	}

	// Without the collaborator the route and the count are absent.
	bare := newEnv(t, config{})
	bare.apiErr(bare.send(req{method: http.MethodDelete, url: bare.a("/v1/users/alice/sessions"), token: adminToken}), http.StatusNotFound, api.CodeNotFound)
	bare.call(http.MethodGet, bare.a("/v1/users/alice"), adminToken, nil, http.StatusOK, &u)
	if u.Sessions != nil {
		t.Errorf("sessions without a session store = %v", *u.Sessions)
	}
}

func TestAdminUserAccountFields(t *testing.T) {
	e := newEnv(t, config{local: true})
	var invited api.Invited
	e.call(http.MethodPost, e.a("/v1/invites"), adminToken, api.InviteRequest{Handle: "carol"}, http.StatusCreated, &invited)

	byHandle := func() map[string]api.User {
		t.Helper()
		var users []api.User
		e.call(http.MethodGet, e.a("/v1/users"), adminToken, nil, http.StatusOK, &users)
		out := map[string]api.User{}
		for _, u := range users {
			out[u.Handle] = u
		}
		return out
	}
	users := byHandle()
	if c := users["carol"]; !c.Invited || c.Password || c.Keys != 0 || c.LastActiveAt != nil {
		t.Errorf("carol before she set a password = %+v", c)
	}
	// alice has the key the fixture issued, used by nothing yet.
	if a := users["alice"]; a.Invited || a.Password || a.Keys != 1 || a.LastActiveAt != nil {
		t.Errorf("alice = %+v", a)
	}

	if _, err := e.users.Redeem(e.ctx, linkToken(t, invited.Link.URL, "invite"), user.RedeemRequest{Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.CheckPassword(e.ctx, "carol", testPassword); err != nil {
		t.Fatal(err)
	}
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), e.alice, nil, http.StatusOK, &me)
	users = byHandle()
	if c := users["carol"]; c.Invited || !c.Password || c.LastActiveAt == nil || !c.LastActiveAt.Equal(t0) {
		t.Errorf("carol after her first sign-in = %+v", c)
	}
	if a := users["alice"]; a.LastActiveAt == nil || !a.LastActiveAt.Equal(t0) {
		t.Errorf("alice after using her key = %+v", a)
	}
	var one api.User
	e.call(http.MethodGet, e.a("/v1/users/carol"), adminToken, nil, http.StatusOK, &one)
	if !one.Password || one.Invited || one.LastActiveAt == nil {
		t.Errorf("GET carol = %+v", one)
	}

	// The list and the single user agree on usage, and a person who owns
	// nothing uses nothing.
	e.create(e.alice, "notes")
	users = byHandle()
	e.call(http.MethodGet, e.a("/v1/users/alice"), adminToken, nil, http.StatusOK, &one)
	if users["alice"].UsedBytes == 0 || users["alice"].UsedBytes != one.UsedBytes || users["carol"].UsedBytes != 0 {
		t.Errorf("used bytes: alice %d in the list and %d alone, carol %d", users["alice"].UsedBytes, one.UsedBytes, users["carol"].UsedBytes)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/users/nobody"), token: adminToken}), http.StatusNotFound, api.CodeNotFound)
}

// An invite link is for a person who has no password yet; one who has gets
// a reset link.
func TestAdminInviteLinkForAPersonWithAPassword(t *testing.T) {
	e := newEnv(t, config{local: true})
	var invited api.Invited
	e.call(http.MethodPost, e.a("/v1/invites"), adminToken, api.InviteRequest{Handle: "carol"}, http.StatusCreated, &invited)
	if _, err := e.users.Redeem(e.ctx, linkToken(t, invited.Link.URL, "invite"), user.RedeemRequest{Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	refused := e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/carol/links"), token: adminToken, contentType: "application/json",
		body: strings.NewReader(`{"purpose":"invite"}`)}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	if !strings.Contains(refused.Message, "reset link") {
		t.Errorf("message = %q, want it to name the reset link", refused.Message)
	}
	var link api.CreatedLink
	e.call(http.MethodPost, e.a("/v1/users/carol/links"), adminToken, api.LinkRequest{Purpose: "reset"}, http.StatusCreated, &link)
}

func TestKeyCapOnEveryRoute(t *testing.T) {
	e := newEnv(t, config{maxKeys: 2})
	var issued api.IssuedKey
	e.call(http.MethodPost, e.a("/v1/users/alice/keys"), adminToken, nil, http.StatusCreated, &issued)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/alice/keys"), token: adminToken}), http.StatusForbidden, api.CodeTooManyKeys)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/me/keys"), token: e.alice}), http.StatusForbidden, api.CodeTooManyKeys)
	// A rotation at the cap goes through: the key it replaces is not a key
	// more. A second one, while the first key's overlap runs, would be.
	var rotated api.IssuedKey
	e.call(http.MethodPost, e.a("/v1/users/alice/keys/"+issued.APIKey.Prefix+"/rotate"), adminToken, nil, http.StatusCreated, &rotated)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/users/alice/keys/" + rotated.APIKey.Prefix + "/rotate"), token: adminToken}),
		http.StatusForbidden, api.CodeTooManyKeys)
	// The cap counts live keys.
	for _, prefix := range []string{issued.APIKey.Prefix, rotated.APIKey.Prefix} {
		e.call(http.MethodDelete, e.a("/v1/users/alice/keys/"+prefix), adminToken, nil, http.StatusNoContent, nil)
	}
	e.call(http.MethodPost, e.u("/v1/me/keys"), e.alice, nil, http.StatusCreated, &issued)
}

// fakeBackup stands in for backup.Service.
type fakeBackup struct {
	mu      sync.Mutex
	err     error // returned before anything is written
	failMid error // returned after the first entry
	opts    []backup.WriteOptions
	actors  []string
}

func (f *fakeBackup) Write(ctx context.Context, w io.Writer, o backup.WriteOptions) (backup.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return backup.Summary{}, f.err
	}
	p, _ := auth.FromContext(ctx)
	f.opts, f.actors = append(f.opts, o), append(f.actors, p.Handle)
	tw := tar.NewWriter(w)
	files := map[string]string{"kazari.db": "the database", "blobs/ab/abcdef": "a blob"}
	if o.Certs {
		files["certmagic/key.pem"] = "the private key"
	}
	if f.failMid != nil {
		// More than the server buffers, so the response is on the wire
		// before the failure.
		files["kazari.db"] = strings.Repeat("x", 1<<16)
	}
	n := 0
	for _, name := range []string{"kazari.db", "blobs/ab/abcdef", "certmagic/key.pem"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			return backup.Summary{}, err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return backup.Summary{}, err
		}
		n++
		if f.failMid != nil {
			if err := tw.Flush(); err != nil {
				return backup.Summary{}, err
			}
			return backup.Summary{Files: n}, f.failMid
		}
	}
	return backup.Summary{Files: n}, tw.Close()
}

func TestAdminBackup(t *testing.T) {
	bk := &fakeBackup{}
	e := newEnv(t, config{backup: bk, admin: httpapi.AdminOptions{BackupName: "art.test"}})

	resp := e.send(req{method: http.MethodPost, url: e.a("/v1/backup"), token: adminToken})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("backup: status %d: %s", resp.StatusCode, b)
	}
	for name, want := range map[string]string{
		"Content-Type":        "application/x-tar",
		"Content-Disposition": `attachment; filename="kazari-art.test-20261024-120000.tar"`,
		"Cache-Control":       "no-store",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	dir := filepath.Join(t.TempDir(), "restore")
	if err := backup.Extract(resp.Body, dir); err != nil {
		t.Fatalf("Extract of the response: %v", err)
	}
	for name, want := range map[string]string{"kazari.db": "the database", "blobs/ab/abcdef": "a blob"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "certmagic")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the download holds certmagic/: %v", err)
	}
	if len(bk.opts) != 1 || bk.opts[0].Certs || bk.actors[0] != "admin" {
		t.Errorf("Write calls = %+v by %v", bk.opts, bk.actors)
	}

	// The private key leaves the host over the socket alone.
	apiErr := e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup?certs=true"), token: adminToken}), http.StatusForbidden, api.CodeForbidden)
	if !strings.Contains(apiErr.Message, "admin socket") || len(bk.opts) != 1 {
		t.Errorf("certs off the socket: %q, %d writes", apiErr.Message, len(bk.opts))
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup?certs=maybe"), token: adminToken}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup"), token: e.alice}), http.StatusForbidden, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup")}), http.StatusUnauthorized, api.CodeUnauthorized)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/backup"), token: adminToken}), http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	if len(bk.opts) != 1 {
		t.Errorf("refused requests wrote %d backups", len(bk.opts)-1)
	}

	local := httptestServer(t, httpapi.NewAdminAPI(httpapi.AdminDeps{Artifacts: e.art, Users: e.users, Backup: bk}, httpapi.AdminOptions{Local: true}))
	resp = e.send(req{method: http.MethodPost, url: local + "/v1/backup?certs=true"})
	dir = filepath.Join(t.TempDir(), "restore")
	if err := backup.Extract(resp.Body, dir); err != nil {
		t.Fatalf("Extract over the socket: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "certmagic", "key.pem")); err != nil || string(got) != "the private key" {
		t.Errorf("certmagic/key.pem over the socket = %q, %v", got, err)
	}
	if cd := resp.Header.Get("Content-Disposition"); !unnamedArchive.MatchString(cd) {
		t.Errorf("Content-Disposition without a name = %q", cd)
	}
}

var unnamedArchive = regexp.MustCompile(`^attachment; filename="kazari-\d{8}-\d{6}\.tar"$`)

func TestAdminBackupRefusals(t *testing.T) {
	bk := &fakeBackup{err: backup.ErrRunning}
	e := newEnv(t, config{backup: bk})
	e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup"), token: adminToken}), http.StatusConflict, api.CodeBackupRunning)

	bk.err = fmt.Errorf("%w: /data/tmp has 10 bytes free and the copy needs up to 4096", backup.ErrNoSpace)
	apiErr := e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup"), token: adminToken}), http.StatusInsufficientStorage, api.CodeNoSpace)
	if !strings.Contains(apiErr.Message, "10 bytes free") {
		t.Errorf("no_space message = %q, want the numbers", apiErr.Message)
	}

	bk.err = errors.New("disk on fire")
	if msg := e.apiErr(e.send(req{method: http.MethodPost, url: e.a("/v1/backup"), token: adminToken}), http.StatusInternalServerError, api.CodeInternal).Message; strings.Contains(msg, "fire") {
		t.Errorf("an internal failure reached the client: %q", msg)
	}

	// Once bytes are out, a failure cuts the transfer short: the client
	// must not take what it has for a whole archive.
	bk.err, bk.failMid = nil, errors.New("blob vanished")
	resp := e.send(req{method: http.MethodPost, url: e.a("/v1/backup"), token: adminToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: the stream had begun", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("a backup that failed mid-stream ended cleanly")
	}

	bare := newEnv(t, config{})
	bare.apiErr(bare.send(req{method: http.MethodPost, url: bare.a("/v1/backup"), token: adminToken}), http.StatusNotFound, api.CodeNotFound)
}

// fakeReady stands in for preflight.Checker.
type fakeReady struct {
	report preflight.Report
	fresh  []bool
}

func (f *fakeReady) Check(_ context.Context, fresh bool) (preflight.Report, error) {
	f.fresh = append(f.fresh, fresh)
	return f.report, nil
}

type fakeUpdates struct{ result update.Result }

func (f *fakeUpdates) Result() update.Result { return f.result }

// The update check's last answer comes with the readiness checks, once it
// has one.
func TestAdminReadyUpdate(t *testing.T) {
	up := &fakeUpdates{}
	e := newEnv(t, config{ready: &fakeReady{report: preflight.Report{At: t0}}, updates: up})
	var out api.Ready
	e.call(http.MethodGet, e.a("/v1/ready"), adminToken, nil, http.StatusOK, &out)
	if out.Update != nil {
		t.Errorf("update = %+v before the check has answered", out.Update)
	}
	up.result = update.Result{Latest: "0.8.0", Newer: true, CheckedAt: t0.Add(time.Hour)}
	out = api.Ready{}
	e.call(http.MethodGet, e.a("/v1/ready"), adminToken, nil, http.StatusOK, &out)
	if out.Update == nil || *out.Update != (api.Update{Latest: "0.8.0", Newer: true, CheckedAt: t0.Add(time.Hour)}) {
		t.Errorf("update = %+v", out.Update)
	}
}

func TestAdminReady(t *testing.T) {
	rd := &fakeReady{report: preflight.Report{At: t0, Findings: []preflight.Finding{
		{Check: "config", Status: preflight.OK, Detail: "valid"},
		{Check: "disk", Status: preflight.Warn, Detail: "8% free on /data; free some space or grow the disk"},
		{Check: "certificate", Status: preflight.Skip, Detail: "a proxy terminates TLS"},
	}}}
	e := newEnv(t, config{ready: rd})

	var out api.Ready
	e.call(http.MethodGet, e.a("/v1/ready"), adminToken, nil, http.StatusOK, &out)
	if !out.Ready || !out.CheckedAt.Equal(t0) || len(out.Checks) != 3 ||
		out.Checks[1] != (api.Check{Check: "disk", Status: "warn", Detail: "8% free on /data; free some space or grow the disk"}) {
		t.Errorf("ready = %+v", out)
	}

	// A failed check is still an answer: the body says so, with what to do.
	rd.report.Findings = append(rd.report.Findings, preflight.Finding{Check: "dns", Status: preflight.Fail,
		Detail: "*.art.test does not resolve; add the wildcard record"})
	e.call(http.MethodGet, e.a("/v1/ready?fresh=true"), adminToken, nil, http.StatusOK, &out)
	if out.Ready || len(out.Checks) != 4 || out.Checks[3].Status != "fail" || !strings.Contains(out.Checks[3].Detail, "add the wildcard record") {
		t.Errorf("ready with a failing check = %+v", out)
	}
	if len(rd.fresh) != 2 || rd.fresh[0] || !rd.fresh[1] {
		t.Errorf("fresh = %v, want false then true", rd.fresh)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/ready?fresh=now"), token: adminToken}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)

	// Without an update check the answer says nothing about one.
	if out.Update != nil {
		t.Errorf("update = %+v on an instance without the check", out.Update)
	}

	// The findings describe the configuration, so they need an admin.
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/ready")}), http.StatusUnauthorized, api.CodeUnauthorized)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.a("/v1/ready"), token: e.alice}), http.StatusForbidden, api.CodeForbidden)
	if len(rd.fresh) != 2 {
		t.Errorf("refused requests ran the checks: %v", rd.fresh)
	}

	// Liveness stays what it was: open, and nothing but ok and the version.
	for _, path := range []string{"/health", "/v1/health"} {
		resp := e.send(req{method: http.MethodGet, url: e.a(path)})
		raw, _ := io.ReadAll(resp.Body)
		var body map[string]any
		if err := jsonUnmarshal(raw, &body); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, %v: %s", path, resp.StatusCode, err, raw)
		}
		if body["status"] != "ok" || len(body) > 2 || (len(body) == 2 && body["version"] == nil) {
			t.Errorf("%s = %s, want only status and version", path, raw)
		}
	}

	bare := newEnv(t, config{})
	bare.apiErr(bare.send(req{method: http.MethodGet, url: bare.a("/v1/ready"), token: adminToken}), http.StatusNotFound, api.CodeNotFound)
}

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
