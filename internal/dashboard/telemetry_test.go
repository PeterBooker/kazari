package dashboard_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
	"github.com/PeterBooker/kazari/internal/user"
)

// A session cookie is a credential like the others: each request to the
// dashboard is counted in kazari.auth.requests by how it was received.
func TestSessionAuthenticationsAreCounted(t *testing.T) {
	rec := instrumenttest.New(t)
	e := newEnv(t, func(_ *dashboard.Deps, o *dashboard.Options) { o.Telemetry = rec.Providers })
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	session := instrument.CredentialType.String(auth.SourceSession)
	result := func(r string) float64 {
		n, _ := rec.Sum("kazari.auth.requests", session, instrument.AuthResult.String(r))
		return n
	}

	b := e.signedIn("boss")
	if got := result(auth.ResultMissing); got != 1 {
		t.Errorf("missing = %v after the visit that led to the sign-in, want 1", got)
	}
	before := result(auth.ResultOK)
	b.get("/people")
	if got := result(auth.ResultOK) - before; got != 1 {
		t.Errorf("ok grew by %v for one page, want 1", got)
	}

	member := e.browser()
	r := member.navigate("/")
	member.signInAt(r.Header.Get("Location"), url.Values{"login": {"ana"}, "password": {password}})
	if r = member.navigate("/"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("a member on the dashboard: status %d", r.StatusCode)
	}
	if got := result(auth.ResultForbidden); got != 1 {
		t.Errorf("forbidden = %v after a member's visit, want 1", got)
	}

	if err := e.oauth.EndSessions(context.Background(), "boss", 0); err != nil {
		t.Fatal(err)
	}
	b.navigate("/people")
	if got := result(auth.ResultInvalid); got != 1 {
		t.Errorf("invalid = %v after the session ended, want 1", got)
	}
}
