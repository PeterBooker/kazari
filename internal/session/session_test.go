package session_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/session"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// provider is the sign-in host: one code, bound to a nonce hash, opens one
// session.
type provider struct {
	code, nonceHash string
	live            map[string]oauth.ControlSession // token -> session
	signedOut       []string
	linked          string // the handle of the last LinkCode
	err             error
}

func (p *provider) Redeem(_ context.Context, code, host, nonce string) (oauth.View, error) {
	if p.err != nil {
		return oauth.View{}, p.err
	}
	sum := sha256.Sum256([]byte(nonce))
	if code != p.code || host != oauth.HostAdmin || base64.RawURLEncoding.EncodeToString(sum[:]) != p.nonceHash {
		return oauth.View{}, oauth.ErrNotFound
	}
	p.code = ""
	p.live["view-token"] = oauth.ControlSession{Handle: "boss", SessionID: 7, ExpiresAt: t0.Add(12 * time.Hour)}
	return oauth.View{Token: "view-token", ExpiresAt: t0.Add(12 * time.Hour), Path: "/people?role=admin"}, nil
}

func (p *provider) ControlSession(_ context.Context, host, token string) (oauth.ControlSession, error) {
	if p.err != nil {
		return oauth.ControlSession{}, p.err
	}
	cs, ok := p.live[token]
	if !ok || host != oauth.HostAdmin {
		return oauth.ControlSession{}, oauth.ErrNotFound
	}
	return cs, nil
}

func (p *provider) SignOut(_ context.Context, _, token string) error {
	p.signedOut = append(p.signedOut, token)
	delete(p.live, token)
	return nil
}

func (p *provider) LinkCode(_ context.Context, handle string) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	p.linked = handle
	return "the-link-code", nil
}

func manager(p *provider, insecure bool) *session.Manager {
	return session.New(p, session.Options{
		Host: oauth.HostAdmin, Cookie: "kazari_admin", SignInURL: "https://mcp.art.test/oauth/signin",
		InsecureCookies: insecure, Now: func() time.Time { return t0 },
	})
}

// browser carries cookies from responses to the next request.
type browser struct{ cookies map[string]string }

func (b *browser) request(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	for name, value := range b.cookies {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	return r
}

func (b *browser) take(t *testing.T, w *httptest.ResponseRecorder) []*http.Cookie {
	t.Helper()
	if b.cookies == nil {
		b.cookies = map[string]string{}
	}
	set := w.Result().Cookies()
	for _, c := range set {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
			continue
		}
		b.cookies[c.Name] = c.Value
	}
	return set
}

