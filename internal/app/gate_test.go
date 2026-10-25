package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"kazari/internal/api"
	"kazari/internal/cli"
)

// browser is a cookie-keeping client that sends a top-level navigation's
// Fetch Metadata and does not follow redirects.
type browser struct {
	t  *testing.T
	hc *http.Client
}

func newBrowser(t *testing.T, addr string) *browser {
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
	for k, v := range header {
		req.Header[k] = v
	}
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
	if !strings.Contains(form.body, `name="api_key"`) {
		t.Fatalf("sign-in page = %q", truncate(form.body))
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
	created, err := user.CreateShareLink(ctx, "team", "reviewer", "2d")
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

	// Signing out on one host ends the browser's access on every host.
	wantStatus(t, b.get("http://team.localhost/.kazari/signout"), http.StatusOK, "sign-out page")
	out := b.send(http.MethodPost, "http://team.localhost/.kazari/signout", url.Values{}, http.Header{"Origin": {"http://team.localhost"}})
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
