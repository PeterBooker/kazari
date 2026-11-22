package account_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestInviteLink(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	if _, _, _, err := e.users.Invite(asAdmin, user.InviteRequest{Handle: "boss", Role: user.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	_, _, token, err := e.users.Invite(asAdmin, user.InviteRequest{Handle: "ana", DisplayName: "Ana Sousa", Email: "ana@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()

	// Opening the link, as a chat application's preview does and then the
	// person, consumes nothing and leaves the token in no address.
	for range 3 {
		resp, _ := b.get("/account/invite?token=" + token)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/invite" {
			t.Fatalf("link with its token: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		var link *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "kazari_link" {
				link = c
			}
		}
		if link == nil || link.Value != token || link.MaxAge != 15*60 || !link.HttpOnly {
			t.Fatalf("link cookie = %+v", link)
		}
	}
	for range 2 {
		resp, body := b.get("/account/invite")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the form: status %d: %s", resp.StatusCode, body)
		}
		for _, want := range []string{"Ana Sousa", `value="ana"`, "ana@example.org", `autocomplete="new-password"`, `minlength="15"`,
			"At least 15 characters", "Length is the only rule", `action="/account/invite"`} {
			if !strings.Contains(body, want) {
				t.Errorf("the form lacks %q", want)
			}
		}
		if strings.Contains(body, token) {
			t.Error("the form holds the token")
		}
	}
	if _, _, err := e.users.Link(ctx, token); err != nil {
		t.Fatalf("the link after being opened five times: %v", err)
	}

	const typed = "a password typed with care"
	resp, body := b.post("/account/invite", url.Values{"password": {typed}, "confirm": {typed + "x"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "The two passwords are not the same") ||
		!strings.Contains(body, `aria-describedby="confirm-err"`) {
		t.Fatalf("mismatched passwords: status %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, typed) {
		t.Error("a password was shown back")
	}
	resp, body = b.post("/account/invite", url.Values{"password": {"too short"}, "confirm": {"too short"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Use between 15 and 256 characters") ||
		!strings.Contains(body, `aria-invalid="true"`) || strings.Contains(body, "too short") {
		t.Fatalf("a short password: status %d: %s", resp.StatusCode, body)
	}
	if _, _, err := e.users.Link(ctx, token); err != nil {
		t.Fatalf("the link after two refused passwords: %v", err)
	}

	resp, body = b.post("/account/invite", url.Values{"password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("setting the password: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		cleared = cleared || (c.Name == "kazari_link" && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("the link cookie outlived the link")
	}
	if _, _, err := e.users.Link(ctx, token); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("the link after use: err = %v, want ErrLinkNotFound", err)
	}
	// The link and the new password show who it is: the browser is signed
	// in, and the account page welcomes them with the next step.
	page := b.page()
	for _, want := range []string{"Welcome, <b>Ana Sousa</b>", "Your password is set and you are signed in.", "Connect your AI agent", e.mcp.URL + "/"} {
		if !strings.Contains(page, want) {
			t.Errorf("account page after the invite lacks %q", want)
		}
	}
	if strings.Index(page, "Your pages") > strings.Index(page, "Recovery codes") {
		t.Error("the account page puts the security sections before the pages")
	}
	if page := b.page(); strings.Contains(page, "Welcome,") {
		t.Error("the welcome was shown twice")
	}
	ev, err := e.audit.List(ctx, audit.Query{Actions: []string{oauth.ActionSignIn}})
	if err != nil || len(ev.Events) != 1 || ev.Events[0].Actor != "ana" || ev.Events[0].Details["via"] != oauth.ViaLink {
		t.Errorf("sign-in events = %+v, %v; want one of ana, via the link", ev.Events, err)
	}

	// The used link opens nothing, here or in another browser.
	for _, other := range []*browser{b, e.browser()} {
		resp, body = other.openLink("invite", token)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "no longer works") {
			t.Errorf("a used link: status %d: %s", resp.StatusCode, body)
		}
	}
}

func TestResetLink(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	here, there := e.browser(), e.browser()
	here.signIn("ana", password)
	there.signIn("ana", password)

	_, token, err := e.users.IssueLink(asAdmin, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	fresh := e.browser()
	resp, body := fresh.openLink("reset", token)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Choose a <b>new password</b>") || !strings.Contains(body, "New password") {
		t.Fatalf("the reset form: status %d: %s", resp.StatusCode, body)
	}
	const next = "another long password"
	resp, _ = fresh.post("/account/reset", url.Values{"password": {next}, "confirm": {next}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("reset: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A used reset link ends every other session, and signs in this one.
	if here.signedIn() || there.signedIn() {
		t.Error("a session outlived the reset")
	}
	if _, err := e.users.CheckPassword(context.Background(), "ana", password); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("the old password after the reset: %v", err)
	}
	if !fresh.signedIn() {
		t.Error("the browser that used the reset link is not signed in")
	}
}

func TestSetupLink(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	_, token, err := e.users.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	resp, body := b.openLink("setup", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the setup form: status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"Create the first admin account", `name="handle"`, `name="name"`, `name="email"`, `name="password"`, `name="confirm"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the setup form lacks %q", want)
		}
	}
	// Every mistake is named at once, and what was right is kept.
	form := url.Values{"handle": {"Peter B"}, "name": {"Peter B"}, "email": {"peter@example.org"}, "password": {password}, "confirm": {password + "x"}}
	resp, body = b.post("/account/setup", form)
	for _, want := range []string{"That handle cannot be used", `<span class="mono">peter-b</span> would do`, "The two passwords are not the same",
		`value="Peter B"`, `value="peter@example.org"`, "Correct the marked fields"} {
		if !strings.Contains(body, want) {
			t.Errorf("an invalid handle and a mismatch: the form lacks %q", want)
		}
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, password) {
		t.Fatalf("an invalid handle and a mismatch: status %d, a password shown back: %t", resp.StatusCode, strings.Contains(body, password))
	}
	form.Set("confirm", password)
	resp, body = b.post("/account/setup", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Count(body, `value="`+password+`"`) != 2 {
		t.Fatalf("an invalid handle alone keeps both passwords: status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `pattern="[a-z0-9][a-z0-9_\-]*"`) {
		t.Error("the handle field has no pattern")
	}
	form.Set("handle", "peter")
	resp, body = b.post("/account/setup", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("setup: status %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	u, err := e.users.GetUser(ctx, "peter")
	if err != nil || u.Role != user.RoleAdmin || u.DisplayName != "Peter B" || u.Email != "peter@example.org" {
		t.Fatalf("the first user = %+v, %v", u, err)
	}
	if page := b.page(); !strings.Contains(page, `<a href="`+adminURL+`/">the dashboard</a>`) {
		t.Errorf("the first admin's welcome does not lead to the dashboard: %s", page)
	}

	// Once any user exists the page is gone, whatever the link.
	for _, other := range []*browser{b, e.browser()} {
		resp, body = other.openLink("setup", token)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("setup once a user exists: status %d: %s", resp.StatusCode, body)
		}
	}
	resp, _ = e.browser().post("/account/setup", form)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("setup posted once a user exists: status %d", resp.StatusCode)
	}
}

// A setup link made before the first user and opened after is a plain 404,
// and without a dashboard the first admin lands on the account page.
func TestSetupLinkRaces(t *testing.T) {
	e := newEnv(t, envOptions{noAdmin: true})
	ctx := context.Background()
	_, token, err := e.users.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, second := e.browser(), e.browser()
	for _, b := range []*browser{first, second} {
		if resp, body := b.openLink("setup", token); resp.StatusCode != http.StatusOK {
			t.Fatalf("the setup form: status %d: %s", resp.StatusCode, body)
		}
	}
	resp, _ := first.post("/account/setup", url.Values{"handle": {"one"}, "password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/" {
		t.Fatalf("setup without a dashboard: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := second.post("/account/setup", url.Values{"handle": {"two"}, "password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the second claim: status %d: %s", resp.StatusCode, body)
	}
	if _, err := e.users.GetUser(ctx, "two"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("the second claim made a user: %v", err)
	}
}

func TestDeadLinks(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("boss", user.RoleAdmin)
	dead := func(what, purpose, token string) {
		t.Helper()
		b := e.browser()
		resp, body := b.openLink(purpose, token)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "no longer works") {
			t.Errorf("%s: status %d: %s", what, resp.StatusCode, body)
		}
		resp, _ = b.post("/account/"+purpose, url.Values{"password": {password}, "confirm": {password}})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s, posted: status %d", what, resp.StatusCode)
		}
	}

	// Each case fails once per request above; the wait they earn is not
	// what is tested here.
	rest := func() { e.clock.Advance(time.Hour) }

	invite := e.invite("ana", "")
	dead("an invite opened as a reset", "reset", invite)
	rest()
	if _, _, err := e.users.Link(context.Background(), invite); err != nil {
		t.Fatalf("the invite after being opened at the wrong address: %v", err)
	}

	replaced := e.invite("ben", "")
	if _, _, err := e.users.IssueLink(asAdmin, "ben", user.PurposeInvite, false); err != nil {
		t.Fatal(err)
	}
	dead("a replaced link", "invite", replaced)
	rest()

	e.person("cy", "")
	_, reset, err := e.users.IssueLink(asAdmin, "cy", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.UpdateUser(asAdmin, "cy", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	dead("a disabled person's link", "reset", reset)
	rest()

	expiring := e.invite("dee", "")
	e.clock.Advance(7 * 24 * time.Hour)
	dead("an expired link", "invite", expiring)
	rest()

	dead("an unknown token", "invite", user.LinkPrefix+strings.Repeat("a", 52))
	rest()

	// A token of the wrong shape is not even kept.
	b := e.browser()
	resp, body := b.get("/account/invite?token=kzl_short")
	if resp.StatusCode != http.StatusNotFound || len(resp.Cookies()) != 0 || !strings.Contains(body, "no longer works") {
		t.Errorf("a malformed token: status %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	resp, _ = b.get("/account/invite")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the form without a link: status %d", resp.StatusCode)
	}
}

func TestLinkBackoff(t *testing.T) {
	e := newEnv(t, envOptions{})
	valid := e.invite("ana", "")
	guess := user.LinkPrefix + strings.Repeat("b", 52)
	b := e.browser()
	// Five wrong tokens cost nothing; the sixth earns a second.
	for i := range 6 {
		if resp, _ := b.openLink("invite", guess); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("guess %d: status %d, want 404", i+1, resp.StatusCode)
		}
	}
	good := e.browser()
	resp, body := good.openLink("invite", valid)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" || !strings.Contains(body, "Wait a second") {
		t.Fatalf("a link during the wait: status %d, Retry-After %q: %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	resp, _ = good.post("/account/invite", url.Values{"password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a post during the wait: status %d, want 429", resp.StatusCode)
	}
	if _, _, err := e.users.Link(context.Background(), valid); err != nil {
		t.Fatalf("the refused post used the link: %v", err)
	}
	// Being refused does not extend the wait, and the wait doubles.
	e.clock.Advance(time.Second)
	if resp, _ = b.openLink("invite", guess); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a guess after the wait: status %d, want 404", resp.StatusCode)
	}
	e.clock.Advance(2*time.Second - time.Millisecond)
	if resp, _ = good.get("/account/invite"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("just before two seconds have passed: status %d, want 429", resp.StatusCode)
	}
	e.clock.Advance(time.Millisecond)
	if resp, body = good.get("/account/invite"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the link after the wait: status %d: %s", resp.StatusCode, body)
	}
	resp, _ = good.post("/account/invite", url.Values{"password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the post after the wait: status %d", resp.StatusCode)
	}
}

// Script on an artifact host is same-site, and must not be able to post a
// form here.
func TestLinkFormFromSiblingOrigin(t *testing.T) {
	e := newEnv(t, envOptions{})
	token := e.invite("ana", "")
	b := e.browser()
	if resp, _ := b.openLink("invite", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("the form: status %d", resp.StatusCode)
	}
	resp, _ := b.postFrom(sibling, e.apex.URL+"/account/invite", url.Values{"password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a post from a sibling origin: status %d, want 403", resp.StatusCode)
	}
	if _, _, err := e.users.Link(context.Background(), token); err != nil {
		t.Errorf("the refused post used the link: %v", err)
	}
}

func TestLinkPagesOffWithoutLocalAccounts(t *testing.T) {
	e := newEnv(t, envOptions{noLocal: true})
	b := e.browser()
	for _, purpose := range []string{"invite", "reset", "setup"} {
		resp, _ := b.get("/account/" + purpose + "?token=" + user.LinkPrefix + strings.Repeat("a", 52))
		if resp.StatusCode != http.StatusNotFound || len(resp.Cookies()) != 0 {
			t.Errorf("%s: status %d, cookies %v; want a plain 404", purpose, resp.StatusCode, resp.Cookies())
		}
		if resp, _ = b.post("/account/"+purpose, url.Values{"password": {password}, "confirm": {password}}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s posted: status %d, want 404", purpose, resp.StatusCode)
		}
	}
}

// A link's form is accepted only from the page that link opened: the
// cookie alone goes with any request to this host. The check comes before
// the link is looked up, so a form without the value uses nothing up and
// earns no wait.
func TestLinkFormCarriesItsToken(t *testing.T) {
	e := newEnv(t, envOptions{})
	token := e.invite("ana", "")
	b := e.browser()
	resp, body := b.openLink("invite", token)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<input type="hidden" name="form_token" value="`+account.LinkFormToken(token)+`">`) {
		t.Fatalf("the form: status %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, token) {
		t.Error("the form holds the link's token itself")
	}
	other := account.LinkFormToken(user.LinkPrefix + strings.Repeat("a", 52))
	for name, form := range map[string]url.Values{
		"no value":             {"password": {password}, "confirm": {password}, "form_token": nil},
		"an empty value":       {"password": {password}, "confirm": {password}, "form_token": {""}},
		"another link's value": {"password": {password}, "confirm": {password}, "form_token": {other}},
		"the link's own token": {"password": {password}, "confirm": {password}, "form_token": {token}},
	} {
		// More often than failures are free: a refused form is not one.
		for range 7 {
			resp, body := b.post("/account/invite", form)
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "not sent from the page your link opened") {
				t.Fatalf("%s: status %d: %s", name, resp.StatusCode, body)
			}
		}
	}
	if _, u, err := e.users.Link(context.Background(), token); err != nil || u.Handle != "ana" {
		t.Fatalf("the refused forms used the link: %v", err)
	}
	if acc, err := e.users.Account(context.Background(), "ana"); err != nil || acc.HasPassword {
		t.Fatalf("the refused forms set a password: %+v, %v", acc, err)
	}
	resp, _ = b.post("/account/invite", url.Values{"password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the form as its page wrote it: status %d", resp.StatusCode)
	}

	// A form that is refused for what it says is shown again with the value.
	token = e.invite("ben", "")
	b = e.browser()
	b.openLink("invite", token)
	resp, body = b.post("/account/invite", url.Values{"password": {password}, "confirm": {password + "x"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `name="form_token" value="`+account.LinkFormToken(token)+`"`) {
		t.Errorf("the form shown again: status %d: %s", resp.StatusCode, body)
	}
}
