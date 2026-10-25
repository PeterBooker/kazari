package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"kazari/internal/auth"
	"kazari/internal/oauth"
	"kazari/internal/user"
)

// nonceHash is what an artifact host sends for its nonce cookie value.
func nonceHash(nonce string) string {
	h := sha256.Sum256([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// browserNonce is the value of the artifact host's nonce cookie: 43
// base64url characters, like the 32 random bytes the gate sets.
const browserNonce = "the-nonce-cookie-of-this-browser-0123456789"

func signInQuery(host, path string) string {
	return oauth.PathSignIn + "?" + url.Values{"host": {host}, "path": {path}, "nonce": {nonceHash(browserNonce)}}.Encode()
}

// hostCode checks that resp sends the browser to host's callback and
// returns the code.
func hostCode(t *testing.T, resp *http.Response, host string) string {
	t.Helper()
	loc := location(t, resp)
	if want := host + ".art.test"; loc.Host != want || loc.Path != "/.kazari/callback" {
		t.Fatalf("redirected to %s, want https://%s/.kazari/callback", loc, want)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", loc)
	}
	return code
}

// signInAt runs a browser sign-in for host, through the identity provider
// unless the browser is signed in already, and returns the code for the
// host.
func (e *env) signInAt(host, path string) string {
	e.t.Helper()
	resp, _ := e.get(signInQuery(host, path))
	loc := location(e.t, resp)
	if loc.Host != "idp.example" {
		return hostCode(e.t, resp, host)
	}
	resp, body := e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {loc.Query().Get("state")}}.Encode())
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	return hostCode(e.t, resp, host)
}

func TestSignInWithIdentityProvider(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	resp, _ := e.get(signInQuery("demo", "/report?q=1"))
	if loc := location(t, resp); loc.Host != "idp.example" || !strings.HasPrefix(loc.Query().Get("state"), "v.") {
		t.Fatalf("first sign-in went to %s, want the identity provider with a v. state", loc)
	}
	code := e.signInAt("demo", "/report?q=1")

	v, err := e.svc.Redeem(ctx, code, "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if v.Path != "/report?q=1" || !v.ExpiresAt.Equal(t0.Add(oauth.DefaultSessionTTL)) || v.Token == "" {
		t.Fatalf("view = %+v", v)
	}
	if got, err := e.svc.Viewer(ctx, "demo", v.Token); err != nil || got != "alice" {
		t.Fatalf("Viewer(demo) = %q, %v; want alice", got, err)
	}
	// The view session is for demo only.
	for _, host := range []string{"other", "demo--v2"} {
		if _, err := e.svc.Viewer(ctx, host, v.Token); !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("Viewer(%s) with demo's token = %v, want ErrNotFound", host, err)
		}
	}
	if _, err := e.svc.Redeem(ctx, code, "demo", browserNonce); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("second Redeem = %v, want ErrNotFound", err)
	}
	if ev := e.rec.actions(oauth.ActionSignIn); len(ev) != 1 || ev[0].Actor != "alice" || ev[0].Details["via"] != "oidc" {
		t.Fatalf("sign-in audit = %+v", ev)
	}

	// The next host signs in silently, from the session on the MCP host.
	resp, body := e.get(signInQuery("other", "/"))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("second host: status %d: %s", resp.StatusCode, body)
	}
	v2, err := e.svc.Redeem(ctx, hostCode(t, resp, "other"), "other", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Viewer(ctx, "other", v2.Token); err != nil || got != "alice" {
		t.Fatalf("Viewer(other) = %q, %v", got, err)
	}

	// Signing out on one host ends the browser's sign-in everywhere.
	if err := e.svc.SignOut(ctx, "other", v2.Token); err != nil {
		t.Fatal(err)
	}
	for host, tok := range map[string]string{"demo": v.Token, "other": v2.Token} {
		if _, err := e.svc.Viewer(ctx, host, tok); !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("Viewer(%s) after sign-out = %v", host, err)
		}
	}
	resp, _ = e.get(signInQuery("demo", "/"))
	if loc := location(t, resp); loc.Host != "idp.example" {
		t.Fatalf("after sign-out the sign-in went to %s, want the identity provider", loc)
	}
	if err := e.svc.SignOut(ctx, "demo", "unknown"); err != nil {
		t.Fatalf("SignOut of an unknown token = %v", err)
	}
}

func TestRedeemChecks(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	tests := []struct {
		name, host, nonce string
		wait              time.Duration
	}{
		{"another host", "other", browserNonce, 0},
		{"another browser's nonce", "demo", "someone-else", 0},
		{"no nonce", "demo", "", 0},
		{"expired code", "demo", browserNonce, oauth.DefaultViewCodeTTL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := e.signInAt("demo", "/")
			e.clock.Advance(tt.wait)
			if _, err := e.svc.Redeem(ctx, code, tt.host, tt.nonce); !errors.Is(err, oauth.ErrNotFound) {
				t.Fatalf("Redeem = %v, want ErrNotFound", err)
			}
			// A failed redemption uses the code up.
			if _, err := e.svc.Redeem(ctx, code, "demo", browserNonce); !errors.Is(err, oauth.ErrNotFound) {
				t.Fatalf("Redeem after a failure = %v, want ErrNotFound", err)
			}
		})
	}
	if _, err := e.svc.Redeem(ctx, "short", "demo", browserNonce); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("Redeem of a malformed code = %v", err)
	}
}

