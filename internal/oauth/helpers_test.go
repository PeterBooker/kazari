package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/user"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recorder) Record(_ context.Context, e audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) actions(action string) []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []audit.Event
	for _, e := range r.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// fakeIDP's Exchange checks the PKCE pair and nonce it was handed in AuthURL,
// for any number of sign-ins in flight.
type fakeIDP struct {
	mu       sync.Mutex
	identity auth.Identity
	flows    map[string]string // nonce -> PKCE challenge
}

func (f *fakeIDP) AuthURL(state, nonce, challenge string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flows == nil {
		f.flows = map[string]string{}
	}
	f.flows[nonce] = challenge
	return "https://idp.example/authorize?" + url.Values{
		"state": {state}, "nonce": {nonce}, "code_challenge": {challenge},
	}.Encode()
}

func (f *fakeIDP) Exchange(_ context.Context, code, nonce, verifier string) (auth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code != "upstream-code" {
		return auth.Identity{}, errors.New("bad upstream code")
	}
	challenge, ok := f.flows[nonce]
	if !ok {
		return auth.Identity{}, errors.New("nonce mismatch")
	}
	h := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(h[:]) != challenge {
		return auth.Identity{}, errors.New("pkce mismatch")
	}
	return f.identity, nil
}

func (f *fakeIDP) setIdentity(id auth.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity = id
}

type env struct {
	t      *testing.T
	srv    *httptest.Server
	svc    *oauth.Service
	store  *sqlite.OAuthStore
	users  *user.Service
	clock  *clock
	idp    *fakeIDP // nil without an identity provider
	rec    *recorder
	jar    http.CookieJar
	client *http.Client
}

type envOptions struct {
	noIDP      bool
	httpClient *http.Client
	logger     *slog.Logger
}

func newEnv(t *testing.T, eo envOptions) *env {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	e := &env{t: t, clock: &clock{now: t0}, rec: &recorder{}, store: sqlite.NewOAuthStore(db)}
	e.users = user.New(sqlite.NewUserStore(db), e.rec, user.Options{
		Now:            e.clock.Now,
		AllowedDomains: []string{"example.com"},
		AutoProvision:  true,
		AdminEmails:    []string{"boss@example.com"},
		// As internal/app wires it.
		OnDisable: func(ctx context.Context, handle string) error { return e.svc.RevokeAllForUser(ctx, handle) },
	})
	var idp oauth.IdentityProvider
	if !eo.noIDP {
		e.idp = &fakeIDP{identity: auth.Identity{Issuer: "https://idp.example", Subject: "alice-sub",
			Email: "alice@example.com", EmailVerified: true, Name: "Alice"}}
		idp = e.idp
	}

	e.srv = httptest.NewUnstartedServer(nil)
	base := "http://" + e.srv.Listener.Addr().String()
	e.svc = oauth.New(e.store, e.users, e.users, idp, e.rec, oauth.Options{
		Issuer:          base,
		Resource:        base + "/",
		Now:             e.clock.Now,
		HTTPClient:      eo.httpClient,
		Logger:          eo.logger,
		InsecureCookies: true,
		ViewCallback:    viewCallback,
	})
	e.srv.Config.Handler = e.svc.Handler()
	e.srv.Start()
	t.Cleanup(e.srv.Close)

	e.jar, _ = cookiejar.New(nil)
	e.client = &http.Client{
		Jar:           e.jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return e
}

// viewCallback knows the artifact hosts demo, demo--v2 and other under
// art.test.
func viewCallback(host string) (string, bool) {
	switch host {
	case "demo", "demo--v2", "other":
		return "https://" + host + ".art.test/.kazari/callback", true
	}
	return "", false
}

func (e *env) url(path string) string { return e.srv.URL + path }

func (e *env) do(req *http.Request) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp, string(body)
}

func (e *env) get(path string) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.url(path), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(req)
}

func (e *env) postForm(path string, v url.Values) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url(path), strings.NewReader(v.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.do(req)
}

