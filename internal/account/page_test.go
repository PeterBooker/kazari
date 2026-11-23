package account_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/signin"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestAccountPage(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	e.person("ben", "")
	e.page("ana", "q3-report", "Q3 report", artifact.Members)
	e.page("ben", "bens-page", "Not hers", artifact.Public)
	b := e.browser()
	b.signIn("ana", password)
	page := b.page()
	for _, want := range []string{
		`<span class="ja" lang="ja" aria-hidden="true">アカウント</span>`, `<title>Account · Kazari</title>`,
		"Only an admin of this instance changes these", `action="/account/password"`, "Sign out of my other sessions",
		"Signed in with the password", "You hold no live key", "No app is connected",
		`<a href="https://q3-report.art.test/" rel="noreferrer">Q3 report</a>`, `href="/account/pages/q3-report"`, "Members only", `action="/account/signout"`, "ana · member",
		`<link rel="stylesheet" href="/account/assets/theme.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the account page lacks %q", want)
		}
	}
	if strings.Contains(page, "bens-page") {
		t.Error("the account page lists another person's page")
	}
	// Every table cell names its column, for the narrow layout.
	if cells, labelled := strings.Count(page, "<td"), strings.Count(page, "<td data-label=")+strings.Count(page, `<td class="row-actions"`); cells != labelled {
		t.Errorf("%d of %d table cells carry a label", labelled, cells)
	}

	css := regexp.MustCompile(`href="(/account/assets/theme\.[^"]+\.css)"`).FindStringSubmatch(page)
	if css == nil {
		t.Fatal("no stylesheet")
	}
	resp, _ := b.get(css[1])
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("stylesheet: status %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if resp, _ = b.get("/account/assets/nothing.css"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown asset: status %d", resp.StatusCode)
	}
	if resp, _ = b.get("/account"); resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/account/" {
		t.Errorf("/account: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ = b.get("/account/nothing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown page: status %d", resp.StatusCode)
	}
}

func TestSessionNeeded(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	b := e.browser()

	// A page load goes to sign in and comes back to where it was going.
	resp, _ := b.get("/account/keys/abcdefgh/revoke")
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), e.mcp.URL+signin.PathSignIn) ||
		loc.Query().Get("host") != signin.HostAccount || loc.Query().Get("path") != "/account/keys/abcdefgh/revoke" {
		t.Fatalf("a page without a session: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Anything else is refused.
	req, _ := http.NewRequest(http.MethodGet, e.apex.URL+"/account/", nil)
	if resp, _ = b.do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a fetch without a session: status %d, want 401", resp.StatusCode)
	}
	for _, path := range []string{"/account/password", "/account/sessions/end", "/account/keys", "/account/keys/abcdefgh/revoke",
		"/account/keys/abcdefgh/rotate", "/account/apps/1/revoke"} {
		if resp, _ = b.post(path, url.Values{"form_token": {"anything"}}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s without a session: status %d, want 401", path, resp.StatusCode)
		}
	}

	// A session that ended is no session.
	b.signIn("ana", password)
	token := b.token()
	e.clock.Advance(signin.DefaultSessionTTL)
	if resp, _ = b.get("/account/"); resp.StatusCode != http.StatusFound {
		t.Errorf("an expired session: status %d, want the redirect to sign in", resp.StatusCode)
	}
	if resp, _ = b.post("/account/keys", url.Values{"form_token": {token}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a form of an expired session: status %d, want 401", resp.StatusCode)
	}
	keys, _ := e.users.ListKeys(context.Background(), "ana")
	if len(keys) != 0 {
		t.Errorf("an expired session issued a key: %+v", keys)
	}
}

func TestFormTokenAndOrigin(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	e.person("eve", "")
	b, evil := e.browser(), e.browser()
	b.signIn("ana", password)
	evil.signIn("eve", password)
	token := b.token()

	posts := map[string]url.Values{
		"/account/keys":         {"label": {"x"}},
		"/account/sessions/end": {},
		"/account/password":     {"current": {password}, "new": {"another long password"}, "confirm": {"another long password"}},
	}
	for path, form := range posts {
		for name, tok := range map[string][]string{"no token": nil, "an empty token": {""}, "another session's token": {evil.token()}} {
			form["form_token"] = tok
			if resp, body := b.post(path, form); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Nothing was changed") {
				t.Errorf("POST %s with %s: status %d, want 403", path, name, resp.StatusCode)
			}
		}
		// With the right token, but from an artifact host or another site.
		form["form_token"] = []string{token}
		for _, origin := range []string{sibling, "https://evil.example", "null"} {
			req, _ := http.NewRequest(http.MethodPost, e.apex.URL+path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "same-site")
			if resp, _ := b.do(req); resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST %s from %s: status %d, want 403", path, origin, resp.StatusCode)
			}
		}
	}
	if keys, _ := e.users.ListKeys(ctx, "ana"); len(keys) != 0 {
		t.Errorf("a refused form issued a key: %+v", keys)
	}
	if _, err := e.users.CheckPassword(ctx, "ana", password); err != nil {
		t.Errorf("a refused form changed the password: %v", err)
	}
	if !evil.signedIn() || !b.signedIn() {
		t.Error("a refused form ended a session")
	}
}