func TestSignInAndRedeem(t *testing.T) {
	p := &provider{code: "the-code", live: map[string]oauth.ControlSession{}}
	m := manager(p, false)
	b := &browser{}

	if s, err := m.Get(b.request("GET", "https://admin.art.test/people")); s != nil || err != nil {
		t.Fatalf("Get without a cookie = %+v, %v", s, err)
	}
	w := httptest.NewRecorder()
	m.SignIn(w, b.request("GET", "https://admin.art.test/people?role=admin"), "/people?role=admin")
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != http.StatusFound || loc.Scheme+"://"+loc.Host+loc.Path != "https://mcp.art.test/oauth/signin" {
		t.Fatalf("SignIn: status %d to %q", w.Code, w.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("host") != oauth.HostAdmin || q.Get("path") != "/people?role=admin" || len(q.Get("nonce")) != 43 {
		t.Fatalf("sign-in query = %v", q)
	}
	for _, c := range b.take(t, w) {
		if c.Name != "__Host-kazari_admin_nonce" || !c.HttpOnly || !c.Secure || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("nonce cookie = %+v", c)
		}
	}
	p.nonceHash = q.Get("nonce")

	// A second tab reuses the nonce, so both sign-ins can come back.
	w = httptest.NewRecorder()
	m.SignIn(w, b.request("GET", "https://admin.art.test/"), "/")
	if again, _ := url.Parse(w.Header().Get("Location")); again.Query().Get("nonce") != p.nonceHash {
		t.Error("a second tab got another nonce")
	}

	// The code opens nothing in a browser without the nonce cookie.
	w = httptest.NewRecorder()
	if _, _, err := m.Open(w, (&browser{}).request("GET", "https://admin.art.test/session/callback?code=the-code")); !errors.Is(err, session.ErrSignIn) {
		t.Fatalf("Open in another browser: err = %v, want ErrSignIn", err)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a refused sign-in set a cookie")
	}

	w = httptest.NewRecorder()
	_, path, err := m.Open(w, b.request("GET", "https://admin.art.test/session/callback?code=the-code"))
	if err != nil || path != "/people?role=admin" {
		t.Fatalf("Open = %q, %v", path, err)
	}
	for _, c := range b.take(t, w) {
		if c.Name != "__Host-kazari_admin" || c.Value != "view-token" || !c.HttpOnly || !c.Secure || c.Path != "/" || c.Domain != "" ||
			c.MaxAge != 12*60*60 || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("session cookie = %+v", c)
		}
	}
	s, err := m.Get(b.request("GET", "https://admin.art.test/people"))
	if err != nil || s == nil || s.Handle != "boss" || s.SessionID != 7 {
		t.Fatalf("Get = %+v, %v", s, err)
	}
	if _, _, err := m.Open(httptest.NewRecorder(), b.request("GET", "https://admin.art.test/session/callback?code=the-code")); !errors.Is(err, session.ErrSignIn) {
		t.Errorf("a code used twice: err = %v, want ErrSignIn", err)
	}
}

func TestExpiredSession(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{"view-token": {Handle: "boss"}}}
	m := manager(p, false)
	b := &browser{cookies: map[string]string{"__Host-kazari_admin": "view-token"}}
	if s, _ := m.Get(b.request("GET", "https://admin.art.test/")); s == nil {
		t.Fatal("Get with a live session = nil")
	}
	delete(p.live, "view-token")
	if s, err := m.Get(b.request("GET", "https://admin.art.test/")); s != nil || err != nil {
		t.Errorf("Get once the session ended = %+v, %v; want none", s, err)
	}
	// An ended session is still a cookie the browser presents, which tells
	// it from a browser that never signed in.
	if !m.Presented(b.request("GET", "https://admin.art.test/")) || m.Presented((&browser{}).request("GET", "https://admin.art.test/")) {
		t.Error("Presented does not tell an ended session from none")
	}
	p.err = errors.New("database is locked")
	if s, err := m.Get(b.request("GET", "https://admin.art.test/")); s != nil || err == nil {
		t.Errorf("Get on a store failure = %+v, %v; want the error, never a session", s, err)
	}
}

// The unprefixed cookie of plain http is not the session cookie of https.
func TestCookieNames(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{"view-token": {Handle: "boss"}}}
	planted := &browser{cookies: map[string]string{"kazari_admin": "view-token"}}
	if s, _ := manager(p, false).Get(planted.request("GET", "https://admin.art.test/")); s != nil {
		t.Error("a cookie without the __Host- prefix opened a session")
	}
	if s, _ := manager(p, true).Get(planted.request("GET", "http://localhost:9443/")); s == nil {
		t.Error("the unprefixed cookie did not open a session over plain http on this machine")
	}
	w := httptest.NewRecorder()
	manager(p, true).SignIn(w, httptest.NewRequest("GET", "http://localhost:9443/", nil), "/")
	if c := w.Result().Cookies()[0]; c.Name != "kazari_admin_nonce" || c.Secure {
		t.Errorf("insecure nonce cookie = %+v", c)
	}
}

func TestSignInReturnPath(t *testing.T) {
	m := manager(&provider{}, false)
	for _, bad := range []string{"https://evil.example/", "//evil.example", `/\evil.example`, "people", ""} {
		w := httptest.NewRecorder()
		m.SignIn(w, httptest.NewRequest("GET", "https://admin.art.test/", nil), bad)
		loc, _ := url.Parse(w.Header().Get("Location"))
		if got := loc.Query().Get("path"); got != "/" {
			t.Errorf("return path %q was sent on as %q, want /", bad, got)
		}
	}
}

