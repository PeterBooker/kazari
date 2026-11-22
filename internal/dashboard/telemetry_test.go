package dashboard_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
	"github.com/PeterBooker/kazari/internal/middleware"
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

// A form of a live session without the session's token is a request that
// was refused, and is counted as one.
func TestRefusedFormIsCounted(t *testing.T) {
	rec := instrumenttest.New(t)
	e := newEnv(t, func(_ *dashboard.Deps, o *dashboard.Options) { o.Telemetry = rec.Providers })
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	forbidden := func() float64 {
		n, _ := rec.Sum("kazari.auth.requests", instrument.CredentialType.String(auth.SourceSession), instrument.AuthResult.String(auth.ResultForbidden))
		return n
	}
	b := e.signedIn("boss")
	b.get("/people/ana")
	for i, path := range []string{"/people/ana/sessions/end", "/session/signout"} {
		if r := b.post(path, url.Values{"form_token": {"not-the-token"}}); r.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s without the token: status %d", path, r.StatusCode)
		}
		if got := forbidden(); got != float64(i+1) {
			t.Errorf("forbidden = %v after %d refused forms", got, i+1)
		}
	}
}

// observed serves one request to a dashboard behind the request observer,
// as the browser b, and returns the status with the recorder that saw it.
func observed(t *testing.T, e *env, b *browser, method, path string, form url.Values) (int, *instrumenttest.Recorder) {
	t.Helper()
	rec := instrumenttest.New(t)
	h := middleware.NewObserver(rec.Providers, true).Observe("admin", middleware.ObserveOptions{ControlPlane: true})(dashboard.New(e.deps, e.opts))
	var body io.Reader
	if form != nil {
		form.Set("form_token", b.token)
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, e.opts.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.opts.URL)
	}
	if b != nil {
		u, _ := url.Parse(e.opts.URL)
		for _, c := range b.client.Jar.Cookies(u) {
			req.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, rec
}

// What the dashboard answers is told to the observer as the API's answers
// are: the route that matched, and the code of an error.
func TestRoutesAndErrorCodesAreReported(t *testing.T) {
	bk := &fakeBackup{}
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Backup = bk })
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	r := b.get("/system")
	asset := regexp.MustCompile(`href="(/assets/theme\.[0-9a-f]+\.css)"`).FindStringSubmatch(r.body)
	if asset == nil {
		t.Fatal("no stylesheet on the page")
	}

	// The assets are served outside the session check, on a route of their
	// own rather than the catch-all.
	status, rec := observed(t, e, nil, http.MethodGet, asset[1], nil)
	if _, ok := rec.Span("GET /assets/"); status != http.StatusOK || !ok {
		t.Errorf("an asset: status %d, span on the asset route %v", status, ok)
	}

	tests := []struct {
		name         string
		b            *browser
		method, path string
		form         url.Values
		backup       error
		status       int
		span, code   string
	}{
		{name: "signed out", method: http.MethodGet, path: "/people", status: http.StatusUnauthorized, span: "GET /people", code: api.CodeUnauthorized},
		{name: "nothing there", b: b, method: http.MethodGet, path: "/no/such/page", status: http.StatusNotFound, span: "GET /", code: api.CodeNotFound},
		{name: "a backup already running", b: b, method: http.MethodPost, path: "/system/backup", form: url.Values{}, backup: backup.ErrRunning,
			status: http.StatusConflict, span: "POST /system/backup", code: api.CodeBackupRunning},
		{name: "no room for a backup", b: b, method: http.MethodPost, path: "/system/backup", form: url.Values{}, backup: backup.ErrNoSpace,
			status: http.StatusInsufficientStorage, span: "POST /system/backup", code: api.CodeNoSpace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bk.err = tt.backup
			status, rec := observed(t, e, tt.b, tt.method, tt.path, tt.form)
			if status != tt.status {
				t.Fatalf("status %d, want %d", status, tt.status)
			}
			if _, ok := rec.Span(tt.span); !ok {
				t.Fatalf("no span named %q", tt.span)
			}
			if got := rec.SpanAttr(tt.span, instrument.ErrorCode); got != tt.code {
				t.Errorf("error code = %q, want %q", got, tt.code)
			}
		})
	}
}
