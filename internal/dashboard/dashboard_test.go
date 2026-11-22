package dashboard_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/user"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

const password = "correct horse battery"

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

// env is an instance with built-in accounts: the dashboard on one test
// server, the sign-in host on another, over real SQLite and a real blob
// store.
type env struct {
	t         *testing.T
	clock     *clock
	users     *user.Service
	artifacts *artifact.Service
	oauth     *oauth.Service
	audit     *sqlite.AuditStore
	jobs      *scheduler.Scheduler
	admin     *httptest.Server // the dashboard
	signIn    *httptest.Server // the sign-in host
	deps      dashboard.Deps
	opts      dashboard.Options
	// wide counts the queries that read every person or every owner.
	wide atomic.Int64
}

// wideUsers and wideArtifacts are the stores, counting the queries whose
// cost grows with the instance.
type wideUsers struct {
	user.Store
	n *atomic.Int64
}

func (s wideUsers) ListUsers(ctx context.Context) ([]user.User, error) {
	s.n.Add(1)
	return s.Store.ListUsers(ctx)
}

func (s wideUsers) ListAccounts(ctx context.Context, now time.Time) ([]user.Account, error) {
	s.n.Add(1)
	return s.Store.ListAccounts(ctx, now)
}

type wideArtifacts struct {
	artifact.Store
	n *atomic.Int64
}

func (s wideArtifacts) OwnerUsage(ctx context.Context) ([]artifact.OwnerUsage, error) {
	s.n.Add(1)
	return s.Store.OwnerUsage(ctx)
}

// newEnv starts an instance. mutate, when not nil, changes the dashboard's
// dependencies and options before it is built.
func newEnv(t *testing.T, mutate func(*dashboard.Deps, *dashboard.Options)) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.NewFS(filepath.Join(dir, "blobs"), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, clock: &clock{now: t0}, audit: sqlite.NewAuditStore(db)}
	e.users = user.New(wideUsers{sqlite.NewUserStore(db), &e.wide}, e.audit, user.Options{
		Now:           e.clock.Now,
		LocalAccounts: true,
		// The real parameters cost 64 MiB a hash.
		Argon2: user.Argon2{Memory: 64, Time: 1, Threads: 1},
		OnDisable: func(ctx context.Context, handle string) error {
			return e.oauth.RevokeAllForUser(ctx, handle)
		},
		EndSessions: func(ctx context.Context, handle string, keep int64) error {
			return e.oauth.EndSessions(ctx, handle, keep)
		},
		RevokeGrants: func(ctx context.Context, handle string) error { return e.oauth.RevokeAllForUser(ctx, handle) },
	})
	e.artifacts = artifact.New(wideArtifacts{sqlite.NewArtifactStore(db), &e.wide}, blobs, e.audit, artifact.Options{
		Quotas: e.users, Now: e.clock.Now, Retention: 30 * 24 * time.Hour, Tombstone: 90 * 24 * time.Hour,
		DefaultGrace: 14 * 24 * time.Hour, MaxShareLinkTTL: 90 * 24 * time.Hour, DefaultVisibility: artifact.Public,
	})
	e.jobs = scheduler.New(scheduler.Options{Now: e.clock.Now},
		scheduler.Job{Name: "sweep", Every: 15 * time.Minute, Run: func(ctx context.Context) error {
			_, err := e.artifacts.Sweep(ctx)
			return err
		}})

	e.admin = httptest.NewUnstartedServer(nil)
	e.signIn = httptest.NewUnstartedServer(nil)
	adminURL := "http://" + e.admin.Listener.Addr().String()
	signInURL := "http://" + e.signIn.Listener.Addr().String()
	e.oauth = oauth.New(sqlite.NewOAuthStore(db), e.users, e.users, nil, e.audit, oauth.Options{
		Issuer: signInURL, Resource: signInURL + "/", Now: e.clock.Now, InsecureCookies: true, Passwords: e.users,
		ControlCallback: func(host string) (string, bool) {
			return adminURL + dashboard.CallbackPath, host == oauth.HostAdmin
		},
	})
	e.signIn.Config.Handler = e.oauth.Handler()

	deps := dashboard.Deps{
		Sessions: e.oauth, Users: e.users, Artifacts: e.artifacts, Grants: e.oauth, Audit: e.audit, Recorder: e.audit,
		Jobs: e.jobs, Links: present.Links{URL: func(sub string) string {
			if sub == "" {
				return "https://art.test/"
			}
			return "https://" + sub + ".art.test/"
		}},
	}
	e.opts = dashboard.Options{
		URL: adminURL, SignInURL: signInURL + oauth.PathSignIn, AccountURL: "https://art.test/account/",
		MCPURL: "https://mcp.art.test/", APIURL: "https://api.art.test/v1", BaseDomain: "art.test",
		LocalAccounts: true, InsecureCookies: true, Now: e.clock.Now, Started: t0, DataDir: dir,
		Retention: 30 * 24 * time.Hour, Tombstone: 90 * 24 * time.Hour, MaxShareLinkTTL: 90 * 24 * time.Hour,
	}
	if mutate != nil {
		mutate(&deps, &e.opts)
	}
	e.deps = deps
	// As internal/app mounts it: the control hosts' default headers first.
	e.admin.Config.Handler = middleware.SecurityHeaders()(dashboard.New(deps, e.opts))
	e.admin.Start()
	e.signIn.Start()
	t.Cleanup(e.admin.Close)
	t.Cleanup(e.signIn.Close)
	return e
}

