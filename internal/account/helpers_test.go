package account_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/user"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

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

const (
	password = "correct horse battery"
	adminURL = "https://admin.art.test"
	// sibling is an artifact host of the same site, where member-authored
	// script runs.
	sibling = "https://demo.art.test"
)

type envOptions struct {
	noLocal   bool          // people sign in with API keys only
	noAdmin   bool          // the dashboard is off
	maxKeys   int           // user.Options.MaxKeys
	maxKeyTTL time.Duration // auth.tokens.max_key_ttl, for the service and the page
	telemetry instrument.Providers
	// grants, when set, stands between the pages and the sign-in host's
	// service.
	grants func(account.Grants) account.Grants
	// shared is the shared write policy: any member may change any page.
	shared bool
}

// env is an instance without an identity provider: the apex with the
// account pages and the sign-in host, each on its own origin.
type env struct {
	t     *testing.T
	clock *clock
	users *user.Service
	oauth *oauth.Service
	audit *sqlite.AuditStore
	arts  *artifact.Service
	apex  *httptest.Server
	mcp   *httptest.Server
}

func newEnv(t *testing.T, eo envOptions) *env {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	e := &env{t: t, clock: &clock{now: t0}, audit: sqlite.NewAuditStore(db)}
	e.apex = httptest.NewUnstartedServer(nil)
	e.mcp = httptest.NewUnstartedServer(nil)
	apexURL := "http://" + e.apex.Listener.Addr().String()
	mcpURL := "http://" + e.mcp.Listener.Addr().String()

	e.users = user.New(sqlite.NewUserStore(db), e.audit, user.Options{
		Now: e.clock.Now,
		// As internal/app wires it.
		OnDisable: func(ctx context.Context, handle string) error { return e.oauth.RevokeAllForUser(ctx, handle) },
		EndSessions: func(ctx context.Context, handle string, keep int64) error {
			return e.oauth.EndSessions(ctx, handle, keep)
		},
		RevokeGrants:  func(ctx context.Context, handle string) error { return e.oauth.RevokeAllForUser(ctx, handle) },
		LocalAccounts: !eo.noLocal,
		MaxKeys:       eo.maxKeys,
		MaxKeyTTL:     eo.maxKeyTTL,
		// The real parameters cost 64 MiB a hash.
		Argon2: user.Argon2{Memory: 64, Time: 1, Threads: 1},
	})
	blobs, err := blob.NewFS(filepath.Join(t.TempDir(), "blobs"), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy := artifact.WriteOwner
	if eo.shared {
		policy = artifact.WriteShared
	}
	e.arts = artifact.New(sqlite.NewArtifactStore(db), blobs, e.audit, artifact.Options{
		Quotas: e.users, Now: e.clock.Now, WritePolicy: policy, DefaultGrace: 14 * 24 * time.Hour,
		Retention: 30 * 24 * time.Hour, MaxShareLinkTTL: 30 * 24 * time.Hour, DefaultVisibility: artifact.Public,
	})
	var (
		passwords oauth.Passwords
		keys      *passkey.Service
	)
	if !eo.noLocal {
		passwords = e.users
		var err error
		keys, err = passkey.New(e.users, passkey.Options{RPID: "art.test", Origins: []string{apexURL, mcpURL}, Now: e.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
	}
	e.oauth = oauth.New(sqlite.NewOAuthStore(db), e.users, e.users, nil, e.audit, oauth.Options{
		Issuer:          mcpURL,
		Resource:        mcpURL + "/",
		Now:             e.clock.Now,
		InsecureCookies: true,
		Passwords:       passwords,
		Passkeys:        oauthPasskeys(keys),
		ControlCallback: func(host string) (string, bool) {
			return apexURL + account.CallbackPath, host == oauth.HostAccount
		},
	})
	opts := account.Options{
		URL:             apexURL,
		SignInURL:       mcpURL + oauth.PathSignIn,
		AdminURL:        adminURL,
		MCPURL:          mcpURL + "/",
		APIURL:          "https://api.art.test/v1",
		LocalAccounts:   !eo.noLocal,
		MaxKeyTTL:       eo.maxKeyTTL,
		MaxShareLinkTTL: 30 * 24 * time.Hour,
		InsecureCookies: true,
		Now:             e.clock.Now,
		Telemetry:       eo.telemetry,
	}
	if eo.noAdmin {
		opts.AdminURL = ""
	}
	var grants account.Grants = e.oauth
	if eo.grants != nil {
		grants = eo.grants(grants)
	}
	deps := account.Deps{
		Sessions: e.oauth, Users: e.users, Grants: grants, Artifacts: e.arts, Audit: e.audit,
		Links: present.Links{URL: func(sub string) string {
			if sub == "" {
				return "https://art.test/"
			}
			return "https://" + sub + ".art.test/"
		}},
	}
	if keys != nil {
		deps.Passkeys = keys
	}
	e.apex.Config.Handler = account.New(deps, opts)
	e.mcp.Config.Handler = e.oauth.Handler()
	e.apex.Start()
	e.mcp.Start()
	t.Cleanup(e.apex.Close)
	t.Cleanup(e.mcp.Close)
	return e
}

// oauthPasskeys keeps a nil *passkey.Service out of the interface.
func oauthPasskeys(k *passkey.Service) oauth.Passkeys {
	if k == nil {
		return nil
	}
	return k
}

var asAdmin = auth.ContextWithPrincipal(context.Background(), auth.Principal{Handle: "admin", Admin: true})

// invite returns the token of a new person's invite link.
func (e *env) invite(handle string, role user.Role) string {
	e.t.Helper()
	_, _, token, err := e.users.Invite(asAdmin, user.InviteRequest{Handle: handle, Role: role})
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

// page publishes a page of one file for owner.
func (e *env) page(owner, slug, name string, v artifact.Visibility) {
	e.t.Helper()
	_, _, err := e.arts.Create(context.Background(), artifact.Actor{Handle: owner}, artifact.CreateRequest{
		Slug: slug, Name: name, Visibility: v,
		Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("<p>" + slug + "</p>")}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// person invites handle and sets their password.
func (e *env) person(handle string, role user.Role) {
	e.t.Helper()
	if _, err := e.users.Redeem(context.Background(), e.invite(handle, role), user.RedeemRequest{Password: password}); err != nil {
		e.t.Fatal(err)
	}
}

// browser is one person's browser: its own cookies, and no redirect
// followed unseen.
type browser struct {
	e    *env
	c    *http.Client
	once string // the single-use value of the last page that carried one
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, c: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.e.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.e.t.Fatal(err)
	}
	if strings.HasPrefix(resp.Request.URL.String(), b.e.apex.URL) {
		checkPage(b.e, resp, string(body))
	}
	if m := onceValue.FindStringSubmatch(string(body)); m != nil {
		b.once = m[1]
	}
	return resp, string(body)
}

// get loads a page of the apex into a tab.
func (b *browser) get(path string) (*http.Response, string) {
	b.e.t.Helper()
	return b.getURL(b.e.apex.URL + path)
}

func (b *browser) getURL(target string) (*http.Response, string) {
	b.e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		b.e.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	return b.do(req)
}

// post sends a form of the apex from one of its own pages. A link's form
// is sent as the page that link opened wrote it, with the value that ties
// it to the link, unless the test names its own; a form that makes a key,
// with the single-use value of the last page loaded.
func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.e.t.Helper()
	if _, named := form["once"]; !named && b.once != "" {
		form.Set("once", b.once)
	}
	if _, named := form["form_token"]; !named && linkPath.MatchString(path) {
		if token := b.linkCookie(); token != "" {
			form.Set("form_token", account.LinkFormToken(token))
		}
	}
	return b.postFrom(b.e.apex.URL, b.e.apex.URL+path, form)
}

// linkCookie returns the token of the link the browser has open, or "".
func (b *browser) linkCookie() string {
	apex, _ := url.Parse(b.e.apex.URL)
	for _, c := range b.c.Jar.Cookies(apex) {
		if c.Name == "kazari_link" {
			return c.Value
		}
	}
	return ""
}

func (b *browser) postFrom(origin, target string, form url.Values) (*http.Response, string) {
	b.e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		b.e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	return b.do(req)
}

// signIn signs the browser in on the apex through the sign-in host, with a
// password or, when secret is an API key, with that.
func (b *browser) signIn(login, secret string) {
	b.e.t.Helper()
	t := b.e.t
	resp, _ := b.get("/account/")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), b.e.mcp.URL+oauth.PathSignIn+"?") {
		t.Fatalf("account page without a session: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	signIn, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := signIn.Query()
	if q.Get("host") != oauth.HostAccount || q.Get("path") != "/account/" {
		t.Fatalf("sign-in query = %v", q)
	}
	form := url.Values{"host": {q.Get("host")}, "path": {q.Get("path")}, "nonce": {q.Get("nonce")}}
	if strings.HasPrefix(secret, user.KeyPrefix) {
		form.Set("api_key", secret)
	} else {
		form.Set("login", login)
		form.Set("password", secret)
	}
	resp, body := b.postFrom(b.e.mcp.URL, b.e.mcp.URL+oauth.PathSignIn, form)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), b.e.apex.URL+account.CallbackPath+"?code=") {
		t.Fatalf("sign-in: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	resp, body = b.getURL(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("callback: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
}

// page loads the account page of a signed-in browser.
func (b *browser) page() string {
	b.e.t.Helper()
	resp, body := b.get("/account/")
	if resp.StatusCode != http.StatusOK {
		b.e.t.Fatalf("account page: status %d: %s", resp.StatusCode, body)
	}
	return body
}

var (
	formToken = regexp.MustCompile(`name="form_token" value="([^"]+)"`)
	onceValue = regexp.MustCompile(`name="once" value="([^"]+)"`)
)

// token returns the form token of the browser's session.
func (b *browser) token() string {
	b.e.t.Helper()
	m := formToken.FindStringSubmatch(b.page())
	if m == nil {
		b.e.t.Fatal("the account page carries no form token")
	}
	return m[1]
}

// signedIn reports whether the browser still has a session on the apex.
func (b *browser) signedIn() bool {
	b.e.t.Helper()
	resp, _ := b.get("/account/")
	return resp.StatusCode == http.StatusOK
}

// openLink follows a link as a person does: the address with the token,
// then the clean address it redirects to.
func (b *browser) openLink(purpose, token string) (*http.Response, string) {
	b.e.t.Helper()
	resp, _ := b.get("/account/" + purpose + "?token=" + token)
	if resp.StatusCode != http.StatusSeeOther {
		return resp, ""
	}
	if loc := resp.Header.Get("Location"); loc != "/account/"+purpose {
		b.e.t.Fatalf("the link redirected to %q, want the clean address", loc)
	}
	return b.get("/account/" + purpose)
}

// connect has the signed-in person of browser b approve an MCP client, and
// returns the access token it got.
func (b *browser) connect(clientName string) string {
	b.e.t.Helper()
	t := b.e.t
	const redirect = "https://app.example/cb"
	meta, _ := json.Marshal(map[string]any{"client_name": clientName, "redirect_uris": []string{redirect}})
	req, _ := http.NewRequest(http.MethodPost, b.e.mcp.URL+"/oauth/register", strings.NewReader(string(meta)))
	req.Header.Set("Content-Type", "application/json")
	resp, body := b.do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d: %s", resp.StatusCode, body)
	}
	var client struct {
		ID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(body), &client); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	resp, body = b.getURL(b.e.mcp.URL + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {redirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize when signed in: status %d: %s", resp.StatusCode, body)
	}
	var requestID string
	mcp, _ := url.Parse(b.e.mcp.URL)
	for _, c := range b.c.Jar.Cookies(mcp) {
		if c.Name == oauth.InsecureCookieName {
			requestID = c.Value
		}
	}
	resp, body = b.postFrom(b.e.mcp.URL, b.e.mcp.URL+"/oauth/consent", url.Values{"request_id": {requestID}, "action": {"approve"}})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("consent: status %d: %s", resp.StatusCode, body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp, body = b.postFrom(b.e.mcp.URL, b.e.mcp.URL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {redirect},
		"code": {loc.Query().Get("code")}, "code_verifier": {verifier},
	})
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &tokens); err != nil || tokens.AccessToken == "" {
		t.Fatalf("token: status %d: %s", resp.StatusCode, body)
	}
	return tokens.AccessToken
}

var (
	inlineScript = regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`)
	assetRef     = regexp.MustCompile(`<(?:link|script|img)\b[^>]*\b(?:href|src)="([^"]*)"`)
	linkPath     = regexp.MustCompile(`^/account/(invite|reset|setup)$`)
	recoverPath  = regexp.MustCompile(`^/account/recover$`)
)

// checkPage holds every response of the apex to the rules of its pages: the
// strict policy, nothing inline, every asset from the page's own origin,
// and no secret in a redirect or a cookie.
func checkPage(e *env, resp *http.Response, body string) {
	t := e.t
	t.Helper()
	where := resp.Request.Method + " " + resp.Request.URL.Path
	if strings.HasPrefix(resp.Request.URL.Path, "/account/assets/") {
		return
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", where, got)
	}
	for _, secret := range []string{user.LinkPrefix, user.KeyPrefix} {
		if strings.Contains(resp.Header.Get("Location"), secret) {
			t.Errorf("%s: a secret in the redirect to %q", where, resp.Header.Get("Location"))
		}
		for _, c := range resp.Cookies() {
			if strings.Contains(c.Value, secret) && c.Name != "kazari_link" {
				t.Errorf("%s: a secret in the cookie %s", where, c.Name)
			}
		}
	}
	for _, c := range resp.Cookies() {
		if !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("%s: cookie %s = %+v", where, c.Name, c)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	want := middleware.PageCSP
	if recoverPath.MatchString(resp.Request.URL.Path) && strings.Contains(body, "<form") {
		// The recovery form ends on the sign-in host.
		want = strings.Replace(want, "form-action 'self'", "form-action 'self' "+e.mcp.URL, 1)
	}
	if linkPath.MatchString(resp.Request.URL.Path) && strings.Contains(body, "<form") && strings.Contains(body, `name="password"`) {
		// A link's form ends on the sign-in host or the dashboard, and only
		// form-action is widened for it.
		extra := " " + e.mcp.URL
		if strings.Contains(csp, adminURL) {
			extra += " " + adminURL
		}
		want = strings.Replace(want, "form-action 'self'", "form-action 'self'"+extra, 1)
	}
	if csp != want {
		t.Errorf("%s: Content-Security-Policy = %q, want %q", where, csp, want)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Errorf("%s: framing headers = %q, %q", where, resp.Header.Get("X-Frame-Options"), resp.Header.Get("Cross-Origin-Resource-Policy"))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return
	}
	for _, bad := range []string{" style=", "<style", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s: the page holds %q", where, bad)
		}
	}
	if inlineScript.MatchString(body) {
		t.Errorf("%s: the page holds an inline script", where)
	}
	for _, m := range assetRef.FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(m[1], "/account/assets/") {
			t.Errorf("%s: asset %q is not served under /account/assets/", where, m[1])
		}
	}
}
