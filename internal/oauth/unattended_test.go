package oauth_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

const reuseWindow = 30 * time.Second

// grantID is the id of the grant an access token belongs to, as the audit
// log names it.
func (e *env) grantID(access string) string {
	e.t.Helper()
	p, err := e.svc.Authenticate(context.Background(), access)
	if err != nil {
		e.t.Fatalf("Authenticate: %v", err)
	}
	return strings.TrimPrefix(p.KeyID, "oauth:")
}

func (e *env) live(access string) bool {
	_, err := e.svc.Authenticate(context.Background(), access)
	return err == nil
}

// wantReplay checks that tr refused a replayed refresh token and that the
// whole grant is gone and recorded as revoked for that reason.
func (e *env) wantReplay(tr tokenResult, clientID, grant string, pairs ...tokenResult) {
	e.t.Helper()
	if tr.status != http.StatusBadRequest || tr.Error != "invalid_grant" {
		e.t.Errorf("replay = %d %q, want invalid_grant", tr.status, tr.Error)
	}
	for i, p := range pairs {
		if e.live(p.AccessToken) {
			e.t.Errorf("access token %d works after the replay", i)
		}
		if r := e.refresh(clientID, p.RefreshToken, ""); r.Error != "invalid_grant" {
			e.t.Errorf("refresh token %d after the replay = %d %q, want invalid_grant", i, r.status, r.Error)
		}
	}
	var mine int
	for _, ev := range e.rec.actions(oauth.ActionRevoke) {
		if ev.Details["reason"] == oauth.ReasonRefreshReplay && ev.Details["grant"] == grant &&
			ev.Details["client_id"] == clientID && ev.Actor == "alice" {
			mine++
		}
	}
	if mine == 0 {
		e.t.Errorf("no oauth.revoke with reason %s for grant %s: %+v", oauth.ReasonRefreshReplay, grant, e.rec.actions(oauth.ActionRevoke))
	}
}