func (e *env) register(redirectURIs ...string) string {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": "Test Client", "redirect_uris": redirectURIs})
	req, _ := http.NewRequest(http.MethodPost, e.url("/oauth/register"), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, raw := e.do(req)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("register: status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		e.t.Fatal(err)
	}
	return out.ClientID
}

type pkce struct{ verifier, challenge string }

func newPKCE(seed string) pkce {
	v := strings.Repeat(seed, 43/len(seed)+1)[:43]
	h := sha256.Sum256([]byte(v))
	return pkce{v, base64.RawURLEncoding.EncodeToString(h[:])}
}

func authorizeParams(clientID, redirectURI, scope string, p pkce) url.Values {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {p.challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"client-state"},
	}
	if scope != "" {
		v.Set("scope", scope)
	}
	return v
}

func location(t *testing.T, resp *http.Response) *url.URL {
	t.Helper()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *env) requestCookie() string {
	u, _ := url.Parse(e.url("/oauth/consent"))
	for _, c := range e.jar.Cookies(u) {
		if c.Name == oauth.InsecureCookieName {
			return c.Value
		}
	}
	return ""
}

func (e *env) signIn(params url.Values) string {
	e.t.Helper()
	resp, body := e.get("/oauth/authorize?" + params.Encode())
	if e.idp == nil {
		if resp.StatusCode != http.StatusOK {
			e.t.Fatalf("authorize: status %d: %s", resp.StatusCode, body)
		}
		return body
	}
	loc := location(e.t, resp)
	if loc.Host != "idp.example" {
		e.t.Fatalf("authorize redirected to %s, want the identity provider", loc)
	}
	resp, body = e.get("/oauth/callback?" + url.Values{"code": {"upstream-code"}, "state": {loc.Query().Get("state")}}.Encode())
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	return body
}

func (e *env) approve(extra url.Values) string {
	e.t.Helper()
	return e.approveWith(nil, extra)
}

func (e *env) approveWith(header http.Header, extra url.Values) string {
	e.t.Helper()
	v := url.Values{"request_id": {e.requestCookie()}, "action": {"approve"}}
	for k, vs := range extra {
		v[k] = vs
	}
	req, err := http.NewRequest(http.MethodPost, e.url("/oauth/consent"), strings.NewReader(v.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, body := e.do(req)
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("consent: status %d: %s", resp.StatusCode, body)
	}
	loc := location(e.t, resp)
	if errCode := loc.Query().Get("error"); errCode != "" {
		e.t.Fatalf("consent redirected with error %s: %s", errCode, loc.Query().Get("error_description"))
	}
	return loc.Query().Get("code")
}

type tokenResult struct {
	status int
	header http.Header

	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (e *env) token(v url.Values) tokenResult {
	e.t.Helper()
	resp, body := e.postForm("/oauth/token", v)
	var r tokenResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		e.t.Fatalf("token: %v: %s", err, body)
	}
	r.status, r.header = resp.StatusCode, resp.Header
	return r
}

func (e *env) exchange(clientID, redirectURI, code string, p pkce) tokenResult {
	e.t.Helper()
	return e.token(url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"code": {code}, "code_verifier": {p.verifier},
	})
}

func (e *env) refresh(clientID, refreshToken, scope string) tokenResult {
	e.t.Helper()
	v := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}}
	if scope != "" {
		v.Set("scope", scope)
	}
	return e.token(v)
}

func (e *env) flow(scope string) (string, tokenResult) {
	e.t.Helper()
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	p := newPKCE("verifier")
	e.signIn(authorizeParams(clientID, redirect, scope, p))
	code := e.approve(nil)
	tr := e.exchange(clientID, redirect, code, p)
	if tr.status != http.StatusOK {
		e.t.Fatalf("exchange: %d %s: %s", tr.status, tr.Error, tr.ErrorDescription)
	}
	return clientID, tr
}