// A member who asks for a code bound to the hash of an empty nonce must
// not be able to hand its callback URL to a browser that has no nonce
// cookie and so sign that browser in as the member.
func TestRedeemRefusesEmptyNonce(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.signInAt("demo", "/") // the member's browser has a session
	for _, nonce := range []string{"", "short", strings.Repeat("!", 43)} {
		resp, body := e.get(oauth.PathSignIn + "?" + url.Values{"host": {"demo"}, "path": {"/"}, "nonce": {nonceHash(nonce)}}.Encode())
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("sign-in for nonce %q: status %d: %s", nonce, resp.StatusCode, body)
		}
		if _, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", nonce); !errors.Is(err, oauth.ErrNotFound) {
			t.Fatalf("Redeem with nonce %q = %v, want ErrNotFound", nonce, err)
		}
	}
}

func TestSignInRejectsBadTargets(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, q := range []url.Values{
		{"host": {"api"}, "path": {"/"}, "nonce": {nonceHash("n")}},
		{"host": {"evil.example"}, "path": {"/"}, "nonce": {nonceHash("n")}},
		{"host": {"demo"}, "path": {"/"}, "nonce": {"short"}},
		{"host": {"demo"}, "path": {"/"}},
	} {
		resp, body := e.get(oauth.PathSignIn + "?" + q.Encode())
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Invalid sign-in link") {
			t.Errorf("%v: status %d: %s", q, resp.StatusCode, body)
		}
	}
}

func TestReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/report?q=1":                   "/report?q=1",
		"/":                             "/",
		"":                              "/",
		"report":                        "/",
		"//evil.example/x":              "/",
		"/\\evil.example":               "/",
		"https://evil.example/":         "/",
		"/a\r\nSet-Cookie: x=1":         "/",
		"/x/../\\evil.example/":         "/",
		"/a\\b":                         "/",
		"/a/../b":                       "/a/../b",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		if got := oauth.ReturnPath(in); got != want {
			t.Errorf("ReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
	// An unsafe path is stored as "/", so the host returns to its root.
	e := newEnv(t, envOptions{})
	v, err := e.svc.Redeem(context.Background(), e.signInAt("demo", "//evil.example/"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if v.Path != "/" {
		t.Fatalf("Path = %q, want /", v.Path)
	}
}

// The callback completes only in the browser that started the sign-in, so
// an attacker cannot hand a victim a callback URL for the attacker's own
// identity-provider sign-in.
func TestSignInCallbackBoundToBrowser(t *testing.T) {
	e := newEnv(t, envOptions{})
	resp, _ := e.get(signInQuery("demo", "/"))
	state := location(t, resp).Query().Get("state")

	e.jar, _ = cookiejar.New(nil) // another browser
	e.client.Jar = e.jar
	resp, body := e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {state}}.Encode())
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "does not belong to this browser") {
		t.Fatalf("callback in another browser: status %d: %s", resp.StatusCode, body)
	}
}

// Several tabs signing in at once each complete, and a sign-in completes
// only once.
func TestSignInParallelTabs(t *testing.T) {
	e := newEnv(t, envOptions{})
	var states []string
	for _, host := range []string{"demo", "other"} {
		resp, _ := e.get(signInQuery(host, "/"))
		states = append(states, location(t, resp).Query().Get("state"))
	}
	for i, host := range []string{"demo", "other"} {
		resp, body := e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {states[i]}}.Encode())
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("callback %d: status %d: %s", i, resp.StatusCode, body)
		}
		if _, err := e.svc.Redeem(context.Background(), hostCode(t, resp, host), host, browserNonce); err != nil {
			t.Fatalf("Redeem for %s: %v", host, err)
		}
	}
	resp, body := e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {states[0]}}.Encode())
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Sign-in expired") {
		t.Fatalf("replayed callback: status %d: %s", resp.StatusCode, body)
	}
}

func TestSignInNotAdmitted(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.idp.setIdentity(auth.Identity{Issuer: "https://idp.example", Subject: "m", Email: "mallory@elsewhere.example", EmailVerified: true})
	resp, _ := e.get(signInQuery("demo", "/"))
	state := location(t, resp).Query().Get("state")
	resp, body := e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {state}}.Encode())
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "No access") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(e.rec.actions(oauth.ActionSignIn)) != 0 {
		t.Fatal("a refused person was signed in")
	}
}

