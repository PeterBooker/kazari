package gate_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/oauth"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

const (
	signInURL = "https://mcp.art.test/oauth/signin"
	viewToken = "view-token-of-alice-on-team"
)

// linkToken is a well-formed share link token.
var linkToken = artifact.LinkTokenPrefix + "aaaaaaaa_" + strings.Repeat("b", 52)

// fakeSessions knows one view session: viewToken on host "team".
type fakeSessions struct {
	mu       sync.Mutex
	redeemed []string // code host nonce
	signOuts []string // host token
	err      error    // returned by every call when set
}

func (f *fakeSessions) Redeem(_ context.Context, code, host, nonce string) (oauth.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeemed = append(f.redeemed, code+" "+host+" "+nonce)
	if f.err != nil {
		return oauth.View{}, f.err
	}
	switch code {
	case "good":
		return oauth.View{Token: viewToken, ExpiresAt: now.Add(12 * time.Hour), Path: "/report?q=1"}, nil
	case "sneaky":
		return oauth.View{Token: viewToken, ExpiresAt: now.Add(time.Hour), Path: "//evil.example/"}, nil
	case "dotted":
		return oauth.View{Token: viewToken, ExpiresAt: now.Add(time.Hour), Path: `/x/../\evil.example/`}, nil
	case "dots":
		return oauth.View{Token: viewToken, ExpiresAt: now.Add(time.Hour), Path: "/a/../b?c=1"}, nil
	}
	return oauth.View{}, oauth.ErrNotFound
}

func (f *fakeSessions) Viewer(_ context.Context, host, token string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if host == "team" && token == viewToken {
		return "alice", nil
	}
	return "", oauth.ErrNotFound
}

func (f *fakeSessions) SignOut(_ context.Context, host, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signOuts = append(f.signOuts, host+" "+token)
	return f.err
}

// fakeLinks knows one link, linkToken, for slug "team".
type fakeLinks struct {
	mu     sync.Mutex
	opened int
	gone   bool // the link was revoked
}

func (f *fakeLinks) CheckShareLink(_ context.Context, slug, token string) (*artifact.ShareLink, error) {
	if slug != "team" || token != linkToken || f.gone {
		return nil, artifact.ErrLinkNotFound
	}
	return &artifact.ShareLink{Prefix: "aaaaaaaa", ExpiresAt: now.Add(48 * time.Hour)}, nil
}

func (f *fakeLinks) OpenShareLink(ctx context.Context, slug, token string) (*artifact.ShareLink, error) {
	l, err := f.CheckShareLink(ctx, slug, token)
	if err == nil {
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
	}
	return l, err
}

type fixture struct {
	g        *gate.Gate
	sessions *fakeSessions
	links    *fakeLinks
}

func newFixture() *fixture {
	fx := &fixture{sessions: &fakeSessions{}, links: &fakeLinks{}}
	fx.g = gate.New(fx.sessions, fx.links, gate.Options{SignInURL: signInURL, Now: func() time.Time { return now }})
	return fx
}

var (
	team   = &artifact.Published{Slug: "team", VersionID: 1, VersionNumber: 1, Visibility: artifact.Members}
	public = &artifact.Published{Slug: "demo", VersionID: 2, VersionNumber: 1, Visibility: artifact.Public}
)

// navigate is the Fetch Metadata of a top-level page load.
var navigate = http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}

