package account_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
)

// A session cookie is a credential like the others: each request with one,
// or without, is counted in kazari.auth.requests.
func TestSessionAuthenticationsAreCounted(t *testing.T) {
	rec := instrumenttest.New(t)
	e := newEnv(t, envOptions{telemetry: rec.Providers})
	e.person("ana", "")
	session := instrument.CredentialType.String(auth.SourceSession)
	result := func(r string) float64 {
		n, _ := rec.Sum("kazari.auth.requests", session, instrument.AuthResult.String(r))
		return n
	}

	b := e.browser()
	b.get("/account/")
	if got := result(auth.ResultMissing); got != 1 {
		t.Errorf("missing = %v after a visit without a session, want 1", got)
	}
	b.signIn("ana", password)
	before := result(auth.ResultOK)
	b.page()
	if got := result(auth.ResultOK) - before; got != 1 {
		t.Errorf("ok grew by %v for one page of a signed-in person, want 1", got)
	}
	// The sign-in ends everywhere; the cookie the browser still sends is
	// then a session that is no more.
	if err := e.signin.EndSessions(t.Context(), "ana", 0); err != nil {
		t.Fatal(err)
	}
	b.get("/account/")
	if got := result(auth.ResultInvalid); got != 1 {
		t.Errorf("invalid = %v after the session ended, want 1", got)
	}
}

// A form of a live session without the session's token is a request that
// was refused, and is counted as one.
func TestRefusedFormIsCounted(t *testing.T) {
	rec := instrumenttest.New(t)
	e := newEnv(t, envOptions{telemetry: rec.Providers})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	forbidden := func() float64 {
		n, _ := rec.Sum("kazari.auth.requests", instrument.CredentialType.String(auth.SourceSession), instrument.AuthResult.String(auth.ResultForbidden))
		return n
	}
	for i, path := range []string{"/account/sessions/end", "/account/signout"} {
		if resp, _ := b.post(path, url.Values{"form_token": {"not-the-token"}}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s without the token: status %d", path, resp.StatusCode)
		}
		if got := forbidden(); got != float64(i+1) {
			t.Errorf("forbidden = %v after %d refused forms", got, i+1)
		}
	}
	if !b.signedIn() {
		t.Error("a refused sign-out signed the browser out")
	}
}
