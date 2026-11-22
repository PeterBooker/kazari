package account_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/passkey/passkeytest"
	"github.com/PeterBooker/kazari/internal/user"
)

var (
	passkeyOptions = regexp.MustCompile(`data-passkey="(create|get)" data-passkey-options="([^"]+)"`)
	passkeyToken   = regexp.MustCompile(`name="passkey_token" value="([^"]+)"`)
	removeAction   = regexp.MustCompile(`action="(/account/passkeys/[0-9]+/remove)"`)
)

// ceremony returns the options and token of the passkey form of kind on a
// page.
func ceremony(t *testing.T, body, kind string) (options, token string) {
	t.Helper()
	o, tok := passkeyOptions.FindStringSubmatch(body), passkeyToken.FindStringSubmatch(body)
	if o == nil || tok == nil || o[1] != kind {
		t.Fatalf("the page has no %s passkey form:\n%s", kind, body)
	}
	return html.UnescapeString(o[2]), html.UnescapeString(tok[1])
}

// addPasskey adds a passkey held by a on the account page of a signed-in
// browser, and returns the answer to the form.
func (b *browser) addPasskey(a *passkeytest.Authenticator, current, name string) (*http.Response, string) {
	b.e.t.Helper()
	body := b.page()
	options, token := ceremony(b.e.t, body, "create")
	answer, err := a.Create(options, b.e.apex.URL)
	if err != nil {
		b.e.t.Fatal(err)
	}
	return b.post("/account/passkeys", url.Values{
		"form_token": {formToken.FindStringSubmatch(body)[1]}, "passkey_token": {token}, "passkey": {answer},
		"current": {current}, "name": {name},
	})
}

// signInWithPasskey signs the browser in on the apex with a passkey held by
// a, through the sign-in host's page.
func (b *browser) signInWithPasskey(a *passkeytest.Authenticator) {
	b.e.t.Helper()
	t := b.e.t
	resp, _ := b.get("/account/")
	resp, body := b.getURL(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in page: status %d", resp.StatusCode)
	}
	options, token := ceremony(t, body, "get")
	answer, err := a.Get(options, b.e.mcp.URL)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(resp.Request.URL.String())
	form := url.Values{"host": {oauth.HostAccount}, "path": {"/account/"}, "nonce": {q.Query().Get("nonce")},
		"passkey_token": {token}, "passkey": {answer}}
	resp, body = b.postFrom(b.e.mcp.URL, b.e.mcp.URL+oauth.PathSignIn, form)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), b.e.apex.URL+account.CallbackPath+"?code=") {
		t.Fatalf("passkey sign-in: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if resp, _ = b.getURL(resp.Header.Get("Location")); resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: status %d", resp.StatusCode)
	}
}

func TestAddPasskeyAndSignIn(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", user.RoleMember)
	b := e.browser()
	b.signIn("ana", password)
	body := b.page()
	for _, want := range []string{"You have added no passkey.", `data-passkey="create"`, `action="/account/passkeys"`, "data-passkey-nojs"} {
		if !strings.Contains(body, want) {
			t.Errorf("the account page lacks %q", want)
		}
	}

	var a passkeytest.Authenticator
	resp, body := b.addPasskey(&a, "not the password", "Laptop")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "No passkey was added.") {
		t.Fatalf("wrong password: status %d: %s", resp.StatusCode, body)
	}
	if keys, _ := e.users.Passkeys(context.Background(), "ana"); len(keys) != 0 {
		t.Fatalf("a wrong password added %d passkeys", len(keys))
	}

	a = passkeytest.Authenticator{}
	resp, body = b.addPasskey(&a, password, "Laptop")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add passkey: status %d: %s", resp.StatusCode, body)
	}
	body = b.page()
	for _, want := range []string{"The passkey is added.", "<td data-label=\"Passkey\">Laptop</td>", "Passkey Laptop added"} {
		if !strings.Contains(body, want) {
			t.Errorf("the account page after adding lacks %q", want)
		}
	}

	// Made on the apex, it signs in on the sign-in host.
	other := e.browser()
	other.signInWithPasskey(&a)
	if body := other.page(); !strings.Contains(body, "Signed in with a passkey") {
		t.Errorf("the account page does not show the passkey sign-in: %s", body)
	}

	// Removed, it signs in no more.
	m := removeAction.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no remove form")
	}
	resp, _ = b.post(m[1], url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: status %d", resp.StatusCode)
	}
	if body := b.page(); !strings.Contains(body, "You have added no passkey.") || !strings.Contains(body, "Passkey Laptop removed") {
		t.Errorf("the account page after removing: %s", body)
	}
}

func TestAddPasskeyRefusals(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", user.RoleMember)
	e.person("ben", user.RoleMember)
	b := e.browser()
	b.signIn("ana", password)

	// Made for another origin.
	body := b.page()
	options, token := ceremony(t, body, "create")
	var a passkeytest.Authenticator
	answer, _ := a.Create(options, "https://q3-report.art.test")
	resp, body := b.post("/account/passkeys", url.Values{"form_token": {b.token()}, "passkey_token": {token}, "passkey": {answer}, "current": {password}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "was not accepted") {
		t.Errorf("passkey made elsewhere: status %d: %s", resp.StatusCode, body)
	}

	// Ana's ceremony, sent from ben's session.
	body = b.page()
	options, token = ceremony(t, body, "create")
	answer, _ = (&passkeytest.Authenticator{}).Create(options, e.apex.URL)
	ben := e.browser()
	ben.signIn("ben", password)
	resp, body = ben.post("/account/passkeys", url.Values{"form_token": {ben.token()}, "passkey_token": {token}, "passkey": {answer}, "current": {password}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "This page was open too long") {
		t.Errorf("another person's ceremony: status %d: %s", resp.StatusCode, body)
	}

	// Another person's passkey is not found.
	var held passkeytest.Authenticator
	if resp, _ := b.addPasskey(&held, password, ""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: status %d", resp.StatusCode)
	}
	m := removeAction.FindStringSubmatch(b.page())
	if resp, _ := ben.post(m[1], url.Values{"form_token": {ben.token()}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("removing another person's passkey: status %d", resp.StatusCode)
	}
}

func TestNoPasskeysWithoutBuiltInAccounts(t *testing.T) {
	e := newEnv(t, envOptions{noLocal: true})
	u, err := e.users.CreateUser(asAdmin, user.CreateUserRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := e.users.IssueKey(asAdmin, user.IssueKeyRequest{Handle: u.Handle})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("ana", secret)
	if body := b.page(); strings.Contains(body, "Passkeys") || strings.Contains(body, "data-passkey") {
		t.Error("the account page offers passkeys without built-in accounts")
	}
	if resp, _ := b.post("/account/passkeys", url.Values{"form_token": {b.token()}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /account/passkeys: status %d", resp.StatusCode)
	}
}
