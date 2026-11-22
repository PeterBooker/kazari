package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

const password = "correct horse battery"

// person invites handle and sets their password through the link.
func (e *env) person(handle, email string, role user.Role) {
	e.t.Helper()
	ctx := auth.ContextWithPrincipal(context.Background(), auth.Principal{Handle: "admin", Admin: true})
	_, _, token, err := e.users.Invite(ctx, user.InviteRequest{Handle: handle, Email: email, Role: role})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.users.Redeem(ctx, token, user.RedeemRequest{Password: password}); err != nil {
		e.t.Fatal(err)
	}
}

// passwordSignIn posts the password form for host.
func (e *env) passwordSignIn(host, login, pw string) (*http.Response, string) {
	e.t.Helper()
	return e.signInForm(url.Values{"host": {host}, "path": {"/"}, "nonce": {nonceHash(browserNonce)}, "login": {login}, "password": {pw}})
}

func (e *env) signInForm(v url.Values) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url(oauth.PathSignIn), strings.NewReader(v.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	return e.do(req)
}

func TestSignInWithPassword(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("ana", "ana@example.org", "")

	resp, body := e.get(signInQuery("demo", "/report"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) || strings.Contains(body, `name="api_key"`) {
		t.Fatalf("sign-in page: status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`autocomplete="current-password"`, `autocomplete="username"`, "Use an API key instead", "with=key", "https://demo.art.test",
		`href="/oauth/recover?host=demo&amp;nonce=`} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "https://demo.art.test") {
		t.Errorf("CSP %q does not allow the redirect to the artifact host", csp)
	}

	resp, body = e.passwordSignIn("demo", "ana@example.org", password)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in: status %d: %s", resp.StatusCode, body)
	}
	v, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Viewer(ctx, "demo", v.Token); err != nil || got != "ana" {
		t.Fatalf("Viewer = %q, %v; want ana", got, err)
	}
	if !v.ExpiresAt.Equal(t0.Add(oauth.DefaultSessionTTL)) {
		t.Errorf("session ends at %v, want 12 hours after the sign-in: no key bounds it", v.ExpiresAt)
	}
	ev := e.rec.actions(oauth.ActionSignIn)
	if len(ev) != 1 || ev[0].Actor != "ana" || ev[0].Details["via"] != "password" || ev[0].Details["key_prefix"] != "" {
		t.Fatalf("sign-in audit = %+v", ev)
	}
	if resp, _ := e.get(oauth.PathRecover); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://art.test/account/recover" {
		t.Errorf("recovery link: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// The browser is signed in: the next host needs no form.
	resp, _ = e.get(signInQuery("other", "/"))
	hostCode(t, resp, "other")
}

// The API-key form stays one link away.
func TestSignInKeyFormBesidePasswords(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("ana", "", "")
	_, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := e.get(signInQuery("demo", "/") + "&with=key")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="api_key"`) || strings.Contains(body, `name="password"`) ||
		!strings.Contains(body, "Use your password instead") {
		t.Fatalf("key form: status %d: %s", resp.StatusCode, body)
	}
	resp, body = e.keySignIn("demo", key, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("key sign-in: status %d: %s", resp.StatusCode, body)
	}
	hostCode(t, resp, "demo")
}

// A wrong password, an unknown name and an account without a password get
// one answer.
func TestPasswordRefusalsLookAlike(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "ana@example.org", "")
	if _, err := e.users.CreateUser(context.Background(), user.CreateUserRequest{Handle: "keyonly"}); err != nil {
		t.Fatal(err)
	}
	answers := map[string]string{}
	for name, login := range map[string]string{"wrong password": "ana", "unknown handle": "nobody", "unknown email": "x@example.org", "no password": "keyonly"} {
		resp, body := e.passwordSignIn("demo", login, "not the password at all")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "That name or password was not accepted.") {
			t.Fatalf("%s: status %d: %s", name, resp.StatusCode, body)
		}
		if strings.Contains(body, "not the password at all") {
			t.Errorf("%s: the page shows the password again", name)
		}
		answers[name] = strings.ReplaceAll(body, login, "LOGIN")
	}
	for name, body := range answers {
		if body != answers["wrong password"] {
			t.Errorf("%s is answered differently from a wrong password", name)
		}
	}
	if ev := e.rec.actions(oauth.ActionSignIn); len(ev) != 0 {
		t.Errorf("refused sign-ins were recorded: %+v", ev)
	}
	resp, body := e.passwordSignIn("demo", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty form: status %d: %s", resp.StatusCode, body)
	}
}

func TestPasswordDelays(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	wrong := func(login string) int {
		t.Helper()
		resp, _ := e.passwordSignIn("demo", login, "a wrong password")
		return resp.StatusCode
	}
	// Three failures are free.
	for i := range 3 {
		if got := wrong("ana"); got != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, got)
		}
	}
	if got := wrong("ana"); got != http.StatusUnauthorized {
		t.Fatalf("the fourth attempt comes before any delay is due: status %d", got)
	}
	// From the fourth failure the next attempt waits, the right password
	// included, and a name written another way is the same account.
	for _, login := range []string{"ana", " ANA "} {
		resp, body := e.passwordSignIn("demo", login, password)
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" || !strings.Contains(body, "Try again in a second") {
			t.Fatalf("%q during the delay: status %d, Retry-After %q: %s", login, resp.StatusCode, resp.Header.Get("Retry-After"), body)
		}
	}
	// Another account is not delayed by this one.
	if got := wrong("ben"); got != http.StatusUnauthorized {
		t.Errorf("another account during ana's delay: status %d, want 401", got)
	}
	// The wait doubles with each failure, and never passes a minute.
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute} {
		e.clock.Advance(want - time.Millisecond)
		if got := wrong("ana"); got != http.StatusTooManyRequests {
			t.Fatalf("just before %s have passed: status %d, want 429", want, got)
		}
		e.clock.Advance(time.Millisecond)
		if got := wrong("ana"); got != http.StatusUnauthorized {
			t.Fatalf("once %s have passed: status %d, want 401", want, got)
		}
	}
	// Never a lockout: after the wait the right password signs in, and the
	// count starts again.
	e.clock.Advance(time.Minute)
	resp, body := e.passwordSignIn("demo", "ana", password)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the right password after the wait: status %d: %s", resp.StatusCode, body)
	}
	if got := wrong("ana"); got != http.StatusUnauthorized {
		t.Fatalf("first failure after a sign-in: status %d, want 401", got)
	}
	// Every request here comes from one address, which by now owes eight
	// seconds of its own; the account owes nothing, where a count that had
	// gone on would owe a minute.
	e.clock.Advance(8 * time.Second)
	if got := wrong("ana"); got != http.StatusUnauthorized {
		t.Fatalf("second failure after a sign-in: status %d, want 401: the account's count did not start again", got)
	}
}

// One address guessing across many accounts is slowed too.
func TestPasswordDelaysPerAddress(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	for i := range 11 {
		resp, _ := e.passwordSignIn("demo", "guess"+strconv.Itoa(i), "a wrong password")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("guess %d: status %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp, _ := e.passwordSignIn("demo", "ana", password)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("an account never guessed at, from the guessing address: status %d, want 429", resp.StatusCode)
	}
	e.clock.Advance(time.Second)
	if resp, body := e.passwordSignIn("demo", "ana", password); resp.StatusCode != http.StatusFound {
		t.Fatalf("after the wait: status %d: %s", resp.StatusCode, body)
	}
}

func TestPasswordFormRefusedWithIdentityProvider(t *testing.T) {
	e := newEnv(t, envOptions{passwords: true})
	e.person("ana", "", "")
	resp, body := e.passwordSignIn("demo", "ana", password)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign-in required") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	resp, _ = e.get(signInQuery("demo", "/"))
	if loc := location(t, resp); loc.Host != "idp.example" {
		t.Errorf("sign-in went to %s, want the identity provider", loc)
	}
}

func TestPasswordFormFromAnotherOrigin(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	v := url.Values{"host": {"demo"}, "path": {"/"}, "nonce": {nonceHash(browserNonce)}, "login": {"ana"}, "password": {password}}
	req, _ := http.NewRequest(http.MethodPost, e.url(oauth.PathSignIn), strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://demo.art.test")
	resp, body := e.do(req)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Request rejected") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestControlHosts(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("boss", "", user.RoleAdmin)

	resp, body := e.get(signInQuery(oauth.HostAdmin, "/people"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sign in to the dashboard") {
		t.Fatalf("sign-in page for the dashboard: status %d: %s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "https://admin.art.test") {
		t.Errorf("CSP %q does not allow the redirect to the admin host", csp)
	}
	resp, body = e.signInForm(url.Values{"host": {oauth.HostAdmin}, "path": {"/people"}, "nonce": {nonceHash(browserNonce)},
		"login": {"boss"}, "password": {password}})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in: status %d: %s", resp.StatusCode, body)
	}
	loc := location(t, resp)
	if loc.Scheme+"://"+loc.Host+loc.Path != "https://admin.art.test/session/callback" {
		t.Fatalf("redirected to %s, want the configured callback of the admin host", loc)
	}
	code := loc.Query().Get("code")

	// A code is for one host and one browser.
	if _, err := e.svc.Redeem(ctx, code, oauth.HostAccount, browserNonce); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("the admin host's code redeemed on the apex: %v", err)
	}
	resp, _ = e.get(signInQuery(oauth.HostAdmin, "/people"))
	code = location(t, resp).Query().Get("code")
	v, err := e.svc.Redeem(ctx, code, oauth.HostAdmin, browserNonce)
	if err != nil || v.Path != "/people" {
		t.Fatalf("Redeem = %+v, %v", v, err)
	}
	cs, err := e.svc.ControlSession(ctx, oauth.HostAdmin, v.Token)
	if err != nil || cs.Handle != "boss" || cs.KeyPrefix != "" || cs.SessionID == 0 || !cs.ExpiresAt.Equal(v.ExpiresAt) {
		t.Fatalf("ControlSession = %+v, %v", cs, err)
	}
	for _, host := range []string{oauth.HostAccount, "demo", "admin", ""} {
		if _, err := e.svc.ControlSession(ctx, host, v.Token); !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("the admin host's cookie on %q: err = %v, want ErrNotFound", host, err)
		}
	}

	// A disabled person's session is gone on the next request.
	e.person("second", "", user.RoleAdmin)
	if _, err := e.users.UpdateUser(ctx, "boss", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ControlSession(ctx, oauth.HostAdmin, v.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("ControlSession of a disabled person: err = %v, want ErrNotFound", err)
	}
}

func TestSignInRejectsUnknownControlHosts(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	for _, host := range []string{"@evil", "@", "@admin.evil.example", "admin", "@ADMIN", "https://evil.example"} {
		resp, body := e.get(signInQuery(host, "/"))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("host %q: status %d: %s", host, resp.StatusCode, body)
		}
	}
	// The callback comes from the configuration: no header of the request
	// moves it.
	req, _ := http.NewRequest(http.MethodGet, e.url(signInQuery(oauth.HostAdmin, "/")), nil)
	req.Host = "admin.evil.example"
	req.Header.Set("X-Forwarded-Host", "admin.evil.example")
	resp, body := e.do(req)
	if strings.Contains(body, "evil") || strings.Contains(resp.Header.Get("Content-Security-Policy"), "evil") {
		t.Errorf("a forged Host reached the sign-in page: %s", body)
	}
}

// With the control callback off, as when the dashboard is, the labels name
// nothing.
func TestControlHostsOff(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	svc := oauth.New(e.store, e.users, e.users, nil, e.rec, oauth.Options{
		Issuer: e.srv.URL, Now: e.clock.Now, InsecureCookies: true, ViewCallback: viewCallback, Passwords: e.users,
	})
	e.srv.Config.Handler = svc.Handler()
	resp, _ := e.get(signInQuery(oauth.HostAdmin, "/"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestControlSignOut(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("boss", "", user.RoleAdmin)
	resp, _ := e.passwordSignIn(oauth.HostAdmin, "boss", password)
	admin, err := e.svc.Redeem(ctx, location(t, resp).Query().Get("code"), oauth.HostAdmin, browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = e.get(signInQuery("demo", "/"))
	page, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SignOut(ctx, oauth.HostAccount, admin.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ControlSession(ctx, oauth.HostAdmin, admin.Token); err != nil {
		t.Fatalf("a sign-out on another host ended the session: %v", err)
	}
	if err := e.svc.SignOut(ctx, oauth.HostAdmin, admin.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ControlSession(ctx, oauth.HostAdmin, admin.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("ControlSession after sign-out: err = %v, want ErrNotFound", err)
	}
	if _, err := e.svc.Viewer(ctx, "demo", page.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("a page opened by the same sign-in outlived it: %v", err)
	}
	resp, _ = e.get(signInQuery(oauth.HostAdmin, "/"))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after sign-out the browser was let in without the form: status %d", resp.StatusCode)
	}
	if err := e.svc.SignOut(ctx, oauth.HostAdmin, "no-such-token"); err != nil {
		t.Errorf("SignOut of an unknown token: %v", err)
	}
}

// An MCP client's authorization signs the person in first, then shows a
// consent page that asks for no credential.
func TestConsentAfterPasswordSignIn(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("ana", "", "")
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	p := newPKCE("verifier")

	resp, body := e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", p).Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) || !strings.Contains(body, "Sign in to connect Test Client") ||
		strings.Contains(body, `name="action"`) {
		t.Fatalf("authorize: status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "/oauth/consent?with=key") {
		t.Error("the sign-in page offers no way to approve with an API key")
	}
	resp, body = e.signInForm(url.Values{"request_id": {"not-the-request"}, "login": {"ana"}, "password": {password}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a form for another request: status %d: %s", resp.StatusCode, body)
	}
	resp, body = e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {"ana"}, "password": {"a wrong password"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, e.requestCookie()) {
		t.Fatalf("wrong password: status %d: %s", resp.StatusCode, body)
	}
	resp, body = e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {"ana"}, "password": {password}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != oauth.PathConsent {
		t.Fatalf("sign-in: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	resp, body = e.get(oauth.PathConsent)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<strong>ana</strong>") || !strings.Contains(body, `value="approve"`) ||
		strings.Contains(body, `name="password"`) {
		t.Fatalf("consent page: status %d: %s", resp.StatusCode, body)
	}

	code := e.approve(nil)
	tr := e.exchange(clientID, redirect, code, p)
	if tr.status != http.StatusOK {
		t.Fatalf("exchange: %d %s", tr.status, tr.ErrorDescription)
	}
	pr, err := e.svc.Authenticate(ctx, tr.AccessToken)
	if err != nil || pr.Handle != "ana" || pr.ExpiresAt != nil || len(pr.Slugs) != 0 || pr.HasScope(auth.ScopeAdmin) {
		t.Fatalf("principal = %+v, %v; want ana, bounded by no key", pr, err)
	}
	grants := e.rec.actions(oauth.ActionGrant)
	if len(grants) != 1 || grants[0].Details["via"] != "password" || grants[0].Details["key_prefix"] != "" {
		t.Errorf("grant audit = %+v", grants)
	}

	// A browser that is signed in goes straight to the consent page.
	resp, _ = e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("second")).Encode())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != oauth.PathConsent {
		t.Fatalf("authorize when signed in: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body = e.get(oauth.PathConsent); !strings.Contains(body, "<strong>ana</strong>") {
		t.Errorf("consent page when signed in: %s", body)
	}
}

// The consent page that takes an API key in place of a sign-in stays.
func TestConsentWithKeyBesidePasswords(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("ana", "", "")
	k, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	p := newPKCE("verifier")
	e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", p).Encode())
	resp, body := e.get(oauth.PathConsent + "?with=key")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="api_key"`) || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("key consent page: status %d: %s", resp.StatusCode, body)
	}
	code := e.approve(url.Values{"api_key": {key}})
	tr := e.exchange(clientID, redirect, code, p)
	pr, err := e.svc.Authenticate(ctx, tr.AccessToken)
	if err != nil || pr.Handle != "ana" || len(pr.Scopes) != 1 {
		t.Fatalf("principal = %+v, %v; want ana with the key's one scope", pr, err)
	}
	if g := e.rec.actions(oauth.ActionGrant); len(g) != 1 || g[0].Details["key_prefix"] != k.Prefix {
		t.Errorf("grant audit = %+v", g)
	}
}