func request(method, target string, header http.Header, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "team.art.test"
	for k, v := range header {
		r.Header[k] = v
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func (fx *fixture) allow(r *http.Request, p *artifact.Published, host string) (bool, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return fx.g.Allow(rec, r, p, host), rec
}

func cookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestAllowPublic(t *testing.T) {
	fx := newFixture()
	ok, rec := fx.allow(request(http.MethodGet, "/", navigate), public, "demo")
	if !ok || rec.Code != http.StatusOK || rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Fatalf("public artifact: ok %v, status %d, headers %v", ok, rec.Code, rec.Header())
	}
}

func TestAllowSendsNavigationsToSignIn(t *testing.T) {
	fx := newFixture()
	ok, rec := fx.allow(request(http.MethodGet, "/report?q=1", navigate), team, "team--v2")
	if ok || rec.Code != http.StatusFound {
		t.Fatalf("ok %v, status %d", ok, rec.Code)
	}
	nonce := cookie(rec, "__Host-kazari_nonce")
	if nonce == nil || !nonce.HttpOnly || !nonce.Secure || nonce.Path != "/" || nonce.Domain != "" ||
		nonce.SameSite != http.SameSiteLaxMode || nonce.MaxAge != 600 || len(nonce.Value) != 43 {
		t.Fatalf("nonce cookie = %+v", nonce)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if loc.Scheme+"://"+loc.Host+loc.Path != signInURL || q.Get("host") != "team--v2" || q.Get("path") != "/report?q=1" ||
		q.Get("nonce") != s256(nonce.Value) {
		t.Fatalf("Location = %s (nonce %s)", loc, nonce.Value)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}

	// A second tab reuses the browser's nonce, so both sign-ins complete.
	_, rec2 := fx.allow(request(http.MethodGet, "/", navigate, nonce), team, "team")
	if c := cookie(rec2, "__Host-kazari_nonce"); c == nil || c.Value != nonce.Value {
		t.Fatalf("second redirect set nonce %+v, want %q again", c, nonce.Value)
	}
}

func TestAllowAnswersOtherRequests401(t *testing.T) {
	fx := newFixture()
	for name, tc := range map[string]struct {
		method string
		header http.Header
		status int
	}{
		"script":                   {http.MethodGet, http.Header{"Sec-Fetch-Mode": {"no-cors"}, "Sec-Fetch-Dest": {"script"}}, http.StatusUnauthorized},
		"fetch":                    {http.MethodGet, http.Header{"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}}, http.StatusUnauthorized},
		"frame":                    {http.MethodGet, http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}, http.StatusUnauthorized},
		"curl":                     {http.MethodGet, http.Header{"Accept": {"*/*"}}, http.StatusUnauthorized},
		"head":                     {http.MethodHead, navigate, http.StatusUnauthorized},
		"browser without metadata": {http.MethodGet, http.Header{"Accept": {"text/html,application/xhtml+xml"}}, http.StatusFound},
	} {
		ok, rec := fx.allow(request(tc.method, "/app.js", tc.header), team, "team")
		if ok || rec.Code != tc.status {
			t.Errorf("%s: ok %v, status %d, want %d", name, ok, rec.Code, tc.status)
		}
		if tc.status == http.StatusUnauthorized {
			if cookie(rec, "__Host-kazari_nonce") != nil {
				t.Errorf("%s: a 401 set a nonce cookie", name)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s: Cache-Control = %q", name, got)
			}
		}
	}
}

func TestAllowWithCookies(t *testing.T) {
	fx := newFixture()
	view := &http.Cookie{Name: "__Host-kazari_view", Value: viewToken}
	if ok, rec := fx.allow(request(http.MethodGet, "/", nil, view), team, "team"); !ok || rec.Code != http.StatusOK {
		t.Fatalf("view session: ok %v, status %d", ok, rec.Code)
	}
	// The view session is for "team" only, not its version hosts.
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, view), team, "team--v1"); ok {
		t.Fatal("team's view session opened team--v1")
	}
	// A plain name, as a sibling artifact could set with Domain=<base>,
	// is ignored.
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, &http.Cookie{Name: "kazari_view", Value: viewToken}), team, "team"); ok {
		t.Fatal("a cookie without the __Host- prefix was accepted")
	}

	link := &http.Cookie{Name: "__Host-kazari_view", Value: linkToken}
	for _, host := range []string{"team", "team--v1"} {
		if ok, _ := fx.allow(request(http.MethodGet, "/", nil, link), team, host); !ok {
			t.Fatalf("share link refused on %s", host)
		}
	}
	other := &artifact.Published{Slug: "other", VersionID: 9, VersionNumber: 1, Visibility: artifact.Members}
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, link), other, "other"); ok {
		t.Fatal("team's share link opened another artifact")
	}
	fx.links.gone = true
	if ok, rec := fx.allow(request(http.MethodGet, "/", navigate, link), team, "team"); ok || rec.Code != http.StatusFound {
		t.Fatalf("revoked link: ok %v, status %d", ok, rec.Code)
	}

	fx.sessions.err = errors.New("database down")
	if ok, rec := fx.allow(request(http.MethodGet, "/", nil, view), team, "team"); ok || rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure: ok %v, status %d", ok, rec.Code)
	}
}

func (fx *fixture) reserved(r *http.Request, p *artifact.Published, host string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fx.g.ServeReserved(rec, r, p, host)
	return rec
}