// asAdmin is the context of a change the test makes as the operator.
func asAdmin() context.Context {
	return auth.ContextWithPrincipal(context.Background(), auth.Principal{Handle: "admin", Admin: true, Source: auth.SourceLocal})
}

// person creates a person with a password.
func (e *env) person(handle string, role user.Role) *user.User {
	e.t.Helper()
	u, _, token, err := e.users.Invite(asAdmin(), user.InviteRequest{Handle: handle, Role: role})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.users.Redeem(context.Background(), token, user.RedeemRequest{Password: password}); err != nil {
		e.t.Fatal(err)
	}
	return u
}

// publish creates an artifact owned by handle with one page.
func (e *env) publish(handle, slug string) *artifact.Artifact {
	e.t.Helper()
	a, _, err := e.artifacts.Create(context.Background(), artifact.Actor{Handle: handle}, artifact.CreateRequest{
		Slug: slug, Name: slug, Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("<h1>" + slug + "</h1>")}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// browser is one browser: a cookie jar that follows no redirect by itself.
type browser struct {
	e      *env
	client *http.Client
	token  string // the form token of the last page that carried one
	once   string // and its single-use value
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, client: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type response struct {
	*http.Response
	body string
}

func (b *browser) do(req *http.Request) response {
	b.e.t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.e.t.Fatal(err)
	}
	if m := tokenRE.FindStringSubmatch(string(body)); m != nil {
		b.token = m[1]
	}
	if m := onceRE.FindStringSubmatch(string(body)); m != nil {
		b.once = m[1]
	}
	return response{resp, string(body)}
}

var (
	tokenRE = regexp.MustCompile(`name="form_token" value="([^"]+)"`)
	onceRE  = regexp.MustCompile(`name="once" value="([^"]+)"`)
)

// navigate is a GET as a browser tab makes it. A path is on the dashboard;
// a full URL goes where it says.
func (b *browser) navigate(target string) response {
	b.e.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = b.e.admin.URL + target
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		b.e.t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Site", "none")
	return b.do(req)
}

// get is navigate following redirects on the dashboard itself, and fails
// the test unless the page answers 200.
func (b *browser) get(path string) response {
	b.e.t.Helper()
	r := b.navigate(path)
	for range 5 {
		if r.StatusCode != http.StatusSeeOther && r.StatusCode != http.StatusFound {
			break
		}
		loc := r.Header.Get("Location")
		if !strings.HasPrefix(loc, "/") {
			break
		}
		r = b.navigate(loc)
	}
	if r.StatusCode != http.StatusOK {
		b.e.t.Fatalf("GET %s: status %d: %s", path, r.StatusCode, r.body)
	}
	return r
}

// post submits a form of the dashboard, with the form token and the
// single-use value of the last page loaded unless the form names its own.
func (b *browser) post(path string, form url.Values) response {
	b.e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if _, ok := form["form_token"]; !ok {
		form.Set("form_token", b.token)
	}
	if _, ok := form["once"]; !ok {
		form.Set("once", b.once)
	}
	return b.postRaw(path, form, b.e.admin.URL)
}

// postRaw posts form as it is, from a page of origin.
func (b *browser) postRaw(path string, form url.Values, origin string) response {
	b.e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.e.admin.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		b.e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return b.do(req)
}

// submit is post for a change that ends in a redirect: it follows it and
// returns the page with its confirmation line.
func (b *browser) submit(path string, form url.Values) response {
	b.e.t.Helper()
	r := b.post(path, form)
	if r.StatusCode != http.StatusSeeOther {
		b.e.t.Fatalf("POST %s: status %d, want 303: %s", path, r.StatusCode, r.body)
	}
	return b.get(r.Header.Get("Location"))
}

// signedIn returns a browser signed in to the dashboard as handle, through
// the sign-in host's password form.
func (e *env) signedIn(handle string) *browser {
	e.t.Helper()
	b := e.browser()
	r := b.navigate("/")
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), e.signIn.URL) {
		e.t.Fatalf("dashboard without a session: status %d to %q", r.StatusCode, r.Header.Get("Location"))
	}
	b.signInAt(r.Header.Get("Location"), url.Values{"login": {handle}, "password": {password}})
	return b
}

