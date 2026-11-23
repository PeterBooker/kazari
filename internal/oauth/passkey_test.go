package oauth_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/passkey/passkeytest"
	"github.com/PeterBooker/kazari/internal/signin"
)

var (
	passkeyOptions = regexp.MustCompile(`data-passkey-options="([^"]+)"`)
	passkeyToken   = regexp.MustCompile(`name="passkey_token" value="([^"]+)"`)
)

// addPasskey gives handle a passkey held by a, made on the apex.
func (e *env) addPasskey(handle string, a *passkeytest.Authenticator) {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.users.GetUser(ctx, handle)
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := e.keys.Registration(ctx, u)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := a.Create(c.Options, "https://art.test")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.keys.Register(ctx, u, c.Token, resp, "Laptop"); err != nil {
		e.t.Fatal(err)
	}
}

// passkeyForm returns the ceremony a sign-in page carries.
func passkeyForm(t *testing.T, body string) (options, token string) {
	t.Helper()
	o, tok := passkeyOptions.FindStringSubmatch(body), passkeyToken.FindStringSubmatch(body)
	if o == nil || tok == nil {
		t.Fatalf("the page has no passkey form:\n%s", body)
	}
	return html.UnescapeString(o[1]), html.UnescapeString(tok[1])
}

// passkeySignIn answers the passkey form of a sign-in page for host with a,
// as the sign-in host's own page would.
func (e *env) passkeySignIn(a *passkeytest.Authenticator, host, origin string) (*http.Response, string) {
	e.t.Helper()
	_, body := e.get(signInQuery(host, "/"))
	options, token := passkeyForm(e.t, body)
	resp, err := a.Get(options, origin)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.signInForm(url.Values{"host": {host}, "path": {"/"}, "nonce": {nonceHash(browserNonce)},
		"passkey_token": {token}, "passkey": {resp}})
}

func TestSignInWithPasskey(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true, passkeys: true})
	ctx := context.Background()
	e.person("ana", "", "")
	var a passkeytest.Authenticator
	e.addPasskey("ana", &a)

	resp, body := e.get(signInQuery("demo", "/report"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in page: status %d", resp.StatusCode)
	}
	for _, want := range []string{`data-passkey="get"`, `autocomplete="username webauthn"`, "Sign in with a passkey", `name="password"`} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in page lacks %q", want)
		}
	}
	e.checkPage("sign-in with passkeys", resp, body, "https://demo.art.test")

	resp, body = e.passkeySignIn(&a, "demo", e.srv.URL)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("passkey sign-in: status %d: %s", resp.StatusCode, body)
	}
	v, err := e.signin.Redeem(ctx, hostCode(t, resp, "demo"), "demo", browserNonce)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.signin.Viewer(ctx, "demo", v.Token); err != nil || got != "ana" {
		t.Fatalf("Viewer = %q, %v; want ana", got, err)
	}
	ev := e.rec.actions(signin.ActionSignIn)
	if len(ev) != 1 || ev[0].Actor != "ana" || ev[0].Details["via"] != "passkey" {
		t.Fatalf("sign-in audit = %+v", ev)
	}
}

func TestPasskeySignInRefusals(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true, passkeys: true})
	e.person("ana", "", "")
	var a passkeytest.Authenticator
	e.addPasskey("ana", &a)

	// An answer made on an artifact host, whose script can ask the browser
	// for the instance's passkeys.
	resp, body := e.passkeySignIn(&a, "demo", "https://demo.art.test")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "That passkey was not accepted") {
		t.Errorf("answer from an artifact host: status %d: %s", resp.StatusCode, body)
	}
	if len(e.rec.actions(signin.ActionSignIn)) != 0 {
		t.Error("a refused passkey signed someone in")
	}

	// The same answer twice.
	_, body = e.get(signInQuery("demo", "/"))
	options, token := passkeyForm(t, body)
	answer, err := a.Get(options, e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"host": {"demo"}, "path": {"/"}, "nonce": {nonceHash(browserNonce)}, "passkey_token": {token}, "passkey": {answer}}
	if resp, body := e.signInForm(form); resp.StatusCode != http.StatusFound {
		t.Fatalf("first use: status %d: %s", resp.StatusCode, body)
	}
	resp, body = e.signInForm(form)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "This page was open too long") {
		t.Errorf("answer sent twice: status %d: %s", resp.StatusCode, body)
	}
	// The refusal shows the form again, with a new ceremony.
	if _, again := passkeyForm(t, body); again == token {
		t.Error("the refusal carries the used ceremony again")
	}
}

func TestPasskeySignInSlowsRefusals(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true, passkeys: true})
	e.person("ana", "", "")
	var a passkeytest.Authenticator
	e.addPasskey("ana", &a)
	var last *http.Response
	for range 30 {
		last, _ = e.passkeySignIn(&a, "demo", "https://evil.example")
		if last.StatusCode == http.StatusTooManyRequests {
			break
		}
	}
	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" {
		t.Errorf("after many refused passkeys: status %d", last.StatusCode)
	}
}

func TestNoPasskeyFormWithoutPasswords(t *testing.T) {
	e := newEnv(t, envOptions{passkeys: true})
	_, body := e.get(signInQuery("demo", "/"))
	if strings.Contains(body, "data-passkey") {
		t.Error("an instance with an identity provider offers a passkey")
	}
	resp, _ := e.signInForm(url.Values{"host": {"demo"}, "path": {"/"}, "nonce": {nonceHash(browserNonce)}, "passkey_token": {"x"}, "passkey": {"{}"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("passkey form posted to an instance with an identity provider: status %d", resp.StatusCode)
	}
}