func TestSignInIdentityProviderError(t *testing.T) {
	e := newEnv(t, envOptions{})
	resp, _ := e.get(signInQuery("demo", "/"))
	state := location(t, resp).Query().Get("state")
	resp, body := e.get("/oauth/callback?" + url.Values{"error": {"access_denied"}, "state": {state}}.Encode())
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign-in failed") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestViewSessionEnds(t *testing.T) {
	ctx := context.Background()
	t.Run("expiry", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		v, err := e.svc.Redeem(ctx, e.signInAt("demo", "/"), "demo", browserNonce)
		if err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(oauth.DefaultSessionTTL - time.Second)
		if _, err := e.svc.Viewer(ctx, "demo", v.Token); err != nil {
			t.Fatalf("before expiry: %v", err)
		}
		e.clock.Advance(time.Second)
		if _, err := e.svc.Viewer(ctx, "demo", v.Token); !errors.Is(err, oauth.ErrNotFound) {
			t.Fatalf("after expiry: %v", err)
		}
		resp, _ := e.get(signInQuery("demo", "/"))
		if loc := location(t, resp); loc.Host != "idp.example" {
			t.Fatalf("an expired session signed in silently: %s", loc)
		}
	})
	t.Run("disabled user", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		v, err := e.svc.Redeem(ctx, e.signInAt("demo", "/"), "demo", browserNonce)
		if err != nil {
			t.Fatal(err)
		}
		disable := true
		if _, err := e.users.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: &disable}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Viewer(ctx, "demo", v.Token); !errors.Is(err, oauth.ErrNotFound) {
			t.Fatalf("Viewer for a disabled user = %v", err)
		}
		// Enabling the user again does not bring the session back.
		enable := false
		if _, err := e.users.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: &enable}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Viewer(ctx, "demo", v.Token); !errors.Is(err, oauth.ErrNotFound) {
			t.Fatalf("Viewer after re-enabling = %v", err)
		}
	})
}

// keySignIn posts the API-key sign-in form as the browser would.
func (e *env) keySignIn(host, key string, header http.Header) (*http.Response, string) {
	e.t.Helper()
	v := url.Values{"host": {host}, "path": {"/"}, "nonce": {nonceHash(browserNonce)}, "api_key": {key}}
	req, err := http.NewRequest(http.MethodPost, e.url(oauth.PathSignIn), strings.NewReader(v.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	for k, vs := range header {
		req.Header[k] = vs
	}
	return e.do(req)
}

func TestSignInWithAPIKey(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true})
	ctx := context.Background()
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "bob"}); err != nil {
		t.Fatal(err)
	}
	k, key, err := e.users.IssueKey(ctx, "bob", "laptop", []string{auth.ScopeRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeOnly, err := e.users.IssueKey(ctx, "bob", "ci", []string{auth.ScopeWrite}, 0)
	if err != nil {
		t.Fatal(err)
	}

	resp, body := e.get(signInQuery("demo", "/"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="api_key"`) || !strings.Contains(body, "https://demo.art.test") {
		t.Fatalf("sign-in page: status %d: %s", resp.StatusCode, body)
	}
	// The form's redirect goes to the artifact host, so form-action allows it.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "https://demo.art.test") {
		t.Fatalf("CSP %q does not allow the redirect to the artifact host", csp)
	}

	for name, bad := range map[string]string{"wrong key": "kzk_nope", "key without read": writeOnly} {
		resp, body = e.keySignIn("demo", bad, nil)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "was not accepted") {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, body)
		}
	}
	resp, body = e.keySignIn("demo", key, http.Header{"Origin": {"https://evil.art.test"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Request rejected") {
		t.Fatalf("form from another origin: status %d: %s", resp.StatusCode, body)
	}

	resp, body = e.keySignIn("demo", key, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in: status %d: %s", resp.StatusCode, body)
	}
	v, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Viewer(ctx, "demo", v.Token); err != nil || got != "bob" {
		t.Fatalf("Viewer = %q, %v; want bob", got, err)
	}
	if ev := e.rec.actions(oauth.ActionSignIn); len(ev) != 1 || ev[0].Details["key_prefix"] != k.Prefix {
		t.Fatalf("sign-in audit = %+v", ev)
	}

	// The session lives only as long as the key.
	if err := e.users.RevokeKey(ctx, "bob", k.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Viewer(ctx, "demo", v.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("Viewer after the key was revoked = %v", err)
	}
	resp, _ = e.get(signInQuery("other", "/"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a session of a revoked key signed in silently: status %d", resp.StatusCode)
	}
}

// A session opened with an expiring key ends with the key.
func TestSignInWithExpiringKey(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true})
	ctx := context.Background()
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "bob"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := e.users.IssueKey(ctx, "bob", "", []string{auth.ScopeRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := e.keySignIn("demo", key, nil)
	v, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want the key's expiry", v.ExpiresAt)
	}
}

// With an identity provider the API-key form is refused.
func TestSignInKeyFormNeedsNoIdentityProvider(t *testing.T) {
	e := newEnv(t, envOptions{})
	resp, body := e.keySignIn("demo", "kzk_whatever", nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign-in required") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}