// signInAt completes the sign-in the dashboard started at signInURL with
// the credentials in form, and loads the dashboard's callback.
func (b *browser) signInAt(signInURL string, form url.Values) response {
	b.e.t.Helper()
	u, err := url.Parse(signInURL)
	if err != nil {
		b.e.t.Fatal(err)
	}
	for _, k := range []string{"host", "path", "nonce"} {
		form.Set(k, u.Query().Get(k))
	}
	req, _ := http.NewRequest(http.MethodPost, b.e.signIn.URL+oauth.PathSignIn, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.e.signIn.URL)
	r := b.do(req)
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), b.e.admin.URL+dashboard.CallbackPath) {
		b.e.t.Fatalf("sign-in: status %d to %q: %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	return b.navigate(r.Header.Get("Location"))
}

// events returns the audit events with action, newest first.
func (e *env) events(action string) []audit.Event {
	e.t.Helper()
	p, err := e.audit.List(context.Background(), audit.Query{Actions: []string{action}})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Events
}

func TestSignInToDashboard(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.browser()

	r := b.navigate("/people?role=admin")
	loc, err := url.Parse(r.Header.Get("Location"))
	if err != nil || r.StatusCode != http.StatusFound || loc.Query().Get("host") != oauth.HostAdmin || loc.Query().Get("path") != "/people?role=admin" {
		t.Fatalf("without a session: status %d to %q", r.StatusCode, r.Header.Get("Location"))
	}
	r = b.signInAt(r.Header.Get("Location"), url.Values{"login": {"boss"}, "password": {password}})
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/people?role=admin" {
		t.Fatalf("callback: status %d to %q: %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	ev := e.events(audit.DashboardSignIn)
	if len(ev) != 1 || ev[0].Actor != "boss" || ev[0].Details["credential"] != "session" {
		t.Errorf("sign-in audit = %+v", ev)
	}

	r = b.navigate("/no-such-page")
	if r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "boss · admin") || !strings.Contains(r.body, `action="/session/signout"`) {
		t.Fatalf("a page of the dashboard: status %d: %s", r.StatusCode, r.body)
	}
	for name, want := range map[string]string{
		"Content-Security-Policy":      middleware.PageCSP,
		"Cross-Origin-Resource-Policy": "same-origin",
		"X-Frame-Options":              "DENY",
		"Cache-Control":                "no-store",
		"Referrer-Policy":              "no-referrer",
	} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// Signing out ends the sign-in everywhere.
	r = b.post("/session/signout", nil)
	if r.StatusCode != http.StatusOK || !strings.Contains(r.body, "Signed out") {
		t.Fatalf("sign-out: status %d: %s", r.StatusCode, r.body)
	}
	if r = b.navigate("/"); r.StatusCode != http.StatusFound {
		t.Errorf("after sign-out: status %d, want a redirect to sign in", r.StatusCode)
	}
	if n, _ := e.oauth.CountSessions(context.Background(), "boss"); n != 0 {
		t.Errorf("%d sessions outlived the sign-out", n)
	}
}

func TestCallbackRejectsForeignCode(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.browser()
	r := b.navigate("/")
	u, _ := url.Parse(r.Header.Get("Location"))
	form := url.Values{"login": {"boss"}, "password": {password}, "host": {u.Query().Get("host")}, "path": {"/"}, "nonce": {u.Query().Get("nonce")}}
	req, _ := http.NewRequest(http.MethodPost, e.signIn.URL+oauth.PathSignIn, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.signIn.URL)
	callback := b.do(req).Header.Get("Location")

	// The code is bound to the browser that asked for it.
	thief := e.browser()
	r = thief.navigate(callback)
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(r.body, "Sign-in failed") {
		t.Fatalf("a code in another browser: status %d: %s", r.StatusCode, r.body)
	}
	if r = thief.navigate("/"); r.StatusCode != http.StatusFound {
		t.Errorf("the other browser was let in: status %d", r.StatusCode)
	}
	if ev := e.events(audit.DashboardSignIn); len(ev) != 0 {
		t.Errorf("a refused sign-in was recorded: %+v", ev)
	}
}

func TestMemberIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	b := e.browser()
	r := b.navigate("/")
	r = b.signInAt(r.Header.Get("Location"), url.Values{"login": {"ana"}, "password": {password}})
	if r.StatusCode != http.StatusFound {
		t.Fatalf("callback: status %d", r.StatusCode)
	}
	r = b.navigate("/")
	if r.StatusCode != http.StatusForbidden || !strings.Contains(r.body, "Not an admin") || !strings.Contains(r.body, "https://art.test/account/") {
		t.Fatalf("a member on the dashboard: status %d: %s", r.StatusCode, r.body)
	}
	if strings.Contains(r.body, `class="nav"`) {
		t.Error("a member was shown the dashboard's navigation")
	}
	if ev := e.events(audit.DashboardSignIn); len(ev) != 0 {
		t.Errorf("a member's sign-in was recorded as a dashboard sign-in: %+v", ev)
	}
	// The page offers the member a way out, which must work for them.
	if r = b.post("/session/signout", nil); r.StatusCode != http.StatusOK || !strings.Contains(r.body, "Signed out") {
		t.Errorf("a member signing out: status %d: %s", r.StatusCode, r.body)
	}
	if n, _ := e.oauth.CountSessions(context.Background(), "ana"); n != 0 {
		t.Errorf("%d sessions outlived the member's sign-out", n)
	}
}