func TestRefreshReuseWindow(t *testing.T) {
	// rotated returns an environment whose first refresh token was rotated
	// once: the client that holds first never saw second.
	rotated := func(t *testing.T, window time.Duration) (e *env, clientID string, first, second tokenResult) {
		t.Helper()
		e = newEnv(t, envOptions{reuseWindow: window})
		clientID, first = e.flow("")
		e.clock.Advance(time.Hour)
		second = e.refresh(clientID, first.RefreshToken, "")
		if second.status != http.StatusOK {
			t.Fatalf("refresh = %d %s", second.status, second.Error)
		}
		return e, clientID, first, second
	}

	t.Run("a second use inside the window forks", func(t *testing.T) {
		e, clientID, first, second := rotated(t, reuseWindow)
		grant := e.grantID(second.AccessToken)
		e.clock.Advance(reuseWindow - time.Second)
		third := e.refresh(clientID, first.RefreshToken, "")
		if third.status != http.StatusOK {
			t.Fatalf("second use inside the window = %d %s: %s", third.status, third.Error, third.ErrorDescription)
		}
		if third.RefreshToken == second.RefreshToken || third.AccessToken == second.AccessToken ||
			third.RefreshToken == first.RefreshToken {
			t.Error("the second use returned a pair that was issued before")
		}
		if got := e.grantID(third.AccessToken); got != grant {
			t.Errorf("the new pair belongs to grant %s, want %s", got, grant)
		}
		if !e.live(second.AccessToken) {
			t.Error("the first successor's access token was revoked")
		}
		if len(e.rec.actions(oauth.ActionRevoke)) != 0 {
			t.Errorf("a reuse inside the window was recorded as a revoke: %+v", e.rec.actions(oauth.ActionRevoke))
		}
		grants, err := e.svc.ListGrants(context.Background(), "alice")
		if err != nil || len(grants) != 1 || fmt.Sprint(grants[0].FamilyID) != grant {
			t.Errorf("ListGrants = %+v, %v; want the one grant %s", grants, err, grant)
		}
		// Both branches go on: the first successor was left alone.
		if r := e.refresh(clientID, second.RefreshToken, ""); r.status != http.StatusOK {
			t.Errorf("the first successor after the fork = %d %s", r.status, r.Error)
		}
		if r := e.refresh(clientID, third.RefreshToken, ""); r.status != http.StatusOK {
			t.Errorf("the forked token = %d %s", r.status, r.Error)
		}
	})

	t.Run("a third use is a replay", func(t *testing.T) {
		e, clientID, first, second := rotated(t, reuseWindow)
		grant := e.grantID(second.AccessToken)
		third := e.refresh(clientID, first.RefreshToken, "")
		if third.status != http.StatusOK {
			t.Fatalf("second use = %d %s", third.status, third.Error)
		}
		e.wantReplay(e.refresh(clientID, first.RefreshToken, ""), clientID, grant, first, second, third)
	})

	for name, wait := range map[string]time.Duration{"at the edge of the window": reuseWindow, "after the window": reuseWindow + time.Second} {
		t.Run(name, func(t *testing.T) {
			e, clientID, first, second := rotated(t, reuseWindow)
			grant := e.grantID(second.AccessToken)
			e.clock.Advance(wait)
			e.wantReplay(e.refresh(clientID, first.RefreshToken, ""), clientID, grant, first, second)
		})
	}

	t.Run("after the successor was itself rotated", func(t *testing.T) {
		e, clientID, first, second := rotated(t, reuseWindow)
		grant := e.grantID(second.AccessToken)
		third := e.refresh(clientID, second.RefreshToken, "")
		if third.status != http.StatusOK {
			t.Fatalf("refresh = %d %s", third.status, third.Error)
		}
		// first is now the second token back, whatever the clock says.
		e.wantReplay(e.refresh(clientID, first.RefreshToken, ""), clientID, grant, first, second, third)
	})

	t.Run("the last rotated token still forks", func(t *testing.T) {
		e, clientID, _, second := rotated(t, reuseWindow)
		third := e.refresh(clientID, second.RefreshToken, "")
		again := e.refresh(clientID, second.RefreshToken, "")
		if third.status != http.StatusOK || again.status != http.StatusOK || again.RefreshToken == third.RefreshToken {
			t.Errorf("refresh = %d %s, its repeat = %d %s", third.status, third.Error, again.status, again.Error)
		}
	})

	t.Run("a revoked family stays revoked", func(t *testing.T) {
		e, clientID, first, second := rotated(t, reuseWindow)
		grants, err := e.svc.ListGrants(context.Background(), "alice")
		if err != nil || len(grants) != 1 {
			t.Fatalf("ListGrants = %+v, %v", grants, err)
		}
		if err := e.svc.RevokeGrant(context.Background(), grants[0].FamilyID); err != nil {
			t.Fatal(err)
		}
		tr := e.refresh(clientID, first.RefreshToken, "")
		if tr.status != http.StatusBadRequest || tr.Error != "invalid_grant" || tr.AccessToken != "" {
			t.Errorf("use inside the window of a revoked family = %d %q, want invalid_grant", tr.status, tr.Error)
		}
		if e.live(second.AccessToken) || e.live(first.AccessToken) {
			t.Error("a token of the revoked family works again")
		}
		if r := e.refresh(clientID, second.RefreshToken, ""); r.Error != "invalid_grant" {
			t.Errorf("the successor after the revoke = %d %q, want invalid_grant", r.status, r.Error)
		}
	})

	t.Run("a disabled user gets nothing", func(t *testing.T) {
		e, clientID, first, _ := rotated(t, reuseWindow)
		disabled := true
		if _, err := e.users.UpdateUser(context.Background(), "alice", user.UpdateUserRequest{Disabled: &disabled}); err != nil {
			t.Fatal(err)
		}
		if tr := e.refresh(clientID, first.RefreshToken, ""); tr.Error != "invalid_grant" || tr.AccessToken != "" {
			t.Errorf("reuse by a disabled user = %d %q, want invalid_grant", tr.status, tr.Error)
		}
	})

	t.Run("two uses at once", func(t *testing.T) {
		e := newEnv(t, envOptions{reuseWindow: reuseWindow})
		clientID, first := e.flow("")
		var (
			wg  sync.WaitGroup
			got [2]tokenResult
		)
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i] = e.refresh(clientID, first.RefreshToken, "")
			}()
		}
		wg.Wait()
		for i, tr := range got {
			if tr.status != http.StatusOK || !e.live(tr.AccessToken) {
				t.Errorf("use %d = %d %s: %s", i, tr.status, tr.Error, tr.ErrorDescription)
			}
		}
		if got[0].RefreshToken == got[1].RefreshToken || got[0].AccessToken == got[1].AccessToken {
			t.Error("both uses got the same pair")
		}
		if len(e.rec.actions(oauth.ActionRevoke)) != 0 {
			t.Error("two uses at once revoked the grant")
		}
	})

	t.Run("a window of 0 accepts no second use", func(t *testing.T) {
		e, clientID, first, second := rotated(t, 0)
		grant := e.grantID(second.AccessToken)
		e.wantReplay(e.refresh(clientID, first.RefreshToken, ""), clientID, grant, first, second)
	})
}