func TestEnd(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{"view-token": {Handle: "boss"}}}
	m := manager(p, false)
	b := &browser{cookies: map[string]string{"__Host-kazari_admin": "view-token"}}
	w := httptest.NewRecorder()
	if err := m.End(w, b.request("POST", "https://admin.art.test/session/signout")); err != nil {
		t.Fatal(err)
	}
	b.take(t, w)
	if len(p.signedOut) != 1 || p.signedOut[0] != "view-token" || len(b.cookies) != 0 {
		t.Errorf("signed out %v, cookies left %v", p.signedOut, b.cookies)
	}
	if err := m.End(httptest.NewRecorder(), b.request("POST", "https://admin.art.test/session/signout")); err != nil || len(p.signedOut) != 1 {
		t.Errorf("End without a session: %v, signed out %v", err, p.signedOut)
	}
}

func TestFormToken(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{"one": {Handle: "boss"}, "two": {Handle: "boss"}}}
	m := manager(p, false)
	get := func(token string) *session.Session {
		s, err := m.Get((&browser{cookies: map[string]string{"__Host-kazari_admin": token}}).request("GET", "https://admin.art.test/"))
		if err != nil || s == nil {
			t.Fatalf("Get(%q) = %+v, %v", token, s, err)
		}
		return s
	}
	one, two := get("one"), get("two")
	tok := one.FormToken()
	if tok == "" || tok == "one" || strings.Contains(tok, "one") || tok != get("one").FormToken() {
		t.Fatalf("FormToken = %q, want a stable value that is not the cookie", tok)
	}
	if !one.ValidForm(tok) {
		t.Error("a session refused its own form token")
	}
	for name, bad := range map[string]string{"empty": "", "another session's": two.FormToken(), "the cookie": "one", "truncated": tok[:len(tok)-1]} {
		if one.ValidForm(bad) {
			t.Errorf("%s token was accepted", name)
		}
	}
}

func TestFlash(t *testing.T) {
	m := manager(&provider{}, false)
	b := &browser{}
	w := httptest.NewRecorder()
	m.SetFlash(w, "person-disabled")
	for _, c := range b.take(t, w) {
		if c.Name != "__Host-kazari_admin_flash" || !c.HttpOnly || !c.Secure || c.MaxAge != 60 {
			t.Errorf("flash cookie = %+v", c)
		}
	}
	w = httptest.NewRecorder()
	if got := m.Flash(w, b.request("GET", "https://admin.art.test/people")); got != "person-disabled" {
		t.Errorf("Flash = %q", got)
	}
	b.take(t, w)
	if got := m.Flash(httptest.NewRecorder(), b.request("GET", "https://admin.art.test/people")); got != "" {
		t.Errorf("Flash shown twice: %q", got)
	}

	// Only a short word travels: never a secret, never a sentence.
	for _, bad := range []string{"", "kzk_abcdefgh_" + strings.Repeat("a", 52), "<script>", "Has Spaces", "https://evil.example"} {
		w = httptest.NewRecorder()
		m.SetFlash(w, bad)
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("SetFlash(%q) set a cookie", bad)
		}
		planted := &browser{cookies: map[string]string{"__Host-kazari_admin_flash": bad}}
		if got := m.Flash(httptest.NewRecorder(), planted.request("GET", "https://admin.art.test/")); got != "" {
			t.Errorf("a planted flash %q was shown as %q", bad, got)
		}
	}
}

