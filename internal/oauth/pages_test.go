package oauth_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/oauth"
)

var (
	styleAttr    = regexp.MustCompile(`(?i)\sstyle\s*=`)
	handlerAttr  = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)
	inlineScript = regexp.MustCompile(`(?is)<script[^>]*>\s*[^<\s]`)
	otherOrigin  = regexp.MustCompile(`(?i)\s(?:src|href|action)\s*=\s*["']?(?:[a-z][a-z0-9+.-]*:|//)`)
	stylesheet   = regexp.MustCompile(`<link rel="stylesheet" href="([^"]+)">`)
	scriptSrc    = regexp.MustCompile(`<script src="([^"]+)" defer></script>`)
	fontURL      = regexp.MustCompile(`url\("([^"]+\.woff2)"\)`)
)

// checkPage asserts what every page of the sign-in host owes its policy:
// the strict headers, with want as the one origin beside its own that a
// form may go on to, nothing inline, and every file it loads served from
// PathAssets for good.
func (e *env) checkPage(name string, resp *http.Response, body, want string) {
	t := e.t
	t.Helper()
	checkPageHeaders(t, resp)
	if got := formTarget(resp.Header.Get("Content-Security-Policy")); got != want {
		t.Errorf("%s: form-action allows %q beside 'self', want %q", name, got, want)
	}
	if !strings.Contains(body, `<body class="door">`) || !strings.Contains(body, `<main class="door-card">`) {
		t.Errorf("%s: not a front-door page:\n%s", name, body)
	}
	for what, re := range map[string]*regexp.Regexp{
		"a style attribute":           styleAttr,
		"an event handler attribute":  handlerAttr,
		"an inline script":            inlineScript,
		"a reference to another site": otherOrigin,
	} {
		if m := re.FindString(body); m != "" {
			t.Errorf("%s: the page holds %s: %q", name, what, m)
		}
	}
	if strings.Contains(strings.ToLower(body), "<style") {
		t.Errorf("%s: the page holds a style element", name)
	}
	css, js := stylesheet.FindStringSubmatch(body), scriptSrc.FindStringSubmatch(body)
	if css == nil || js == nil {
		t.Fatalf("%s: the page links no stylesheet or script:\n%s", name, body)
	}
	for _, ref := range []string{css[1], js[1]} {
		if !strings.HasPrefix(ref, oauth.PathAssets) {
			t.Errorf("%s: %s is not under %s", name, ref, oauth.PathAssets)
		}
		e.checkAsset(ref)
	}
}

// checkAsset fetches a file a page loads and returns its body.
func (e *env) checkAsset(path string) string {
	e.t.Helper()
	resp, body := e.get(path)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		e.t.Errorf("GET %s: status %d, Cache-Control %q, X-Content-Type-Options %q; want 200, immutable, nosniff",
			path, resp.StatusCode, resp.Header.Get("Cache-Control"), resp.Header.Get("X-Content-Type-Options"))
	}
	return body
}

func TestEveryPageUnderTheStrictPolicy(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)

	resp, body := e.get("/oauth/authorize?client_id=nobody")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("message page: status %d", resp.StatusCode)
	}
	e.checkPage("message", resp, body, "")

	for name, tt := range map[string]struct{ host, origin, mark string }{
		"password sign-in for an artifact host": {"demo", "https://demo.art.test", "https://demo.art.test"},
		"password sign-in for the dashboard":    {oauth.HostAdmin, "https://admin.art.test", "Sign in to the dashboard"},
		"password sign-in for the account page": {oauth.HostAccount, "https://art.test", "Sign in to your account"},
	} {
		resp, body = e.get(signInQuery(tt.host, "/"))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) || !strings.Contains(body, tt.mark) {
			t.Fatalf("%s: status %d:\n%s", name, resp.StatusCode, body)
		}
		e.checkPage(name, resp, body, tt.origin)
	}

	resp, body = e.get(signInQuery("demo", "/") + "&with=key")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="api_key"`) {
		t.Fatalf("API-key form: status %d:\n%s", resp.StatusCode, body)
	}
	e.checkPage("API-key form", resp, body, "https://demo.art.test")

	resp, body = e.passwordSignIn("demo", "ana", "a wrong password")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `role="alert"`) {
		t.Fatalf("refused sign-in: status %d:\n%s", resp.StatusCode, body)
	}
	e.checkPage("refused sign-in", resp, body, "https://demo.art.test")

	resp, body = e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("verifier")).Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="request_id"`) || !strings.Contains(body, `name="password"`) {
		t.Fatalf("consent sign-in: status %d:\n%s", resp.StatusCode, body)
	}
	// This form ends on the consent page, on this host.
	e.checkPage("consent sign-in", resp, body, "")

	resp, body = e.get(oauth.PathConsent + "?with=key")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="api_key"`) || !strings.Contains(body, `value="approve"`) {
		t.Fatalf("key consent: status %d:\n%s", resp.StatusCode, body)
	}
	e.checkPage("key consent", resp, body, "https://app.example")

	e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {"ana"}, "password": {password}})
	resp, body = e.get(oauth.PathConsent)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<strong>ana</strong>") || !strings.Contains(body, `<details class="more">`) {
		t.Fatalf("consent: status %d:\n%s", resp.StatusCode, body)
	}
	e.checkPage("consent", resp, body, "https://app.example")
	// One primary button a view.
	if n := strings.Count(body, "btn-primary"); n != 1 {
		t.Errorf("consent: %d primary buttons, want 1", n)
	}

	resp, body = e.get(oauth.PathSignOut)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="/oauth/signout"`) {
		t.Fatalf("sign-out: status %d:\n%s", resp.StatusCode, body)
	}
	e.checkPage("sign-out", resp, body, "")
}