// keyApproval signs alice in at the identity provider, issues her a key and
// approves the client's request with it.
func keyApproval(t *testing.T, req user.IssueKeyRequest) (e *env, clientID string, k *user.APIKey, tr tokenResult) {
	t.Helper()
	ctx := context.Background()
	e = newEnv(t, envOptions{})
	const redirect = "https://app.example/cb"
	clientID = e.register(redirect)
	p := newPKCE("verifier")
	page := e.signIn(authorizeParams(clientID, redirect, "", p))
	if !strings.Contains(page, "Approve with an API key instead") || !strings.Contains(page, `name="api_key"`) {
		t.Fatalf("consent page offers no API key:\n%s", page)
	}
	req.Handle = "alice"
	k, key, err := e.users.IssueKey(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	code := e.approve(url.Values{"api_key": {" " + key + "\n"}})
	tr = e.exchange(clientID, redirect, code, p)
	if tr.status != http.StatusOK {
		t.Fatalf("exchange = %d %s: %s", tr.status, tr.Error, tr.ErrorDescription)
	}
	return e, clientID, k, tr
}

func TestConsentWithKeyAfterSignIn(t *testing.T) {
	ctx := context.Background()

	t.Run("the grant is the key over OAuth", func(t *testing.T) {
		e, _, k, tr := keyApproval(t, user.IssueKeyRequest{Label: "weekly",
			Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"}, ExpiresIn: 90 * time.Minute})
		if tr.Scope != "artifacts:read artifacts:write" {
			t.Errorf("scope = %q, want the key's", tr.Scope)
		}
		p, err := e.svc.Authenticate(ctx, tr.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		want := auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Source: auth.SourceOAuth,
			KeyID: p.KeyID, ClientID: p.ClientID, Slugs: []string{"weekly-*"}, ExpiresAt: k.ExpiresAt}
		if diff := cmp.Diff(want, p); diff != "" {
			t.Errorf("Principal (-want +got):\n%s", diff)
		}
		g := e.rec.actions(oauth.ActionGrant)
		if len(g) != 1 || g[0].Actor != "alice" || g[0].Details["via"] != "api_key" || g[0].Details["key_prefix"] != k.Prefix ||
			g[0].Details["scopes"] != "artifacts:read artifacts:write" {
			t.Errorf("grant audit = %+v", g)
		}
		grants, err := e.svc.ListGrants(ctx, "alice")
		if err != nil || len(grants) != 1 || grants[0].KeyPrefix != k.Prefix || !grants[0].ExpiresAt.Equal(*k.ExpiresAt) {
			t.Errorf("grants = %+v, %v; want one of key %s ending with it", grants, err, k.Prefix)
		}
	})

	t.Run("a key that expires mid-connection", func(t *testing.T) {
		e, clientID, k, tr := keyApproval(t, user.IssueKeyRequest{ExpiresIn: 90 * time.Minute})
		e.clock.Advance(time.Hour)
		next := e.refresh(clientID, tr.RefreshToken, "")
		if next.status != http.StatusOK || next.ExpiresIn != 1800 {
			t.Fatalf("refresh before the key's expiry = %d %s, expires_in %d; want 200 and 1800", next.status, next.Error, next.ExpiresIn)
		}
		e.clock.Advance(k.ExpiresAt.Sub(e.clock.Now()))
		if e.live(next.AccessToken) {
			t.Error("the access token outlived the key")
		}
		if r := e.refresh(clientID, next.RefreshToken, ""); r.Error != "invalid_grant" {
			t.Errorf("refresh at the key's expiry = %d %q, want invalid_grant", r.status, r.Error)
		}
	})

	t.Run("a key revoked between two refreshes", func(t *testing.T) {
		e, clientID, k, tr := keyApproval(t, user.IssueKeyRequest{})
		e.clock.Advance(time.Hour)
		next := e.refresh(clientID, tr.RefreshToken, "")
		if next.status != http.StatusOK {
			t.Fatalf("refresh = %d %s", next.status, next.Error)
		}
		if err := e.users.RevokeKey(ctx, user.KeyRef{Handle: "alice", Prefix: k.Prefix}); err != nil {
			t.Fatal(err)
		}
		if e.live(next.AccessToken) {
			t.Error("the access token works after its key was revoked")
		}
		if r := e.refresh(clientID, next.RefreshToken, ""); r.Error != "invalid_grant" {
			t.Errorf("refresh after the key was revoked = %d %q, want invalid_grant", r.status, r.Error)
		}
	})

	t.Run("the key it was issued with is revoked", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		const redirect = "https://app.example/cb"
		clientID := e.register(redirect)
		p := newPKCE("verifier")
		e.signIn(authorizeParams(clientID, redirect, "", p))
		laptop, _, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		_, agentKey, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "alice", With: laptop.Prefix, Slugs: []string{"weekly-*"}})
		if err != nil {
			t.Fatal(err)
		}
		tr := e.exchange(clientID, redirect, e.approve(url.Values{"api_key": {agentKey}}), p)
		if !e.live(tr.AccessToken) {
			t.Fatalf("exchange = %d %s", tr.status, tr.Error)
		}
		if err := e.users.RevokeKey(ctx, user.KeyRef{Handle: "alice", Prefix: laptop.Prefix}); err != nil {
			t.Fatal(err)
		}
		if e.live(tr.AccessToken) {
			t.Error("the connection outlived the key its key was issued with")
		}
	})

	t.Run("a demoted owner", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		e.idp.setIdentity(auth.Identity{Issuer: "https://idp.example", Subject: "boss-sub", Email: "boss@example.com", EmailVerified: true})
		const redirect = "https://app.example/cb"
		clientID := e.register(redirect)
		p := newPKCE("verifier")
		e.signIn(authorizeParams(clientID, redirect, "", p))
		_, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Scopes: auth.AllScopes})
		if err != nil {
			t.Fatal(err)
		}
		tr := e.exchange(clientID, redirect, e.approve(url.Values{"api_key": {key}}), p)
		pr, err := e.svc.Authenticate(ctx, tr.AccessToken)
		if err != nil || pr.Admin || pr.HasScope(auth.ScopeAdmin) || !cmp.Equal(pr.Scopes, auth.MemberScopes) {
			t.Errorf("a connection approved with an admin key: %+v, %v; want the member scopes", pr, err)
		}
	})

	t.Run("a key that is refused", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		const redirect = "https://app.example/cb"
		clientID := e.register(redirect)
		p := newPKCE("verifier")
		e.signIn(authorizeParams(clientID, redirect, "", p))
		if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol"}); err != nil {
			t.Fatal(err)
		}
		_, carols, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "carol"})
		if err != nil {
			t.Fatal(err)
		}
		revokedKey, revoked, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.users.RevokeKey(ctx, user.KeyRef{Handle: "alice", Prefix: revokedKey.Prefix}); err != nil {
			t.Fatal(err)
		}
		for name, key := range map[string]string{"another user's key": carols, "a revoked key": revoked, "no key at all": "kzk_nope"} {
			resp, body := e.postForm("/oauth/consent", url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}, "api_key": {key}})
			if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "That API key was not accepted.") ||
				!strings.Contains(body, "<details open") || !strings.Contains(body, "<strong>alice</strong>") {
				t.Errorf("%s: status %d:\n%s", name, resp.StatusCode, body)
			}
		}
		if g := e.rec.actions(oauth.ActionGrant); len(g) != 0 {
			t.Errorf("a refused key was granted: %+v", g)
		}
		// The request is still pending: the person can approve as themselves.
		tr := e.exchange(clientID, redirect, e.approve(nil), p)
		pr, err := e.svc.Authenticate(ctx, tr.AccessToken)
		if err != nil || pr.Handle != "alice" || len(pr.Slugs) != 0 || pr.ExpiresAt != nil {
			t.Errorf("approval without a key: %+v, %v", pr, err)
		}
		if g := e.rec.actions(oauth.ActionGrant); len(g) != 1 || g[0].Details["via"] != "oidc" || g[0].Details["key_prefix"] != "" {
			t.Errorf("grant audit = %+v", g)
		}
	})
}