// A session opened with an API key has admin rights only when the key
// carries the admin scope.
func TestKeySessionNeedsAdminScope(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	ctx := context.Background()
	_, memberKey, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Scopes: auth.MemberScopes})
	if err != nil {
		t.Fatal(err)
	}
	adminKey, adminSecret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Scopes: auth.AllScopes})
	if err != nil {
		t.Fatal(err)
	}

	b := e.browser()
	r := b.navigate("/")
	b.signInAt(r.Header.Get("Location"), url.Values{"api_key": {memberKey}})
	if r = b.navigate("/"); r.StatusCode != http.StatusForbidden || !strings.Contains(r.body, "does not hold the admin scope") {
		t.Fatalf("an admin's member key on the dashboard: status %d: %s", r.StatusCode, r.body)
	}

	b = e.browser()
	r = b.navigate("/")
	b.signInAt(r.Header.Get("Location"), url.Values{"api_key": {adminSecret}})
	if r = b.navigate("/x"); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "boss · admin") {
		t.Fatalf("an admin key on the dashboard: status %d: %s", r.StatusCode, r.body)
	}
	if ev := e.events(audit.DashboardSignIn); len(ev) != 1 || ev[0].Details["credential"] != "session:"+adminKey.Prefix {
		t.Errorf("sign-in audit = %+v", ev)
	}
	// The session ends with the key.
	if err := e.users.RevokeKey(ctx, user.KeyRef{Handle: "boss", Prefix: adminKey.Prefix}); err != nil {
		t.Fatal(err)
	}
	if r = b.navigate("/x"); r.StatusCode != http.StatusFound {
		t.Errorf("after the key was revoked: status %d, want a redirect to sign in", r.StatusCode)
	}
}