func TestCallback(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	a, other := e.browser(), e.browser()

	resp, _ := a.get("/account/")
	signIn, _ := url.Parse(resp.Header.Get("Location"))
	q := signIn.Query()
	resp, _ = a.postFrom(e.mcp.URL, e.mcp.URL+signin.PathSignIn, url.Values{
		"host": {q.Get("host")}, "path": {q.Get("path")}, "nonce": {q.Get("nonce")}, "login": {"ana"}, "password": {password},
	})
	callback := resp.Header.Get("Location")
	if !strings.Contains(callback, "/account/callback?code=") {
		t.Fatalf("sign-in redirected to %q", callback)
	}
	// The code opens nothing in a browser that did not ask for it.
	resp, body := other.getURL(callback)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "started in another browser") || len(resp.Cookies()) != 0 {
		t.Fatalf("the code in another browser: status %d, cookies %v: %s", resp.StatusCode, resp.Cookies(), body)
	}
	// And it was used up by the attempt.
	if resp, _ = a.getURL(callback); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a code used twice: status %d, want 400", resp.StatusCode)
	}
	if resp, _ = a.get("/account/callback"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the callback without a code: status %d, want 400", resp.StatusCode)
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	e.person("ben", "")
	here, there, bens := e.browser(), e.browser(), e.browser()
	here.signIn("ana", password)
	there.signIn("ana", password)
	bens.signIn("ben", password)
	const next = "another long password"
	form := func(current, new, confirm string) url.Values {
		return url.Values{"form_token": {here.token()}, "current": {current}, "new": {new}, "confirm": {confirm}}
	}

	resp, body := here.post("/account/password", form("not the password", next, next))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "That is not your current password") ||
		!strings.Contains(body, `aria-describedby="current-err"`) {
		t.Fatalf("a wrong current password: status %d: %s", resp.StatusCode, body)
	}
	resp, body = here.post("/account/password", form(password, next, next+"x"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "The two new passwords are not the same") {
		t.Fatalf("mismatched new passwords: status %d: %s", resp.StatusCode, body)
	}
	resp, body = here.post("/account/password", form(password, "short", "short"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Use between 15 and 256 characters") {
		t.Fatalf("a short new password: status %d: %s", resp.StatusCode, body)
	}
	for _, typed := range []string{"not the password", next, "short"} {
		if strings.Contains(body, typed) {
			t.Errorf("the page shows the password %q back", typed)
		}
	}
	if !there.signedIn() {
		t.Fatal("a refused change ended the other session")
	}

	resp, _ = here.post("/account/password", form(password, next, next))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("the change: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if page := here.page(); !strings.Contains(page, "Your password is changed.") || !strings.Contains(page, "Password changed") {
		t.Errorf("the page after the change: %s", page)
	}
	if there.signedIn() {
		t.Error("the other browser's session outlived the change")
	}
	if !bens.signedIn() {
		t.Error("another person's session ended")
	}
	if _, err := e.users.CheckPassword(ctx, "ana", next); err != nil {
		t.Errorf("the new password: %v", err)
	}
	if _, err := e.users.CheckPassword(ctx, "ana", password); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("the old password: %v", err)
	}
}

// Wrong current passwords slow the next attempt, and never lock the form.
func TestChangePasswordBackoff(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	token := b.token()
	const next = "another long password"
	try := func(current string) *http.Response {
		t.Helper()
		resp, _ := b.post("/account/password", url.Values{"form_token": {token}, "current": {current}, "new": {next}, "confirm": {next}})
		return resp
	}
	for i := range 6 {
		if resp := try("a wrong password"); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("failure %d: status %d, want 422", i+1, resp.StatusCode)
		}
	}
	resp := try(password)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("the right password during the wait: status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	e.clock.Advance(time.Second)
	if resp = try(password); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the right password after the wait: status %d", resp.StatusCode)
	}
}