func TestConsentWithRestrictedKeyWithoutProvider(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{noIDP: true})
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol"}); err != nil {
		t.Fatal(err)
	}
	k, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "carol", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*", "report"}, ExpiresIn: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "http://localhost/callback"
	clientID := e.register(redirect)
	p := newPKCE("apikey")
	e.signIn(authorizeParams(clientID, redirect, "", p))
	tr := e.exchange(clientID, redirect, e.approve(url.Values{"api_key": {key}}), p)
	pr, err := e.svc.Authenticate(ctx, tr.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal(pr.Slugs, k.Slugs) || !pr.ExpiresAt.Equal(*k.ExpiresAt) || !cmp.Equal(pr.Scopes, []string{auth.ScopeRead, auth.ScopeWrite}) {
		t.Errorf("principal = %+v, want the key's scopes, slugs and expiry", pr)
	}
	// The restriction is read from the key on every request and after every refresh.
	e.clock.Advance(time.Hour)
	next := e.refresh(clientID, tr.RefreshToken, "")
	if pr, err := e.svc.Authenticate(ctx, next.AccessToken); err != nil || !cmp.Equal(pr.Slugs, k.Slugs) {
		t.Errorf("principal after a refresh = %+v, %v", pr, err)
	}
}

// A browser session opens every members page, so a key restricted to some
// artifacts must not start one.
func TestRestrictedKeyOpensNoBrowserSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{noIDP: true})
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "bob"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "bob", Slugs: []string{"demo"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := e.keySignIn("demo", key, nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "restricted to certain pages") ||
		!strings.Contains(body, `name="api_key"`) {
		t.Fatalf("sign-in with a restricted key: status %d:\n%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Location") != "" || len(resp.Cookies()) != 0 {
		t.Errorf("a refused sign-in redirected to %q or set cookies %v", resp.Header.Get("Location"), resp.Cookies())
	}
	if ev := e.rec.actions(oauth.ActionSignIn); len(ev) != 0 {
		t.Errorf("a refused sign-in was recorded: %+v", ev)
	}
	// The same person's unrestricted key signs in.
	_, full, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "bob", Scopes: []string{auth.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := e.keySignIn("demo", full, nil); resp.StatusCode != http.StatusFound {
		t.Errorf("sign-in with an unrestricted key: status %d:\n%s", resp.StatusCode, body)
	}
}

// The key typed into the consent page must not reach a log line or an
// audit row, whether it is accepted or not.
func TestConsentKeyStaysOutOfLogsAndAudit(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newEnv(t, envOptions{noIDP: true, logger: logger})
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "carol"}); err != nil {
		t.Fatal(err)
	}
	k, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "carol", Slugs: []string{"weekly-*"}})
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "http://localhost/callback"
	clientID := e.register(redirect)
	p := newPKCE("apikey")
	e.signIn(authorizeParams(clientID, redirect, "", p))
	wrong := key[:len(key)-1] + "2"
	if wrong == key {
		wrong = key[:len(key)-1] + "3"
	}
	e.postForm("/oauth/consent", url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}, "api_key": {wrong}})
	tr := e.exchange(clientID, redirect, e.approve(url.Values{"api_key": {key}}), p)
	e.refresh(clientID, tr.RefreshToken, "")
	e.keySignIn("demo", key, nil)

	_, secret, _ := user.ParseKey(key)
	_, wrongSecret, _ := user.ParseKey(wrong)
	mu.Lock()
	logged := logs.String()
	mu.Unlock()
	rows := fmt.Sprintf("%+v", e.rec.actions(oauth.ActionGrant)) + fmt.Sprintf("%+v", e.rec.actions(oauth.ActionSignIn)) +
		fmt.Sprintf("%+v", e.rec.actions(oauth.ActionRevoke))
	if !strings.Contains(rows, k.Prefix) {
		t.Fatalf("the audit rows do not name the key's prefix: %s", rows)
	}
	for name, text := range map[string]string{"log": logged, "audit": rows} {
		for _, s := range []string{secret, wrongSecret, user.HashKey(key), tr.AccessToken, tr.RefreshToken} {
			if strings.Contains(text, s) {
				t.Errorf("the %s holds a secret: %s", name, text)
			}
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
