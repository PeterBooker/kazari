package account_test

import (
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
	if err := e.oauth.EndSessions(t.Context(), "ana", 0); err != nil {
		t.Fatal(err)
	}
	b.get("/account/")
	if got := result(auth.ResultInvalid); got != 1 {
		t.Errorf("invalid = %v after the session ended, want 1", got)
	}
}