func TestAssets(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	_, page := e.get(signInQuery("demo", "/"))
	css := stylesheet.FindStringSubmatch(page)
	if css == nil {
		t.Fatalf("no stylesheet in:\n%s", page)
	}
	sheet := e.checkAsset(css[1])
	fonts := fontURL.FindAllStringSubmatch(sheet, -1)
	if len(fonts) == 0 {
		t.Fatal("the stylesheet names no font")
	}
	for _, f := range fonts {
		// Beside the stylesheet, so one policy source covers both.
		if strings.ContainsAny(f[1], "/:") {
			t.Errorf("font %q is not beside the stylesheet", f[1])
			continue
		}
		e.checkAsset(oauth.PathAssets + f[1])
	}
	for _, missing := range []string{oauth.PathAssets, oauth.PathAssets + "theme.css", oauth.PathAssets + "templates.html", oauth.PathAssets + "css/tokens.css"} {
		if resp, _ := e.get(missing); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", missing, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, e.url(css[1]), nil)
	if resp, _ := e.do(req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST %s: status %d, want 405", css[1], resp.StatusCode)
	}
}

// Names that other people chose reach the pages as text, never as markup.
func TestHostileNames(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	const (
		clientName = `<script>alert(1)</script><img src=x onerror=alert(2)>`
		login      = `"><script>alert(3)</script>`
		redirect   = "https://app.example/cb"
	)
	reg, _ := json.Marshal(map[string]any{"client_name": clientName, "redirect_uris": []string{redirect}})
	req, _ := http.NewRequest(http.MethodPost, e.url("/oauth/register"), strings.NewReader(string(reg)))
	req.Header.Set("Content-Type", "application/json")
	resp, raw := e.do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d: %s", resp.StatusCode, raw)
	}
	var client struct {
		ID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(raw), &client); err != nil {
		t.Fatal(err)
	}

	safe := func(name, body string, rendered ...string) {
		t.Helper()
		for _, bad := range []string{"<script>alert", "<img src=x", `"><script`} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: the page holds %q unescaped:\n%s", name, bad, body)
			}
		}
		if m := handlerAttr.FindString(body); m != "" {
			t.Errorf("%s: the page holds an event handler: %q", name, m)
		}
		for _, want := range rendered {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the page does not show %q, so the test proves nothing:\n%s", name, want, body)
			}
		}
	}
	const escapedName = "&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src=x onerror=alert(2)&gt;"

	_, body := e.get("/oauth/authorize?" + authorizeParams(client.ID, redirect, "", newPKCE("verifier")).Encode())
	safe("consent sign-in", body, "Sign in to connect "+escapedName)

	resp, body = e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {login}, "password": {"a wrong password"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refused sign-in: status %d", resp.StatusCode)
	}
	safe("refused sign-in", body, `value="&#34;&gt;&lt;script&gt;alert(3)&lt;/script&gt;"`)

	_, body = e.get(oauth.PathConsent + "?with=key")
	safe("key consent", body, "<strong>"+escapedName+"</strong>")

	e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {"ana"}, "password": {password}})
	_, body = e.get(oauth.PathConsent)
	safe("consent", body, "<strong>"+escapedName+"</strong>", "<strong>ana</strong>")

	resp, body = e.passwordSignIn("demo", login, "a wrong password")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refused sign-in for a host: status %d", resp.StatusCode)
	}
	safe("refused sign-in for a host", body, `&lt;script&gt;alert(3)`)

	v := authorizeParams(client.ID, `https://evil.example/"><script>alert(4)</script>`, "", newPKCE("verifier"))
	resp, body = e.get("/oauth/authorize?" + v.Encode())
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered redirect: status %d", resp.StatusCode)
	}
	safe("message", body, "&lt;script&gt;alert(4)")
}

// The sign-in host says "pages", as every other page a person reads does,
// and spells as the rest of Kazari spells. The scope names are the API's
// and stay as they are.
func TestPagesSpeakOfPages(t *testing.T) {
	e := newEnv(t, envOptions{noIDP: true, passwords: true})
	e.person("ana", "", "")
	const redirect = "https://app.example/cb"
	clientID := e.register(redirect)
	views := map[string]string{}
	_, views["the API-key sign-in"] = e.get(signInQuery("demo", "/") + "&with=key")
	e.get("/oauth/authorize?" + authorizeParams(clientID, redirect, "", newPKCE("verifier")).Encode())
	_, views["the key consent"] = e.get(oauth.PathConsent + "?with=key")
	e.signInForm(url.Values{"request_id": {e.requestCookie()}, "login": {"ana"}, "password": {password}})
	_, consent := e.get(oauth.PathConsent)
	views["the consent"] = consent

	for _, want := range []string{
		"<title>Authorise access", `<h1 class="title">Authorise <b>`,
		"<li>List and read your pages</li>", "Create and update pages", "<summary>Technical details</summary>",
		"Change who can open pages, and create share links", "Delete pages", "its restriction to certain pages",
	} {
		if !strings.Contains(consent, want) {
			t.Errorf("the consent page lacks %q:\n%s", want, consent)
		}
	}
	scopeName := regexp.MustCompile(`artifacts:[a-z]+`)
	for name, body := range views {
		prose := strings.ToLower(scopeName.ReplaceAllString(body, ""))
		for _, word := range []string{"artifact", "authoriz"} {
			if strings.Contains(prose, word) {
				t.Errorf("%s says %q", name, word)
			}
		}
	}
}
