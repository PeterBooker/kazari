package account_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/user"
)

var recoveryCode = regexp.MustCompile(`[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}`)

// makeCodes makes recovery codes on the account page of a signed-in
// browser and returns them.
func (b *browser) makeCodes() []string {
	b.e.t.Helper()
	tok := b.token()
	resp, body := b.post("/account/recovery-codes", url.Values{"form_token": {tok}, "current": {password}})
	if resp.StatusCode != http.StatusOK {
		b.e.t.Fatalf("make codes: status %d: %s", resp.StatusCode, body)
	}
	return recoveryCode.FindAllString(body, -1)
}

func TestRecoveryCodesOnAccountPage(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", user.RoleMember)
	b := e.browser()
	b.signIn("ana", password)
	if body := b.page(); !strings.Contains(body, "You hold no recovery codes.") || !strings.Contains(body, `action="/account/recovery-codes"`) {
		t.Fatalf("the account page lacks the recovery section: %s", body)
	}

	tok := b.token()
	resp, body := b.post("/account/recovery-codes", url.Values{"form_token": {tok}, "current": {"not the password"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "No codes were made") || recoveryCode.MatchString(body) {
		t.Fatalf("wrong password: status %d: %s", resp.StatusCode, body)
	}

	tok = b.token()
	form := url.Values{"form_token": {tok}, "current": {password}}
	resp, body = b.post("/account/recovery-codes", form)
	codes := recoveryCode.FindAllString(body, -1)
	if resp.StatusCode != http.StatusOK || len(codes) != user.RecoveryCodeCount || !strings.Contains(body, "Your <b>recovery codes</b>") {
		t.Fatalf("make codes: status %d, %d codes: %s", resp.StatusCode, len(codes), body)
	}
	// A reload sends the form again, and makes nothing new.
	resp, body = b.post("/account/recovery-codes", form)
	if resp.StatusCode != http.StatusConflict || recoveryCode.MatchString(body) {
		t.Fatalf("form sent again: status %d: %s", resp.StatusCode, body)
	}
	if body := b.page(); !strings.Contains(body, "You hold 10 unused recovery codes.") || !strings.Contains(body, "New recovery codes made") {
		t.Errorf("the account page after making codes: %s", body)
	}
	if _, err := e.users.Recover(context.Background(), "ana", codes[3], "recovered long password"); err != nil {
		t.Errorf("a code from the page: %v", err)
	}
}

func TestRecoverPage(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", user.RoleMember)
	signedIn := e.browser()
	signedIn.signIn("ana", password)
	codes := signedIn.makeCodes()

	b := e.browser()
	resp, body := b.get("/account/recover")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="/account/recover"`) || !strings.Contains(body, `name="code"`) {
		t.Fatalf("the recover page: status %d: %s", resp.StatusCode, body)
	}
	const next = "recovered long password"
	form := func(login, code, pw, confirm string) url.Values {
		return url.Values{"login": {login}, "code": {code}, "password": {pw}, "confirm": {confirm}}
	}
	for _, c := range []struct {
		name   string
		form   url.Values
		status int
		says   string
	}{
		{"wrong code", form("ana", "aaaa-aaaa-aaaa-aaaa", next, next), http.StatusUnprocessableEntity, "was not accepted"},
		{"unknown name", form("nobody", codes[0], next, next), http.StatusUnprocessableEntity, "was not accepted"},
		{"passwords differ", form("ana", codes[0], next, next+"!"), http.StatusUnprocessableEntity, "are not the same"},
		{"weak password", form("ana", codes[0], "short", "short"), http.StatusUnprocessableEntity, "Use between 15"},
		{"no code", form("ana", "", next, next), http.StatusUnprocessableEntity, "a recovery code"},
	} {
		resp, body := b.post("/account/recover", c.form)
		if resp.StatusCode != c.status || !strings.Contains(body, c.says) {
			t.Errorf("%s: status %d, want %d with %q: %s", c.name, resp.StatusCode, c.status, c.says, body)
		}
	}

	resp, body = b.post("/account/recover", form("ana", strings.ToUpper(codes[0]), next, next))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("recover: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if signedIn.signedIn() {
		t.Error("the browser signed in before the recovery is still signed in")
	}
	b.signIn("ana", next)
	if body := b.page(); !strings.Contains(body, "You hold 9 unused recovery codes.") || !strings.Contains(body, "New password set with a recovery code") {
		t.Errorf("the account page after the recovery: %s", body)
	}

	resp, _ = e.browser().post("/account/recover", form("ana", codes[0], password, password))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a used code: status %d", resp.StatusCode)
	}
}

func TestRecoverPageSlowsWrongCodes(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", user.RoleMember)
	b := e.browser()
	wrong := url.Values{"login": {"ana"}, "code": {"aaaa-aaaa-aaaa-aaaa"}, "password": {"a brand new password"}, "confirm": {"a brand new password"}}
	var last *http.Response
	for range 8 {
		last, _ = b.post("/account/recover", wrong)
	}
	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" {
		t.Errorf("after eight wrong codes: status %d, Retry-After %q", last.StatusCode, last.Header.Get("Retry-After"))
	}
}

func TestRecoverPageOff(t *testing.T) {
	e := newEnv(t, envOptions{noLocal: true})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, e.apex.URL+"/account/recover", nil)
		req.Header.Set("Origin", e.apex.URL)
		if resp, _ := e.browser().do(req); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s with built-in accounts off: status %d", method, resp.StatusCode)
		}
	}
}