func TestCallback(t *testing.T) {
	fx := newFixture()
	nonce := &http.Cookie{Name: "__Host-kazari_nonce", Value: strings.Repeat("n", 43)}
	rec := fx.reserved(request(http.MethodGet, gate.CallbackPath+"?code=good", navigate, nonce), team, "team")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/report?q=1" {
		t.Fatalf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := fx.sessions.redeemed; len(got) != 1 || got[0] != "good team "+nonce.Value {
		t.Fatalf("Redeem calls = %q", got)
	}
	view := cookie(rec, "__Host-kazari_view")
	if view == nil || view.Value != viewToken || !view.HttpOnly || !view.Secure || view.Path != "/" ||
		view.SameSite != http.SameSiteLaxMode || view.MaxAge != int((12*time.Hour)/time.Second) {
		t.Fatalf("view cookie = %+v", view)
	}
	// The nonce stays: another tab's sign-in may come back with it.
	if c := cookie(rec, "__Host-kazari_nonce"); c != nil {
		t.Fatalf("the callback touched the nonce cookie: %+v", c)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	// The return path never leaves the host, and is sent as it is, not
	// cleaned into a form a browser reads as another host.
	for code, want := range map[string]string{"sneaky": "/", "dotted": "/", "dots": "/a/../b?c=1"} {
		rec = fx.reserved(request(http.MethodGet, gate.CallbackPath+"?code="+code, navigate, nonce), team, "team")
		if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != want {
			t.Fatalf("%s: status %d, Location %q, want %q", code, rec.Code, got, want)
		}
	}

	rec = fx.reserved(request(http.MethodGet, gate.CallbackPath+"?code=bad", navigate), team, "team")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Sign-in failed") || cookie(rec, "__Host-kazari_view") != nil {
		t.Fatalf("bad code: status %d, body %q", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("page CSP = %q", csp)
	}
}

func TestShare(t *testing.T) {
	fx := newFixture()
	rec := fx.reserved(request(http.MethodGet, gate.SharePath+"?token="+linkToken, navigate), team, "team--v1")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	view := cookie(rec, "__Host-kazari_view")
	if view == nil || view.Value != linkToken || !view.HttpOnly || view.MaxAge != int((48*time.Hour)/time.Second) {
		t.Fatalf("view cookie = %+v", view)
	}
	if fx.links.opened != 1 {
		t.Fatalf("opened %d times", fx.links.opened)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}

	// A HEAD, as link scanners send, neither counts nor sets anything.
	rec = fx.reserved(request(http.MethodHead, gate.SharePath+"?token="+linkToken, nil), team, "team")
	if rec.Code != http.StatusMethodNotAllowed || fx.links.opened != 1 || cookie(rec, "__Host-kazari_view") != nil {
		t.Fatalf("HEAD: status %d, opened %d", rec.Code, fx.links.opened)
	}

	rec = fx.reserved(request(http.MethodGet, gate.SharePath+"?token="+linkToken, navigate), public, "demo")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Link not valid") {
		t.Fatalf("another artifact's link: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestSignOut(t *testing.T) {
	fx := newFixture()
	if rec := fx.reserved(request(http.MethodGet, gate.SignOutPath, navigate), team, "team"); !strings.Contains(rec.Body.String(), "Not signed in here") ||
		strings.Contains(rec.Body.String(), "<form") {
		t.Fatalf("GET without a cookie: %q", rec.Body.String())
	}
	view := &http.Cookie{Name: "__Host-kazari_view", Value: viewToken}
	rec := fx.reserved(request(http.MethodGet, gate.SignOutPath, navigate, view), team, "team")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="/.kazari/signout"`) || len(fx.sessions.signOuts) != 0 {
		t.Fatalf("GET: status %d, sign-outs %v, body %q", rec.Code, fx.sessions.signOuts, rec.Body.String())
	}

	rec = fx.reserved(request(http.MethodPost, gate.SignOutPath, http.Header{"Origin": {"https://evil.art.test"}}, view), team, "team")
	if rec.Code != http.StatusForbidden || len(fx.sessions.signOuts) != 0 {
		t.Fatalf("foreign POST: status %d, sign-outs %v", rec.Code, fx.sessions.signOuts)
	}

	rec = fx.reserved(request(http.MethodPost, gate.SignOutPath, http.Header{"Origin": {"https://team.art.test"}}, view), team, "team")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Signed out") {
		t.Fatalf("POST: status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := fx.sessions.signOuts; len(got) != 1 || got[0] != "team "+viewToken {
		t.Fatalf("SignOut calls = %q", got)
	}
	if c := cookie(rec, "__Host-kazari_view"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("view cookie not cleared: %+v", c)
	}

	// A share link's cookie is dropped without touching any session.
	link := &http.Cookie{Name: "__Host-kazari_view", Value: linkToken}
	rec = fx.reserved(request(http.MethodPost, gate.SignOutPath, http.Header{"Sec-Fetch-Site": {"same-origin"}}, link), team, "team")
	if rec.Code != http.StatusOK || len(fx.sessions.signOuts) != 1 {
		t.Fatalf("share link sign-out: status %d, sign-outs %v", rec.Code, fx.sessions.signOuts)
	}
}

func TestReservedUnknownPath(t *testing.T) {
	fx := newFixture()
	if rec := fx.reserved(request(http.MethodGet, "/.kazari/whatever", navigate), team, "team"); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if rec := fx.reserved(request(http.MethodPost, gate.CallbackPath, nil), team, "team"); rec.Code != http.StatusMethodNotAllowed ||
		rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST callback: status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// Over plain http (development) the cookies drop Secure and the prefix.
func TestInsecureCookies(t *testing.T) {
	fx := newFixture()
	fx.g = gate.New(fx.sessions, fx.links, gate.Options{SignInURL: signInURL, InsecureCookies: true, Now: func() time.Time { return now }})
	_, rec := fx.allow(request(http.MethodGet, "/", navigate), team, "team")
	c := cookie(rec, "kazari_nonce")
	if c == nil || c.Secure {
		t.Fatalf("nonce cookie = %+v", c)
	}
	if ok, _ := fx.allow(request(http.MethodGet, "/", nil, &http.Cookie{Name: "kazari_view", Value: viewToken}), team, "team"); !ok {
		t.Fatal("plain view cookie refused in insecure mode")
	}
}
