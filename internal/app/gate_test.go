package app_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/cli"
)

// browser is a cookie-keeping client that sends a top-level navigation's
// Fetch Metadata and does not follow redirects.
type browser struct {
	t  *testing.T
	hc *http.Client
}

func newBrowser(t *testing.T, addr string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := client(addr)
	hc.Jar = jar
	return &browser{t: t, hc: hc}
}

func (b *browser) send(method, rawURL string, form url.Values, header http.Header) response {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	maps.Copy(req.Header, header)
	resp, err := b.hc.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

func (b *browser) get(rawURL string) response { return b.send(http.MethodGet, rawURL, nil, nil) }

// follow requires a redirect and returns where it points.
func (b *browser) follow(r response, what string) string {
	b.t.Helper()
	wantStatus(b.t, r, http.StatusFound, what)
	return r.header.Get("Location")
}

// A members artifact on an instance without an identity provider: a
// browser signs in once with an API key, every host of the artifact is then
// opened silently, a share link opens it for someone without a key, and
// revoking the link or signing out takes access away again.
func TestGatedEndToEnd(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter"}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{Label: "browser"})
	if err != nil {
		t.Fatal(err)
	}
	user := cli.NewClient("http://api.localhost", issued.Key, client(addr))
	for _, v := range []string{"v1", "v2"} {
		_, err := user.Update(ctx, "team", api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "index.html", Content: "team " + v}}})
		if cli.IsCode(err, api.CodeNotFound) {
			_, err = user.Create(ctx, api.CreateArtifactRequest{Slug: "team", Visibility: "members",
				Files: []api.FileInput{{Path: "index.html", Content: "team " + v}}})
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// Anonymous: a navigation goes to sign in, anything else gets 401.
	anon := get(t, client(addr), "http://team.localhost/")
	wantStatus(t, anon, http.StatusUnauthorized, "anonymous fetch")
	if strings.Contains(anon.body, "team v") {
		t.Fatal("the 401 carries the page")
	}
	if got := anon.header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("the 401 has Cross-Origin-Resource-Policy %q", got)
	}

	b := newBrowser(t, addr)
	signIn := b.follow(b.get("http://team.localhost/index.html?x=1"), "navigation without access")
	u, err := url.Parse(signIn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "mcp.localhost" || u.Path != "/oauth/signin" || u.Query().Get("host") != "team" || u.Query().Get("path") != "/index.html?x=1" {
		t.Fatalf("sign-in URL = %s", signIn)
	}
	form := b.get(signIn)
	wantStatus(t, form, http.StatusOK, "sign-in page")
	// Built-in accounts are on without a provider: the page asks for a
	// password, with the API-key form one link away.
	if !strings.Contains(form.body, `name="password"`) || !strings.Contains(form.body, "with=key") {
		t.Fatalf("sign-in page = %q", truncate(form.body))
	}
	if keyForm := b.get(signIn + "&with=key"); !strings.Contains(keyForm.body, `name="api_key"`) {
		t.Fatalf("API-key sign-in page = %q", truncate(keyForm.body))
	}
	q := u.Query()
	callback := b.follow(b.send(http.MethodPost, "http://mcp.localhost/oauth/signin", url.Values{
		"host": {q.Get("host")}, "path": {q.Get("path")}, "nonce": {q.Get("nonce")}, "api_key": {issued.Key},
	}, http.Header{"Origin": {"http://mcp.localhost"}}), "API-key sign-in")
	if !strings.HasPrefix(callback, "http://team.localhost/.kazari/callback?code=") {
		t.Fatalf("callback = %s", callback)
	}
	if back := b.follow(b.get(callback), "callback"); back != "/index.html?x=1" {
		t.Fatalf("callback returned to %q", back)
	}
	page := b.get("http://team.localhost/")
	wantStatus(t, page, http.StatusOK, "signed-in page")
	if page.body != "team v2" || page.header.Get("Cache-Control") != "private, no-cache" ||
		page.header.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("page = %q, headers %v", page.body, page.header)
	}
	// A cross-site opener must not tell these responses apart, with access
	// or without, by the opener policy of where the navigation ended, and
	// the sign-in page keeps its own.
	for name, r := range map[string]response{
		"redirect to sign in": newBrowser(t, addr).get("http://team.localhost/"),
		"refused fetch":       anon,
		"sign-in page":        form,
		"gate page":           newBrowser(t, addr).get("http://team.localhost/.kazari/share?token=nope"),
		"page":                page,
		"missing page":        b.get("http://team.localhost/nope"),
	} {
		if got := r.header.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
			t.Errorf("%s: Cross-Origin-Opener-Policy = %q, want same-origin", name, got)
		}
	}
	// A page the artifact does not have answers with the policy of one it has.
	miss := b.get("http://team.localhost/nope.bin")
	wantStatus(t, miss, http.StatusNotFound, "signed-in miss")
	if got := miss.header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("the 404 has Cross-Origin-Resource-Policy %q", got)
	}
	// A replayed callback is refused.
	wantStatus(t, b.get(callback), http.StatusBadRequest, "replayed callback")

	// A version host needs its own cookie, which the session on the MCP
	// host hands out without asking again.
	signIn = b.follow(b.get("http://team--v1.localhost/"), "version host")
	callback = b.follow(b.get(signIn), "silent sign-in")
	b.follow(b.get(callback), "version callback")
	v1 := b.get("http://team--v1.localhost/")
	wantStatus(t, v1, http.StatusOK, "version host page")
	// Never "immutable": the browser must ask again, or a sign-out would
	// not reach a version it has cached.
	if v1.body != "team v1" || v1.header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("v1 = %q, Cache-Control %q", v1.body, v1.header.Get("Cache-Control"))
	}

	// A share link opens the page for a browser that never signed in.
	created, err := user.CreateShareLink(ctx, "team", "reviewer", "2d", "")
	if err != nil {
		t.Fatal(err)
	}
	guest := newBrowser(t, addr)
	if back := guest.follow(guest.get(created.URL), "share link"); back != "/" {
		t.Fatalf("share link returned to %q", back)
	}
	if got := guest.get("http://team.localhost/"); got.status != http.StatusOK || got.body != "team v2" {
		t.Fatalf("guest page: %d %q", got.status, got.body)
	}
	if err := user.RevokeShareLink(ctx, "team", created.ShareLink.Prefix); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, guest.get("http://team.localhost/"), http.StatusFound, "guest after revoke")
	wantStatus(t, guest.get(created.URL), http.StatusForbidden, "revoked share link")

	// A link with a password opens a form, and the URL alone opens nothing.
	locked, err := user.CreateShareLink(ctx, "team", "client", "2d", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !locked.ShareLink.Password {
		t.Fatalf("created link = %+v, want password true", locked.ShareLink)
	}
	visitor := newBrowser(t, addr)
	moved := visitor.get(locked.URL)
	wantStatus(t, moved, http.StatusSeeOther, "link with a password")
	if loc := moved.header.Get("Location"); loc != "/.kazari/share" {
		t.Fatalf("link with a password went on to %q, want the share path without the token", loc)
	}
	asked := visitor.get("http://team.localhost/.kazari/share")
	if asked.status != http.StatusOK || !strings.Contains(asked.body, `name="password"`) {
		t.Fatalf("the password form: %d, want 200 and the form; body %q", asked.status, truncate(asked.body))
	}
	wantStatus(t, visitor.get("http://team.localhost/"), http.StatusFound, "page before the password")
	post := func(password string) response {
		return visitor.send(http.MethodPost, "http://team.localhost/.kazari/share",
			url.Values{"password": {password}}, http.Header{"Origin": {"http://team.localhost"}})
	}
	wantStatus(t, post("open says me please"), http.StatusForbidden, "wrong password")
	if back := visitor.follow(post("correct horse battery"), "right password"); back != "/" {
		t.Fatalf("right password returned to %q", back)
	}
	if got := visitor.get("http://team.localhost/"); got.status != http.StatusOK || got.body != "team v2" {
		t.Fatalf("page after the password: %d %q", got.status, got.body)
	}

	// Signing out on the MCP host ends the browser's access on every host.
	wantStatus(t, b.get("http://mcp.localhost/oauth/signout"), http.StatusOK, "sign-out page")
	out := b.send(http.MethodPost, "http://mcp.localhost/oauth/signout", url.Values{}, http.Header{"Origin": {"http://mcp.localhost"}})
	wantStatus(t, out, http.StatusOK, "sign out")
	wantStatus(t, b.get("http://team--v1.localhost/"), http.StatusFound, "version host after sign-out")
	if loc := b.follow(b.get("http://team.localhost/"), "after sign-out"); !strings.HasPrefix(loc, "http://mcp.localhost/oauth/signin?") {
		t.Fatalf("after sign-out redirected to %s", loc)
	}
	// The session on the MCP host is gone too: the sign-in asks for a key.
	signIn = b.follow(b.get("http://team.localhost/"), "after sign-out")
	wantStatus(t, b.get(signIn), http.StatusOK, "sign-in page after sign-out")

	// Public pages are untouched by all of this, and no artifact may ship
	// files under the reserved directory.
	if _, err := user.Create(ctx, api.CreateArtifactRequest{Slug: "open",
		Files: []api.FileInput{{Path: ".kazari/share", Content: "x"}}}); !cli.IsCode(err, api.CodeInvalidPath) {
		t.Fatalf("a file under .kazari/ was accepted: %v", err)
	}
}