// Without a sign-in the consent page approves nothing.
func TestConsentNeedsSignIn(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("verifier")).Encode())
	resp, body := e.get(oauth.PathConsent)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) || strings.Contains(body, `value="approve"`) {
		t.Fatalf("consent page before sign-in: status %d: %s", resp.StatusCode, body)
	}
	v := url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}}
	req, _ := http.NewRequest(http.MethodPost, e.url(oauth.PathConsent), strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, body = e.do(req)
	if resp.StatusCode == http.StatusFound && strings.Contains(resp.Header.Get("Location"), "code=") {
		t.Fatalf("approved without a sign-in: %s", resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("approve without a sign-in: status %d: %s", resp.StatusCode, body)
	}
}

// Ending a person's sessions leaves the applications they connected.
func TestEndSessions(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("ana", "", "")
	e.person("ben", "", "")

	// ana connects a client, and is signed in in two browsers.
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	p := newPKCE("verifier")
	e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", p).Encode())
	e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {"ana"}, "password": {password}})
	tr := e.exchange(clientID, redirect, e.approve(nil), p)
	resp, _ := e.get(signInQuery(oauth.HostAccount, "/account/"))
	here, err := e.svc.Redeem(ctx, location(t, resp).Query().Get("code"), oauth.HostAccount, browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	laptop := e.jar
	e.jar, _ = cookiejar.New(nil)
	e.client.Jar = e.jar
	resp, _ = e.passwordSignIn("demo", "ana", password)
	there, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = e.passwordSignIn("demo", "ben", password)
	bens, err := e.svc.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	e.jar, e.client.Jar = laptop, laptop
	if n, err := e.svc.CountSessions(ctx, "ana"); err != nil || n != 2 {
		t.Fatalf("CountSessions = %d, %v; want 2", n, err)
	}

	// Changing the password where ana is signed in keeps that browser.
	cs, err := e.svc.ControlSession(ctx, oauth.HostAccount, here.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.users.ChangePassword(ctx, "ana", password, "another long password", cs.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ControlSession(ctx, oauth.HostAccount, here.Token); err != nil {
		t.Errorf("the session the password was changed in ended: %v", err)
	}
	if _, err := e.svc.Viewer(ctx, "demo", there.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("another browser's session outlived the password change: %v", err)
	}
	if _, err := e.svc.Viewer(ctx, "demo", bens.Token); err != nil {
		t.Errorf("another person's session ended: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, tr.AccessToken); err != nil {
		t.Errorf("the connected application was disconnected: %v", err)
	}
	ev := e.rec.actions(audit.SessionRevoke)
	if len(ev) != 1 || ev[0].Details["user"] != "ana" || ev[0].Details["sessions"] != "1" {
		t.Errorf("session audit = %+v", ev)
	}

	// A used reset link ends every session; with revoke-all the
	// applications too.
	admin := auth.ContextWithPrincipal(ctx, auth.Principal{Handle: "admin", Admin: true})
	_, token, err := e.users.IssueLink(admin, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.Redeem(ctx, token, user.RedeemRequest{Password: password}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ControlSession(ctx, oauth.HostAccount, here.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("a session outlived the reset: %v", err)
	}
	if n, _ := e.svc.CountSessions(ctx, "ana"); n != 0 {
		t.Errorf("CountSessions after a reset = %d", n)
	}
	if _, err := e.svc.Authenticate(ctx, tr.AccessToken); err != nil {
		t.Errorf("a plain reset disconnected the application: %v", err)
	}
	if _, token, err = e.users.IssueLink(admin, "ana", user.PurposeReset, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.Redeem(ctx, token, user.RedeemRequest{Password: password}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, tr.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("a reset with revoke-all left the application connected: %v", err)
	}
}

// A session opened with an API key carries the key's prefix to the control
// host, which decides what it may do there.
func TestControlSessionWithKey(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	ctx := context.Background()
	e.person("boss", "", user.RoleAdmin)
	k, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Scopes: auth.MemberScopes})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := e.keySignIn(oauth.HostAdmin, key, nil)
	v, err := e.svc.Redeem(ctx, location(t, resp).Query().Get("code"), oauth.HostAdmin, browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := e.svc.ControlSession(ctx, oauth.HostAdmin, v.Token)
	if err != nil || cs.Handle != "boss" || cs.KeyPrefix != k.Prefix {
		t.Fatalf("ControlSession = %+v, %v", cs, err)
	}
	if err := e.users.RevokeKey(ctx, user.KeyRef{Handle: "boss", Prefix: k.Prefix}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ControlSession(ctx, oauth.HostAdmin, v.Token); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("ControlSession after the key was revoked: err = %v, want ErrNotFound", err)
	}
}

// heldChecker refuses every password, after waiting to be released: it
// stands in for the hash, which takes long enough for a burst of requests
// to arrive while the first is still being checked.
type heldChecker struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
	err     error
}

func (h *heldChecker) CheckPassword(ctx context.Context, _, _ string) (*user.User, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	if h.release != nil {
		<-h.release
	}
	if h.err != nil {
		return nil, h.err
	}
	return nil, user.ErrBadPassword
}

func (h *heldChecker) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// Guesses sent all at once get no further than guesses sent in turn: the
// attempts under way are counted before their passwords are checked.
func TestPasswordDelaysHoldAgainstABurst(t *testing.T) {
	checker := &heldChecker{release: make(chan struct{})}
	e := newEnv(t, envOptions{noIDP: true, passwords: true, checker: checker})
	const burst = 30
	statuses := make(chan int, burst)
	for range burst {
		go func() {
			v := url.Values{"host": {"demo"}, "path": {"/"}, "nonce": {nonceHash(browserNonce)}, "login": {"ana"}, "password": {"a wrong password"}}
			req, _ := http.NewRequest(http.MethodPost, e.url(oauth.PathSignIn), strings.NewReader(v.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", e.srv.URL)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	// Every request but the ones being checked is answered at once.
	refused := 0
	for refused < burst-3 {
		if got := <-statuses; got != http.StatusTooManyRequests {
			t.Fatalf("a request of the burst was answered %d while others were being checked, want 429", got)
		}
		refused++
	}
	if got := checker.count(); got != 3 {
		t.Errorf("%d passwords were checked for one account in a burst of %d, want the 3 free attempts", got, burst)
	}
	close(checker.release)
	for range 3 {
		if got := <-statuses; got != http.StatusUnauthorized {
			t.Errorf("an admitted attempt was answered %d, want 401", got)
		}
	}
}

// A person's handle and their email are one account to the delays.
func TestPasswordDelaysCountHandleAndEmailTogether(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "ana@example.org", "")
	for i, login := range []string{"ana", "ana@example.org", "ANA", "Ana@Example.org"} {
		if resp, _ := e.passwordSignIn("demo", login, "a wrong password"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d as %q: status %d, want 401", i+1, login, resp.StatusCode)
		}
	}
	for _, login := range []string{"ana", "ana@example.org"} {
		if resp, _ := e.passwordSignIn("demo", login, password); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("after four failures under both names, %q: status %d, want 429", login, resp.StatusCode)
		}
	}
}

func TestPasswordSignInWhenBusy(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true, checker: &heldChecker{err: user.ErrBusy}})
	for range 5 {
		resp, body := e.passwordSignIn("demo", "ana", password)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "busy") {
			t.Fatalf("status %d, Retry-After %q: %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
		}
	}
	// A server too busy to check a password has not seen a wrong one.
	if resp, _ := e.passwordSignIn("demo", "ana", password); resp.StatusCode == http.StatusTooManyRequests {
		t.Error("attempts the server could not check were counted as failures")
	}
}

// The recovery link carries the sign-in it leaves, and the recovery form
// comes back to it with the name filled in.
func TestRecoveryCarriesTheSignIn(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	nonce := nonceHash(browserNonce)

	resp, _ := e.get(oauth.PathRecover + "?" + url.Values{"host": {"demo"}, "path": {"/report"}, "nonce": {nonce}}.Encode())
	want := "https://art.test/account/recover?" + url.Values{"host": {"demo"}, "path": {"/report"}, "nonce": {nonce}}.Encode()
	if got := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || got != want {
		t.Errorf("recover for a host: status %d to %q, want %q", resp.StatusCode, got, want)
	}

	for name, q := range map[string]url.Values{
		"bad nonce":      {"host": {"demo"}, "path": {"/"}, "nonce": {"short"}},
		"bad request id": {"request_id": {"x"}},
		"nothing":        {},
	} {
		resp, _ := e.get(oauth.PathRecover + "?" + q.Encode())
		if got := resp.Header.Get("Location"); got != "https://art.test/account/recover" {
			t.Errorf("recover with %s goes to %q, want the bare form", name, got)
		}
	}

	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("verifier")).Encode())
	id := e.requestCookie()
	resp, _ = e.get(oauth.PathRecover + "?" + url.Values{"request_id": {id}}.Encode())
	if got := resp.Header.Get("Location"); got != "https://art.test/account/recover?request_id="+id {
		t.Errorf("recover for an authorization goes to %q", got)
	}

	_, body := e.get(oauth.PathConsent + "?" + url.Values{"login": {"ana"}, "notice": {"recovered"}}.Encode())
	for _, want := range []string{`name="request_id" value="` + id + `"`, `name="login" value="ana"`, "Your new password is set."} {
		if !strings.Contains(body, want) {
			t.Errorf("the sign-in form after recovery lacks %q", want)
		}
	}
	_, body = e.get(signInQuery("demo", "/") + "&login=ana&notice=other")
	if !strings.Contains(body, `name="login" value="ana"`) || strings.Contains(body, "Your new password is set.") {
		t.Errorf("a host's sign-in form fills in the name and no unknown notice:\n%s", body)
	}
}

// An artifact's sign-in page tells a visitor who is no member what to ask
// for, and keeps the API key one step further away.
func TestSignInForAPageLeadsOutsiders(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	_, body := e.get(signInQuery("demo", "/"))
	for _, want := range []string{"Not a member?", "Ask the person who sent you this page for a share link.", "<summary>More ways to sign in</summary>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the sign-in page for a page lacks %q", want)
		}
	}
	if strings.Contains(body, "Kazari instance") {
		t.Errorf("the sign-in page for a page speaks of a Kazari instance")
	}
}