// The role and the disabled state are checked on every request.
func TestAdminChecksEveryRequest(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("second", user.RoleAdmin)
	b := e.signedIn("second")
	if r := b.navigate("/x"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", r.StatusCode)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "second", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Fatal(err)
	}
	if r := b.navigate("/x"); r.StatusCode != http.StatusForbidden {
		t.Errorf("after a demotion: status %d, want 403", r.StatusCode)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "second", user.UpdateUserRequest{Role: ptr(user.RoleAdmin)}); err != nil {
		t.Fatal(err)
	}
	if r := b.navigate("/x"); r.StatusCode != http.StatusNotFound {
		t.Errorf("after being made an admin again: status %d", r.StatusCode)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "second", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if r := b.navigate("/x"); r.StatusCode != http.StatusFound {
		t.Errorf("after being disabled: status %d, want a redirect to sign in", r.StatusCode)
	}
}

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

func TestExpiredSession(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	b.navigate("/x")
	e.clock.Advance(oauth.DefaultSessionTTL)
	if r := b.navigate("/people"); r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), e.signIn.URL) {
		t.Errorf("an expired session: status %d to %q, want a redirect to sign in", r.StatusCode, r.Header.Get("Location"))
	}
	// A request that is no page load is told, not redirected.
	req, _ := http.NewRequest(http.MethodGet, e.admin.URL+"/people", nil)
	if r := b.do(req); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired session, not a navigation: status %d, want 401", r.StatusCode)
	}
	if r := b.post("/session/signout", nil); r.StatusCode != http.StatusOK || !strings.Contains(r.body, "Signed out") {
		t.Errorf("signing out after the session expired: status %d: %s", r.StatusCode, r.body)
	}
}

func TestChangesNeedOriginAndToken(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	b.navigate("/x")
	good := url.Values{"form_token": {b.token}}

	for name, origin := range map[string]string{
		"a sibling artifact": "https://demo.art.test",
		"the sign-in host":   e.signIn.URL,
		"another site":       "https://evil.example",
	} {
		if r := b.postRaw("/session/signout", good, origin); r.StatusCode != http.StatusForbidden {
			t.Errorf("a form posted from %s: status %d, want 403", name, r.StatusCode)
		}
	}
	for name, form := range map[string]url.Values{
		"no token":         {},
		"an empty token":   {"form_token": {""}},
		"a made-up token":  {"form_token": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
		"the cookie value": {"form_token": {cookieValue(t, b, e.admin.URL, "kazari_admin")}},
	} {
		r := b.postRaw("/session/signout", form, e.admin.URL)
		if r.StatusCode != http.StatusForbidden || !strings.Contains(r.body, "This form has expired") {
			t.Errorf("a form with %s: status %d: %s", name, r.StatusCode, r.body)
		}
	}
	if n, _ := e.oauth.CountSessions(context.Background(), "boss"); n != 1 {
		t.Fatalf("a refused form signed the admin out: %d sessions", n)
	}
	// Nothing changes on a GET.
	if r := b.navigate("/session/signout"); r.StatusCode == http.StatusOK || r.StatusCode == http.StatusFound {
		t.Errorf("GET of an action: status %d", r.StatusCode)
	}
	if n, _ := e.oauth.CountSessions(context.Background(), "boss"); n != 1 {
		t.Errorf("a GET signed the admin out: %d sessions", n)
	}
	// Another admin's token is not this session's.
	e.person("second", user.RoleAdmin)
	other := e.signedIn("second")
	other.navigate("/x")
	if r := b.postRaw("/session/signout", url.Values{"form_token": {other.token}}, e.admin.URL); r.StatusCode != http.StatusForbidden {
		t.Errorf("a form with another session's token: status %d, want 403", r.StatusCode)
	}
}

func cookieValue(t *testing.T, b *browser, rawURL, name string) string {
	t.Helper()
	u, _ := url.Parse(rawURL)
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	t.Fatalf("no cookie %s", name)
	return ""
}

// A page of another origin cannot read the dashboard, whatever cookies the
// browser sends.
func TestCrossOriginReadsRefused(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	req, _ := http.NewRequest(http.MethodGet, e.admin.URL+"/people", nil)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	if r := b.do(req); r.StatusCode != http.StatusForbidden {
		t.Errorf("an artifact's fetch of a dashboard page: status %d, want 403", r.StatusCode)
	}
}

func TestAssets(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	r := b.navigate("/x")
	m := regexp.MustCompile(`href="(/assets/theme\.[0-9a-f]+\.css)"`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no stylesheet in %s", r.body)
	}
	// The stylesheet needs no session, and alone may be cached.
	css := e.browser().navigate(m[1])
	if css.StatusCode != http.StatusOK || !strings.Contains(css.Header.Get("Cache-Control"), "immutable") ||
		!strings.HasPrefix(css.Header.Get("Content-Type"), "text/css") {
		t.Errorf("stylesheet: status %d, Cache-Control %q, Content-Type %q", css.StatusCode, css.Header.Get("Cache-Control"), css.Header.Get("Content-Type"))
	}
}