// Disabling a user ends every share link they created, on any member's
// artifact, including a browser that already opened one. Enabling the user
// does not bring the links back, and other people's links are untouched.
func TestDisableRevokesShareLinks(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	member := func(handle string) *cli.Client {
		t.Helper()
		if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: handle}); err != nil {
			t.Fatal(err)
		}
		issued, err := admin.IssueKey(ctx, handle, api.IssueKeyRequest{Label: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return cli.NewClient("http://api.localhost", issued.Key, client(addr))
	}
	owner, leaver := member("owner"), member("leaver")
	if _, err := owner.Create(ctx, api.CreateArtifactRequest{Slug: "team", Visibility: "members",
		Files: []api.FileInput{{Path: "index.html", Content: "team page"}}}); err != nil {
		t.Fatal(err)
	}
	theirs, err := leaver.CreateShareLink(ctx, "team", "kept", "90d", "")
	if err != nil {
		t.Fatal(err)
	}
	reviewers, err := owner.CreateShareLink(ctx, "team", "reviewer", "2d", "")
	if err != nil {
		t.Fatal(err)
	}
	opened, reviewer := newBrowser(t, addr), newBrowser(t, addr)
	opened.follow(opened.get(theirs.URL), "link before the disable")
	reviewer.follow(reviewer.get(reviewers.URL), "reviewer's link")
	wantStatus(t, opened.get("http://team.localhost/"), http.StatusOK, "page before the disable")

	disabled := true
	if _, err := admin.UpdateUser(ctx, "leaver", api.UpdateUserRequest{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	gone := func(when string) {
		t.Helper()
		wantStatus(t, opened.get("http://team.localhost/"), http.StatusFound, "open browser "+when)
		fresh := newBrowser(t, addr)
		wantStatus(t, fresh.get(theirs.URL), http.StatusForbidden, "opening the link "+when)
		wantStatus(t, fresh.get("http://team.localhost/"), http.StatusFound, "fresh browser "+when)
	}
	gone("after the disable")
	wantStatus(t, reviewer.get("http://team.localhost/"), http.StatusOK, "another member's link")

	disabled = false
	if _, err := admin.UpdateUser(ctx, "leaver", api.UpdateUserRequest{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	gone("after enabling the user")

	links, err := owner.ListShareLinks(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links.Items {
		if want := l.Prefix == reviewers.ShareLink.Prefix; l.Active != want {
			t.Errorf("link by %s: active = %v, want %v", l.CreatedBy, l.Active, want)
		}
	}
	events, err := admin.Audit(ctx, cli.AuditParams{Slug: "team"})
	if err != nil {
		t.Fatal(err)
	}
	var revokes []api.AuditEvent
	for _, ev := range events.Events {
		if ev.Action == audit.ShareLinkRevoke {
			revokes = append(revokes, ev)
		}
	}
	if len(revokes) != 1 || revokes[0].Actor != "admin" ||
		revokes[0].Details["prefix"] != theirs.ShareLink.Prefix || revokes[0].Details["created_by"] != "leaver" {
		t.Errorf("share_link.revoke events = %+v, want one by the admin", revokes)
	}
}

// Script on one artifact host runs on the same origin as the gate, so what
// it can post there must not reach past that host: the visitor keeps their
// sign-in and every other host. Only the page on the MCP host ends it.
func TestArtifactScriptCannotEndSignIn(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter"}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{Label: "browser"})
	if err != nil {
		t.Fatal(err)
	}
	user := cli.NewClient("http://api.localhost", issued.Key, client(addr))
	for _, slug := range []string{"team", "notes"} {
		if _, err := user.Create(ctx, api.CreateArtifactRequest{Slug: slug, Visibility: "members",
			Files: []api.FileInput{{Path: "index.html", Content: slug}}}); err != nil {
			t.Fatal(err)
		}
	}

	b := newBrowser(t, addr)
	signIn := b.follow(b.get("http://team.localhost/"), "first navigation")
	u, err := url.Parse(signIn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	callback := b.follow(b.send(http.MethodPost, "http://mcp.localhost/oauth/signin", url.Values{
		"host": {q.Get("host")}, "path": {q.Get("path")}, "nonce": {q.Get("nonce")}, "api_key": {issued.Key},
	}, http.Header{"Origin": {"http://mcp.localhost"}}), "API-key sign-in")
	b.follow(b.get(callback), "callback")
	// The second host opens with no prompt.
	silent := b.follow(b.get("http://notes.localhost/"), "second host")
	b.follow(b.get(b.follow(b.get(silent), "silent sign-in")), "second callback")
	wantStatus(t, b.get("http://notes.localhost/"), http.StatusOK, "second host signed in")

	// What page script on notes can send: a same-origin fetch.
	script := http.Header{"Origin": {"http://notes.localhost"}, "Sec-Fetch-Site": {"same-origin"},
		"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}}
	out := b.send(http.MethodPost, "http://notes.localhost/.kazari/signout", url.Values{}, script)
	wantStatus(t, out, http.StatusOK, "sign-out on the artifact host")
	wantStatus(t, b.get("http://team.localhost/"), http.StatusOK, "another host after sign-out on notes")
	// notes itself lost its view session, and comes back with no prompt.
	back := b.follow(b.get("http://notes.localhost/"), "notes after its sign-out")
	if code := b.follow(b.get(back), "silent sign-in after sign-out on notes"); !strings.HasPrefix(code, "http://notes.localhost/.kazari/callback?code=") {
		t.Fatalf("the sign-in did not survive: went to %s", code)
	}

	// The same script cannot use the MCP host's form either.
	script = http.Header{"Origin": {"http://notes.localhost"}, "Sec-Fetch-Site": {"same-site"}}
	wantStatus(t, b.send(http.MethodPost, "http://mcp.localhost/oauth/signout", url.Values{}, script), http.StatusForbidden, "sign-out from an artifact host")
	wantStatus(t, b.get("http://team.localhost/"), http.StatusOK, "after a refused sign-out")

	// A person using the page on the MCP host ends the sign-in everywhere.
	page := b.get("http://mcp.localhost/oauth/signout")
	wantStatus(t, page, http.StatusOK, "sign-out page")
	if !strings.Contains(page.body, `action="/oauth/signout"`) {
		t.Fatalf("sign-out page = %q", truncate(page.body))
	}
	wantStatus(t, b.send(http.MethodPost, "http://mcp.localhost/oauth/signout", url.Values{}, http.Header{"Origin": {"http://mcp.localhost"}}), http.StatusOK, "sign out")
	again := b.follow(b.get("http://team.localhost/"), "after signing out")
	if got := b.get(again); got.status != http.StatusOK || !strings.Contains(got.body, `name="password"`) {
		t.Fatalf("after signing out: status %d, body %q", got.status, truncate(got.body))
	}
}