func TestEndOtherSessions(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	here, there := e.browser(), e.browser()
	here.signIn("ana", password)
	there.signIn("ana", password)
	access := here.connect("Notebook")
	resp, _ := here.post("/account/sessions/end", url.Values{"form_token": {here.token()}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if page := here.page(); !strings.Contains(page, "Every other browser is signed out") {
		t.Errorf("the page afterwards: %s", page)
	}
	if there.signedIn() {
		t.Error("the other session is still open")
	}
	if _, err := e.oauth.Authenticate(context.Background(), access); err != nil {
		t.Errorf("the connected app was disconnected: %v", err)
	}
}

// What an admin did to an account shows on it, by name; what the admin did
// to someone else's does not show on theirs.
func TestEventsAboutTheAccount(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", user.RoleAdmin)
	e.person("ben", "")
	asAna := auth.ContextWithPrincipal(context.Background(), auth.Principal{Handle: "ana", Admin: true})
	if _, _, err := e.users.IssueLink(asAna, "ben", user.PurposeReset, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.users.IssueKey(asAna, user.IssueKeyRequest{Handle: "ben", Label: "from-ana"}); err != nil {
		t.Fatal(err)
	}
	if err := e.signin.EndSessions(asAna, "ben", 0); err != nil {
		t.Fatal(err)
	}
	bens, anas := e.browser(), e.browser()
	bens.signIn("ben", password)
	anas.signIn("ana", password)

	page := bens.page()
	for _, want := range []string{"Password reset link made", "(from-ana) issued", `<span class="mono">ana</span>`, ">You<"} {
		if !strings.Contains(page, want) {
			t.Errorf("ben's page lacks %q", want)
		}
	}
	if page := anas.page(); strings.Contains(page, "from-ana") || strings.Contains(page, "reset link made") {
		t.Errorf("ana's page lists what she did to ben's account: %s", page)
	}
}

var newKey = regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`)

// A reload of the page that showed a new key sends its form again; that
// makes no second key.
func TestKeyFormSentTwice(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	form := url.Values{"form_token": {b.token()}, "once": {b.once}, "label": {"ci"}}
	if resp, body := b.post("/account/keys", form); resp.StatusCode != http.StatusOK || !newKey.MatchString(body) {
		t.Fatalf("the first time: status %d", resp.StatusCode)
	}
	resp, body := b.post("/account/keys", form)
	if resp.StatusCode != http.StatusConflict || newKey.MatchString(body) || !strings.Contains(body, "Form <b>already sent</b>") {
		t.Errorf("the second time: status %d: %s", resp.StatusCode, body)
	}
	keys, err := e.users.ListKeys(context.Background(), "ana")
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %+v, %v; want one", keys, err)
	}

	rotate := url.Values{"form_token": {b.token()}, "once": {b.once}, "overlap": {"hour"}}
	path := "/account/keys/" + keys[0].Prefix + "/rotate"
	if resp, _ := b.post(path, rotate); resp.StatusCode != http.StatusOK {
		t.Fatalf("rotating: status %d", resp.StatusCode)
	}
	if resp, _ := b.post(path, rotate); resp.StatusCode != http.StatusConflict {
		t.Errorf("rotating again: status %d, want 409", resp.StatusCode)
	}
	if resp, _ := b.post("/account/keys", url.Values{"form_token": {b.token()}, "once": {"made-up"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a made-up value: status %d, want 403", resp.StatusCode)
	}
}

func TestSignOut(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	if resp, _ := b.post("/account/signout", url.Values{}); resp.StatusCode != http.StatusForbidden || !b.signedIn() {
		t.Fatalf("sign-out without the form token: status %d", resp.StatusCode)
	}
	resp, body := b.post("/account/signout", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "You are signed out") {
		t.Fatalf("sign-out: status %d: %s", resp.StatusCode, body)
	}
	if b.signedIn() {
		t.Error("still signed in on the apex")
	}
	// The whole sign-in ended: the sign-in host asks again.
	resp, _ = b.get("/account/")
	if resp, body = b.getURL(resp.Header.Get("Location")); resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Errorf("the sign-in host after sign-out: status %d", resp.StatusCode)
	}
	if resp, _ = b.post("/account/signout", url.Values{}); resp.StatusCode != http.StatusOK {
		t.Errorf("sign-out when signed out: status %d", resp.StatusCode)
	}
}

func TestKeysFromPasswordSession(t *testing.T) {
	e := newEnv(t, envOptions{maxKeyTTL: 90 * 24 * time.Hour})
	ctx := context.Background()
	e.person("boss", user.RoleAdmin)
	b := e.browser()
	b.signIn("boss", password)

	// An admin's own session issues what a member's does, whatever the
	// form asks for.
	resp, body := b.post("/account/keys", url.Values{"form_token": {b.token()}, "label": {"weekly-report"}, "slugs": {"weekly-*, q3-report"},
		"days": {"30"}, "scopes": {"admin"}, "handle": {"someone-else"}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" || len(resp.Cookies()) != 0 {
		t.Fatalf("issuing a key: status %d, Cache-Control %q, cookies %v: %s", resp.StatusCode, resp.Header.Get("Cache-Control"), resp.Cookies(), body)
	}
	secret := regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`).FindString(body)
	if secret == "" {
		t.Fatalf("the page shows no key: %s", body)
	}
	for _, want := range []string{"it cannot be shown again", `data-copy-target="secret"`, "Authorization: Bearer " + secret,
		// The command line takes the API's origin and adds the version itself.
		"kazari login --url https://api.art.test<", "export KAZARI_URL=https://api.art.test\nexport KAZARI_TOKEN=" + secret + "<",
		e.mcp.URL + "/", "weekly-* q3-report"} {
		if !strings.Contains(body, want) {
			t.Errorf("the new-key page lacks %q", want)
		}
	}
	p, err := e.users.Authenticate(ctx, secret)
	if err != nil || p.Handle != "boss" || p.Admin || p.HasScope(auth.ScopeAdmin) || !slices.Equal(p.Scopes, auth.MemberScopes) ||
		!slices.Equal(p.Slugs, []string{"weekly-*", "q3-report"}) || p.ExpiresAt == nil || !p.ExpiresAt.Equal(t0.Add(30*24*time.Hour)) {
		t.Fatalf("the key's principal = %+v, %v; want boss with the member scopes", p, err)
	}
	keys, _ := e.users.ListKeys(ctx, "boss")
	if len(keys) != 1 || keys[0].IssuedBy != "" || keys[0].Label != "weekly-report" {
		t.Fatalf("keys = %+v", keys)
	}
	// The secret is on no later page.
	page := b.page()
	if strings.Contains(page, secret) || !strings.Contains(page, "kzk_"+keys[0].Prefix) || !strings.Contains(page, "weekly-report") {
		t.Errorf("the account page after issuing: %s", page)
	}
	events, err := e.audit.List(ctx, audit.Query{Actions: []string{audit.KeyIssue}})
	if err != nil || len(events.Events) != 1 || events.Events[0].Actor != "boss" {
		t.Errorf("audit = %+v, %v; want the key issued by boss", events.Events, err)
	}

	// Without an expiry the key takes the instance's limit.
	_, body = b.post("/account/keys", url.Values{"form_token": {b.token()}})
	plain := regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`).FindString(body)
	if p, err = e.users.Authenticate(ctx, plain); err != nil || p.ExpiresAt == nil || !p.ExpiresAt.Equal(t0.Add(90*24*time.Hour)) {
		t.Errorf("a key without an expiry = %+v, %v; want 90 days", p, err)
	}

	for name, tt := range map[string]struct {
		form url.Values
		want string
	}{
		"days over the limit": {url.Values{"days": {"91"}}, "Enter a whole number of days, 90 at most"},
		"days not a number":   {url.Values{"days": {"soon"}}, "Enter a whole number of days"},
		"a bad slug pattern":  {url.Values{"slugs": {"Not A Slug"}, "label": {"kept"}}, "Give the first part of a page's address"},
		"a pattern for all":   {url.Values{"slugs": {"*"}}, "Give the first part of a page's address"},
	} {
		tt.form.Set("form_token", b.token())
		resp, body = b.post("/account/keys", tt.form)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tt.want) {
			t.Errorf("%s: status %d, want 422 with %q", name, resp.StatusCode, tt.want)
		}
		if label := tt.form.Get("label"); label != "" && !strings.Contains(body, `value="`+label+`"`) {
			t.Errorf("%s: the label was not kept", name)
		}
	}
	if keys, _ = e.users.ListKeys(ctx, "boss"); len(keys) != 2 {
		t.Errorf("refused forms issued keys: %d keys", len(keys))
	}
}

// A session opened with an API key issues nothing wider than that key.
func TestKeysFromKeySession(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	narrow, narrowSecret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Label: "reader", Scopes: []string{auth.ScopeRead}, ExpiresIn: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	wide, _, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Label: "everything"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("", narrowSecret)
	page := b.page()
	if !strings.Contains(page, "Signed in with the API key") || !strings.Contains(page, narrow.Prefix) {
		t.Errorf("the page does not say how the session was opened: %s", page)
	}

	resp, body := b.post("/account/keys", url.Values{"form_token": {b.token()}, "label": {"child"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issuing from a key session: status %d: %s", resp.StatusCode, body)
	}
	child, err := e.users.Authenticate(ctx, regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`).FindString(body))
	if err != nil || !slices.Equal(child.Scopes, []string{auth.ScopeRead}) || child.ExpiresAt == nil || !child.ExpiresAt.Equal(*narrow.ExpiresAt) {
		t.Fatalf("the child's principal = %+v, %v; want the key's one scope and its expiry", child, err)
	}
	keys, _ := e.users.ListKeys(ctx, "ana")
	if keys[0].IssuedBy != narrow.Prefix {
		t.Errorf("the child was issued with %q, want %q", keys[0].IssuedBy, narrow.Prefix)
	}

	// A lifetime the key does not have is refused, in plain words.
	resp, body = b.post("/account/keys", url.Values{"form_token": {b.token()}, "days": {"30"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "That key cannot be made from this session") {
		t.Errorf("outliving the session's key: status %d: %s", resp.StatusCode, body)
	}
	// So is rotating a key that holds more than the session's.
	resp, body = b.post("/account/keys/"+wide.Prefix+"/rotate", url.Values{"form_token": {b.token()}, "overlap": {"now"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "This key cannot be rotated here") {
		t.Errorf("rotating a wider key: status %d: %s", resp.StatusCode, body)
	}
	if k, _ := e.users.KeyStatus(ctx, wide.Prefix); !k.Active {
		t.Error("the refused rotation ended the key")
	}
	// And revoking one.
	resp, body = b.post("/account/keys/"+wide.Prefix+"/revoke", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "This key cannot be revoked here") {
		t.Errorf("revoking a wider key: status %d: %s", resp.StatusCode, body)
	}
	if k, _ := e.users.KeyStatus(ctx, wide.Prefix); !k.Active {
		t.Error("the refused revocation ended the key")
	}

	// Revoking the key the session was opened with signs the browser out.
	resp, body = b.get("/account/keys/" + narrow.Prefix + "/revoke")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "you signed in with this key") || !strings.Contains(body, "kzk_"+keys[0].Prefix) {
		t.Fatalf("the confirmation: status %d: %s", resp.StatusCode, body)
	}
	resp, body = b.post("/account/keys/"+narrow.Prefix+"/revoke", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "this browser is signed out too") {
		t.Fatalf("revoking the session's key: status %d: %s", resp.StatusCode, body)
	}
	if b.signedIn() {
		t.Error("the session outlived its key")
	}
	if _, err := e.users.Authenticate(ctx, narrowSecret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the revoked key: %v", err)
	}
	if k, _ := e.users.KeyStatus(ctx, keys[0].Prefix); k.Active {
		t.Error("the key issued with the revoked key is still active")
	}
}

func TestRotateAndRevokeKey(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("boss", user.RoleAdmin)
	b := e.browser()
	b.signIn("boss", password)
	k, oldSecret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Label: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Label: "ops", Scopes: auth.AllScopes})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := b.get("/account/keys/" + k.Prefix + "/rotate")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="day" checked`) {
		t.Fatalf("the rotate page: status %d: %s", resp.StatusCode, body)
	}
	resp, body = b.post("/account/keys/"+k.Prefix+"/rotate", url.Values{"form_token": {b.token()}, "overlap": {"hour"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "The key <b>is rotated</b>") || !strings.Contains(body, "works until") {
		t.Fatalf("rotating: status %d: %s", resp.StatusCode, body)
	}
	next := regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`).FindString(body)
	if _, err := e.users.Authenticate(ctx, next); err != nil {
		t.Fatalf("the new key: %v", err)
	}
	if _, err := e.users.Authenticate(ctx, oldSecret); err != nil {
		t.Errorf("the old key during the overlap: %v", err)
	}
	e.clock.Advance(time.Hour)
	if _, err := e.users.Authenticate(ctx, oldSecret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the old key after the overlap: %v", err)
	}
	// What has ended is not rotated again.
	resp, body = b.post("/account/keys/"+k.Prefix+"/rotate", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "This key has ended") {
		t.Errorf("rotating an expired key: status %d: %s", resp.StatusCode, body)
	}

	// A key that holds admin is rotated on the admin API alone, and the
	// page offers no link for it; revoking it is the person's to do.
	page := b.page()
	if strings.Contains(page, `href="/account/keys/`+adminKey.Prefix+`/rotate"`) || !strings.Contains(page, `href="/account/keys/`+adminKey.Prefix+`/revoke"`) {
		t.Errorf("the links of an admin key: %s", page)
	}
	resp, body = b.post("/account/keys/"+adminKey.Prefix+"/rotate", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "rotated on the admin API") {
		t.Errorf("rotating an admin key: status %d: %s", resp.StatusCode, body)
	}
	resp, body = b.get("/account/keys/" + adminKey.Prefix + "/revoke")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Every key issued with it and every app approved with one of them ends too") {
		t.Fatalf("the revoke confirmation: status %d: %s", resp.StatusCode, body)
	}
	if st, _ := e.users.KeyStatus(ctx, adminKey.Prefix); !st.Active {
		t.Fatal("opening the confirmation revoked the key")
	}
	resp, _ = b.post("/account/keys/"+adminKey.Prefix+"/revoke", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("revoking: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if st, _ := e.users.KeyStatus(ctx, adminKey.Prefix); st.Active {
		t.Error("the key is still active")
	}
	page = b.page()
	if !strings.Contains(page, "The key is revoked") || !strings.Contains(page, "Revoked and expired keys") {
		t.Errorf("the page after revoking: %s", page)
	}
}