func TestNavigation(t *testing.T) {
	tests := []struct {
		name, method string
		headers      map[string]string
		want         bool
	}{
		{"a tab", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, true},
		{"a frame", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, false},
		{"a fetch", "GET", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, false},
		{"an old browser", "GET", map[string]string{"Accept": "text/html,application/xhtml+xml"}, true},
		{"curl", "GET", map[string]string{"Accept": "*/*"}, false},
		{"a form post", "POST", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, "https://admin.art.test/", nil)
		for k, v := range tt.headers {
			r.Header.Set(k, v)
		}
		if got := session.Navigation(r); got != tt.want {
			t.Errorf("%s: Navigation = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestOnce(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{"one": {Handle: "boss"}, "two": {Handle: "boss"}}}
	now := t0
	m := session.New(p, session.Options{Host: oauth.HostAdmin, Cookie: "kazari_admin", Now: func() time.Time { return now }})
	get := func(m *session.Manager, token string) *session.Session {
		s, err := m.Get((&browser{cookies: map[string]string{"__Host-kazari_admin": token}}).request("GET", "https://admin.art.test/"))
		if err != nil || s == nil {
			t.Fatalf("Get(%q) = %+v, %v", token, s, err)
		}
		return s
	}
	one := get(m, "one")
	v := one.Once()
	if v == one.Once() {
		t.Fatal("two forms got the same value")
	}
	if err := one.Claim(v); err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	if err := get(m, "one").Claim(v); !errors.Is(err, session.ErrFormUsed) {
		t.Errorf("the second claim: %v, want ErrFormUsed", err)
	}

	restarted := session.New(p, session.Options{Host: oauth.HostAdmin, Cookie: "kazari_admin", Now: func() time.Time { return now }})
	for name, tt := range map[string]struct {
		s *session.Session
		v string
	}{
		"empty":               {one, ""},
		"not base64":          {one, "not base64!"},
		"truncated":           {one, one.Once()[:20]},
		"another session's":   {get(m, "two"), one.Once()},
		"made before restart": {get(restarted, "one"), one.Once()},
		"the form token":      {one, one.FormToken()},
	} {
		if err := tt.s.Claim(tt.v); !errors.Is(err, session.ErrFormExpired) {
			t.Errorf("%s: %v, want ErrFormExpired", name, err)
		}
	}

	old := one.Once()
	now = now.Add(13 * time.Hour)
	if err := one.Claim(old); !errors.Is(err, session.ErrFormExpired) {
		t.Errorf("a value older than a form may stay open: %v, want ErrFormExpired", err)
	}
}

func TestRequire(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{"view-token": {Handle: "boss"}}}
	m := manager(p, false)
	signedIn := &browser{cookies: map[string]string{"__Host-kazari_admin": "view-token"}}

	w := httptest.NewRecorder()
	page := (&browser{}).request("GET", "https://admin.art.test/people")
	page.Header.Set("Sec-Fetch-Mode", "navigate")
	page.Header.Set("Sec-Fetch-Dest", "document")
	if _, err := m.Require(w, page); !errors.Is(err, session.ErrSignInStarted) || w.Code != http.StatusFound {
		t.Errorf("a page load without a session: %v, status %d", err, w.Code)
	}
	w = httptest.NewRecorder()
	if _, err := m.Require(w, (&browser{}).request("POST", "https://admin.art.test/people")); !errors.Is(err, session.ErrSignedOut) || len(w.Header()) != 0 {
		t.Errorf("a form without a session: %v, headers %v", err, w.Header())
	}
	s, err := m.Require(httptest.NewRecorder(), signedIn.request("GET", "https://admin.art.test/"))
	if err != nil || s == nil || s.Handle != "boss" {
		t.Fatalf("Require = %+v, %v", s, err)
	}

	post := func(body string) *http.Request {
		r := httptest.NewRequest("POST", "https://admin.art.test/people", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "__Host-kazari_admin", Value: "view-token"})
		return r
	}
	if err := s.CheckForm(httptest.NewRecorder(), post(url.Values{session.FormField: {s.FormToken()}}.Encode())); err != nil {
		t.Errorf("a form with its token: %v", err)
	}
	if err := s.CheckForm(httptest.NewRecorder(), post("form_token=nope")); !errors.Is(err, session.ErrFormToken) {
		t.Errorf("a form with another token: %v, want ErrFormToken", err)
	}
	if err := s.CheckForm(httptest.NewRecorder(), post("form_token="+strings.Repeat("a", 70<<10))); !errors.Is(err, session.ErrBadForm) {
		t.Errorf("a form too large to read: %v, want ErrBadForm", err)
	}
}

func TestCallbackAndSignOut(t *testing.T) {
	p := &provider{code: "the-code", live: map[string]oauth.ControlSession{}}
	m := manager(p, false)
	b := &browser{}
	w := httptest.NewRecorder()
	m.SignIn(w, b.request("GET", "https://admin.art.test/"), "/people?role=admin")
	b.take(t, w)
	loc, _ := url.Parse(w.Header().Get("Location"))
	p.nonceHash = loc.Query().Get("nonce")

	w = httptest.NewRecorder()
	s, err := m.Callback(w, b.request("GET", "https://admin.art.test/session/callback?code=the-code"))
	if err != nil || s == nil || s.Handle != "boss" || w.Code != http.StatusFound || w.Header().Get("Location") != "/people?role=admin" {
		t.Fatalf("Callback = %+v, %v; status %d to %q", s, err, w.Code, w.Header().Get("Location"))
	}
	b.take(t, w)
	w = httptest.NewRecorder()
	if _, err := m.Callback(w, b.request("GET", "https://admin.art.test/session/callback?code=the-code")); !errors.Is(err, session.ErrSignIn) || w.Header().Get("Location") != "" {
		t.Errorf("a used code: %v, Location %q", err, w.Header().Get("Location"))
	}

	signOut := func(token string) *http.Request {
		r := b.request("POST", "https://admin.art.test/session/signout")
		r.Body = io.NopCloser(strings.NewReader(url.Values{session.FormField: {token}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	if err := m.SignOut(httptest.NewRecorder(), signOut("nope")); !errors.Is(err, session.ErrFormToken) || len(p.signedOut) != 0 {
		t.Errorf("a sign-out without the form token: %v, signed out %v", err, p.signedOut)
	}
	w = httptest.NewRecorder()
	if err := m.SignOut(w, signOut(s.FormToken())); err != nil || len(p.signedOut) != 1 {
		t.Fatalf("SignOut: %v, signed out %v", err, p.signedOut)
	}
	b.take(t, w)
	// A browser whose session ended is signed out without a token.
	if err := m.SignOut(httptest.NewRecorder(), signOut("")); err != nil {
		t.Errorf("signing out an ended session: %v", err)
	}
}

// Handoff sends the browser through the sign-in host with a code that signs
// it in there, so the sign-in it opens is the browser's one, and the code
// comes back as any sign-in's does.
func TestHandoff(t *testing.T) {
	p := &provider{live: map[string]oauth.ControlSession{}}
	m := manager(p, false)
	b := &browser{}
	w := httptest.NewRecorder()
	if err := m.Handoff(t.Context(), w, b.request("POST", "https://admin.art.test/invite"), "ana", "/welcome"); err != nil {
		t.Fatalf("Handoff = %v", err)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != http.StatusSeeOther || loc.Scheme+"://"+loc.Host+loc.Path != "https://mcp.art.test/oauth/signin" {
		t.Fatalf("Handoff: status %d to %q", w.Code, w.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get(oauth.ParamHandoff) != "the-link-code" || q.Get("login") != "ana" || q.Get("host") != oauth.HostAdmin ||
		q.Get("path") != "/welcome" || len(q.Get("nonce")) != 43 || p.linked != "ana" {
		t.Errorf("handoff query = %v, linked %q", q, p.linked)
	}
	set := b.take(t, w)
	if len(set) != 1 || set[0].Name != "__Host-kazari_admin_nonce" {
		t.Errorf("Handoff set the cookies %+v, want the nonce alone", set)
	}
	p.err = errors.New("store down")
	if err := m.Handoff(t.Context(), httptest.NewRecorder(), b.request("POST", "https://admin.art.test/invite"), "ana", "/"); err == nil {
		t.Error("Handoff with a failing provider = nil")
	}
}