// Another person's key is not found, never forbidden: the page says nothing
// about whose it is.
func TestAnotherPersonsKey(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	e.person("ben", "")
	bens, bensSecret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ben", Label: "bens"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("ana", password)
	for _, prefix := range []string{bens.Prefix, "nosuchky", "../../x"} {
		for _, action := range []string{"rotate", "revoke"} {
			path := "/account/keys/" + url.PathEscape(prefix) + "/" + action
			if resp, body := b.get(path); resp.StatusCode != http.StatusNotFound || strings.Contains(body, "bens") {
				t.Errorf("GET %s: status %d, want 404", path, resp.StatusCode)
			}
			if resp, _ := b.post(path, url.Values{"form_token": {b.token()}}); resp.StatusCode != http.StatusNotFound {
				t.Errorf("POST %s: status %d, want 404", path, resp.StatusCode)
			}
		}
	}
	if _, err := e.users.Authenticate(ctx, bensSecret); err != nil {
		t.Errorf("ben's key after ana's attempts: %v", err)
	}
	if page := b.page(); strings.Contains(page, bens.Prefix) {
		t.Error("ana's page lists ben's key")
	}
}

func TestKeyCap(t *testing.T) {
	e := newEnv(t, envOptions{maxKeys: 2})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	for i := range 2 {
		if resp, _ := b.post("/account/keys", url.Values{"form_token": {b.token()}}); resp.StatusCode != http.StatusOK {
			t.Fatalf("key %d: status %d", i+1, resp.StatusCode)
		}
	}
	resp, body := b.post("/account/keys", url.Values{"form_token": {b.token()}, "label": {"one too many"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "You hold as many live keys as a person may") ||
		!strings.Contains(body, "Revoke one you no longer use") {
		t.Fatalf("a key past the cap: status %d: %s", resp.StatusCode, body)
	}
	keys, _ := e.users.ListKeys(context.Background(), "ana")
	// A rotation is room for one key more while the old one runs out; a
	// second on top of it is not.
	resp, body = b.post("/account/keys/"+keys[0].Prefix+"/rotate", url.Values{"form_token": {b.token()}, "overlap": {"day"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "works until") {
		t.Fatalf("rotating with an overlap at the cap: status %d: %s", resp.StatusCode, body)
	}
	resp, body = b.post("/account/keys/"+keys[1].Prefix+"/rotate", url.Values{"form_token": {b.token()}, "overlap": {"day"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "A key you rotated counts until it ends") {
		t.Errorf("a second rotation with an overlap at the cap: status %d: %s", resp.StatusCode, body)
	}
}

func TestConnectedApps(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	e.person("ben", "")
	a, b := e.browser(), e.browser()
	a.signIn("ana", password)
	b.signIn("ben", password)
	anas := a.connect("Notebook")
	bens := b.connect("Bens agent")

	grants, err := e.oauth.ListGrants(ctx, "ana")
	if err != nil || len(grants) != 1 {
		t.Fatalf("ana's grants = %+v, %v", grants, err)
	}
	bensGrants, _ := e.oauth.ListGrants(ctx, "ben")
	mine, theirs := strconv.FormatInt(grants[0].FamilyID, 10), strconv.FormatInt(bensGrants[0].FamilyID, 10)

	page := a.page()
	if !strings.Contains(page, "Notebook") || strings.Contains(page, "Bens agent") || !strings.Contains(page, `href="/account/apps/`+mine+`/revoke"`) ||
		!strings.Contains(page, "</span> connected") {
		t.Errorf("ana's page: %s", page)
	}
	// Only one's own app is there to disconnect.
	for _, id := range []string{theirs, "0", "-1", "999", "x", mine + "x"} {
		if resp, _ := a.get("/account/apps/" + id + "/revoke"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET the confirmation of grant %s: status %d, want 404", id, resp.StatusCode)
		}
		if resp, _ := a.post("/account/apps/"+id+"/revoke", url.Values{"form_token": {a.token()}}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST the revocation of grant %s: status %d, want 404", id, resp.StatusCode)
		}
	}
	if _, err := e.oauth.Authenticate(ctx, bens); err != nil {
		t.Fatalf("ben's app after ana's attempts: %v", err)
	}

	resp, body := a.get("/account/apps/" + mine + "/revoke")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Disconnect <b>Notebook</b>") {
		t.Fatalf("the confirmation: status %d: %s", resp.StatusCode, body)
	}
	if _, err := e.oauth.Authenticate(ctx, anas); err != nil {
		t.Fatalf("opening the confirmation disconnected the app: %v", err)
	}
	resp, _ = a.post("/account/apps/"+mine+"/revoke", url.Values{"form_token": {a.token()}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disconnecting: status %d", resp.StatusCode)
	}
	if _, err := e.oauth.Authenticate(ctx, anas); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the app after being disconnected: %v", err)
	}
	if page = a.page(); !strings.Contains(page, "The app is disconnected.") || !strings.Contains(page, "No app is connected") {
		t.Errorf("the page afterwards: %s", page)
	}
	events, _ := e.audit.List(ctx, audit.Query{Actions: []string{oauth.ActionRevoke}})
	if len(events.Events) != 1 || events.Events[0].Actor != "ana" {
		t.Errorf("revoke audit = %+v, want ana as the actor", events.Events)
	}
}

// Names that people and applications choose are text, wherever they show.
func TestHostileNames(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	hostile := []string{`<script>alert(1)</script>`, `"><img src=x onerror=alert(1)>`}
	if _, _, _, err := e.users.Invite(asAdmin, user.InviteRequest{Handle: "ana", DisplayName: hostile[0], Email: "ana@example.org"}); err != nil {
		t.Fatal(err)
	}
	_, token, err := e.users.IssueLink(asAdmin, "ana", user.PurposeInvite, false)
	if err != nil {
		t.Fatal(err)
	}
	e.page("ana", "page", hostile[1], artifact.Public)
	b := e.browser()
	var bodies []string
	collect := func(resp *http.Response, body string) {
		t.Helper()
		// 422 is the form shown again, with what was typed.
		if resp.StatusCode >= 400 && resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s: status %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body)
		}
		bodies = append(bodies, body)
	}
	invited := e.browser()
	collect(invited.openLink("invite", token))
	collect(invited.post("/account/invite", url.Values{"password": {password}, "confirm": {password}}))
	collect(invited.get("/account/"))
	b.signIn("ana", password)
	for _, name := range hostile {
		b.connect(name)
		collect(b.post("/account/keys", url.Values{"form_token": {b.token()}, "label": {name}, "slugs": {name}}))
		collect(b.post("/account/keys", url.Values{"form_token": {b.token()}, "label": {name}}))
	}
	collect(b.get("/account/"))
	keys, _ := e.users.ListKeys(ctx, "ana")
	grants, _ := e.oauth.ListGrants(ctx, "ana")
	if len(keys) != 2 || len(grants) != 2 {
		t.Fatalf("%d keys and %d grants, want two of each", len(keys), len(grants))
	}
	for _, k := range keys {
		collect(b.get("/account/keys/" + k.Prefix + "/revoke"))
		collect(b.get("/account/keys/" + k.Prefix + "/rotate"))
	}
	for _, g := range grants {
		collect(b.get("/account/apps/" + strconv.FormatInt(g.FamilyID, 10) + "/revoke"))
	}
	collect(b.post("/account/keys/"+keys[0].Prefix+"/rotate", url.Values{"form_token": {b.token()}}))
	shown := 0
	for _, body := range bodies {
		for _, raw := range []string{"<script>alert", "<img src=x", `"><img`, "onerror=alert(1)>"} {
			if strings.Contains(body, raw) {
				t.Errorf("a page holds %q unescaped", raw)
			}
		}
		shown += strings.Count(body, "&lt;script&gt;alert(1)&lt;/script&gt;") + strings.Count(body, "&lt;img src=x onerror=alert(1)&gt;")
	}
	if shown < 10 {
		t.Errorf("the names were shown %d times; the pages under test do not show them", shown)
	}
}

// Without built-in accounts the page is for keys and apps alone.
func TestWithoutLocalAccounts(t *testing.T) {
	e := newEnv(t, envOptions{noLocal: true})
	ctx := context.Background()
	if _, err := e.users.CreateUser(ctx, user.CreateUserRequest{Handle: "ana"}); err != nil {
		t.Fatal(err)
	}
	_, secret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("", secret)
	page := b.page()
	if strings.Contains(page, `action="/account/password"`) || strings.Contains(page, "change your password") || !strings.Contains(page, "API keys") {
		t.Errorf("the page without built-in accounts: %s", page)
	}
	if resp, _ := b.post("/account/password", url.Values{"form_token": {b.token()}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /account/password: status %d, want 404", resp.StatusCode)
	}

	// A person who signs in at the provider or with a key has browser
	// sessions too, and ends the others here like anyone else.
	if !strings.Contains(page, `action="/account/sessions/end"`) || !strings.Contains(page, "Sign out of my other sessions") {
		t.Fatalf("the page without built-in accounts has no way to end the other sessions: %s", page)
	}
	other := e.browser()
	other.signIn("", secret)
	resp, _ := b.post("/account/sessions/end", url.Values{"form_token": {b.token()}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("POST /account/sessions/end: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if other.signedIn() || !b.signedIn() {
		t.Errorf("after ending the other sessions: the other browser signed in %v, this one %v", other.signedIn(), b.signedIn())
	}
}

// A key rotated "at once" has ended by the time its page is shown, and the
// page does not say it works until a time already past.
func TestRotateAtOnce(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	k, oldSecret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := b.post("/account/keys/"+k.Prefix+"/rotate", url.Values{"form_token": {b.token()}, "overlap": {"now"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<span class="mono">kzk_`+k.Prefix+`</span> has stopped working`) ||
		strings.Contains(body, "works until") {
		t.Errorf("rotating at once: status %d: %s", resp.StatusCode, body)
	}
	if _, err := e.users.Authenticate(ctx, oldSecret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the old key after a rotation at once: %v", err)
	}
}

// The page has one thing it is for at a time, and one button dressed as it.
func TestAccountPageHasOnePrimaryButton(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	page := b.page()
	if n := strings.Count(page, "btn-primary"); n != 1 {
		t.Errorf("the account page has %d primary buttons, want 1", n)
	}
	// Ending the other sessions is not part of changing the password.
	sessions, pw := strings.Index(page, `action="/account/sessions/end"`), strings.Index(page, `id="password-h"`)
	if sessions < 0 || pw < 0 || sessions > pw {
		t.Errorf("the sessions button stands inside the password section, or is missing: at %d, password at %d", sessions, pw)
	}
}

// A key that no key issued says so in the words the dashboard uses.
func TestKeyWithoutAnIssuingKey(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	if _, _, err := e.users.IssueKey(asAdmin, user.IssueKeyRequest{Handle: "ana"}); err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("ana", password)
	if page := b.page(); !strings.Contains(page, `<td data-label="Issued with"><span class="soft">A sign-in or an admin</span></td>`) {
		t.Errorf("a key an admin issued: %s", page)
	}
}

// The longest lifetime a form accepts is the dashboard's: ten years.
func TestKeyLifetimeLimit(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	b := e.browser()
	b.signIn("ana", password)
	resp, body := b.post("/account/keys", url.Values{"form_token": {b.token()}, "days": {"3651"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `id="days-err"`) {
		t.Errorf("a key for 3651 days: status %d: %s", resp.StatusCode, body)
	}
	if resp, body = b.post("/account/keys", url.Values{"form_token": {b.token()}, "days": {"3650"}}); resp.StatusCode != http.StatusOK {
		t.Errorf("a key for 3650 days: status %d: %s", resp.StatusCode, body)
	}
}

// failingGrants cannot look a client up.
type failingGrants struct {
	account.Grants
	err error
}

func (f failingGrants) GetClient(context.Context, string) (*oauth.Client, error) { return nil, f.err }

// A client that is gone leaves its id to show; a lookup that failed is not
// a reason to show an app under another name.
func TestAppsWhenTheClientLookupFails(t *testing.T) {
	for name, tt := range map[string]struct {
		err    error
		status int
	}{
		"the client is gone": {oauth.ErrNotFound, http.StatusOK},
		"the lookup failed":  {errors.New("database is locked"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOptions{grants: func(g account.Grants) account.Grants { return failingGrants{g, tt.err} }})
			e.person("ana", "")
			b := e.browser()
			b.signIn("ana", password)
			b.connect("Notebook")
			grants, err := e.oauth.ListGrants(context.Background(), "ana")
			if err != nil || len(grants) != 1 {
				t.Fatalf("grants = %+v, %v", grants, err)
			}
			resp, body := b.get("/account/")
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tt.status, body)
			}
			if listed := strings.Contains(body, `<span class="mono">`+grants[0].ClientID+`</span>`); listed != (tt.status == http.StatusOK) {
				t.Errorf("the app listed by its id: %v", listed)
			}
			if strings.Contains(body, "database is locked") {
				t.Error("the page shows the error")
			}
		})
	}
}

// A session opened with a key that only administers has nothing to pass on
// to a key made here. That is the person's to read, not a fault.
func TestKeyFromASessionWithNothingToPassOn(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("boss", user.RoleAdmin)
	_, secret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "boss", Scopes: []string{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("", secret)
	resp, body := b.post("/account/keys", url.Values{"form_token": {b.token()}, "label": {"kept"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "That key cannot be made here") ||
		!strings.Contains(body, `value="kept"`) || strings.Contains(body, "Something went wrong") || strings.Contains(body, "user:") {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
	if keys, _ := e.users.ListKeys(ctx, "boss"); len(keys) != 1 {
		t.Errorf("the refused form issued a key: %d keys", len(keys))
	}
}
