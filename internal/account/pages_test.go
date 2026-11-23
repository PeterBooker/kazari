package account_test

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

// form posts a form of a signed-in page with the session's form token.
func (b *browser) form(path string, v url.Values) (*http.Response, string) {
	b.e.t.Helper()
	if v == nil {
		v = url.Values{}
	}
	v.Set("form_token", b.token())
	return b.post(path, v)
}

// done checks that resp goes back to the page with slug.
func done(t *testing.T, what string, resp *http.Response, body, slug string) {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/pages/"+slug {
		t.Fatalf("%s: status %d to %q: %s", what, resp.StatusCode, resp.Header.Get("Location"), body)
	}
}

var shareURL = regexp.MustCompile(`value="(https://notes\.art\.test/[^"]+)"`)

// A member manages their own page in the browser: who opens it, share
// links, versions, and taking it offline.
func TestOwnPage(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.person("ana", "")
	e.page("ana", "notes", "Notes", artifact.Members)
	ana := artifact.Actor{Handle: "ana"}
	if _, _, err := e.arts.Update(ctx, ana, artifact.UpdateRequest{Slug: "notes",
		Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("<p>two</p>")}}}); err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("ana", password)

	resp, body := b.get("/account/pages/notes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the page's page: status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"<title>Notes · Kazari</title>", "Version 2", `href="/account/pages/notes/versions/1/publish"`,
		`action="/account/pages/notes/links"`, "Make a share link", `action="/account/pages/notes/stale"`, "Move to the bin"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page's page lacks %q", want)
		}
	}

	resp, body = b.form("/account/pages/notes/links", url.Values{"label": {"the client"}, "days": {"7"}})
	m := shareURL.FindStringSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil || !strings.Contains(body, "Shown once") {
		t.Fatalf("a share link: status %d: %s", resp.StatusCode, body)
	}
	links, err := e.shares.ListShareLinks(ctx, ana, "notes")
	if err != nil || len(links) != 1 || links[0].Label != "the client" || !links[0].ExpiresAt.Equal(t0.Add(7*24*time.Hour)) {
		t.Fatalf("share links = %+v, %v", links, err)
	}
	if !strings.HasPrefix(m[1], "https://notes.art.test/") {
		t.Errorf("the link %q does not open the page", m[1])
	}
	if resp, _ := b.post("/account/pages/notes/links", url.Values{"form_token": {b.token()}, "label": {"again"}, "days": {"7"}, "once": {"used"}}); resp.StatusCode == http.StatusOK {
		t.Error("a link form with a used single-use value made a link")
	}
	resp, body = b.get("/account/pages/notes/links/" + links[0].Prefix + "/revoke")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Revoke the link") {
		t.Fatalf("revoke confirmation: status %d: %s", resp.StatusCode, body)
	}
	resp, body = b.form("/account/pages/notes/links/"+links[0].Prefix+"/revoke", nil)
	done(t, "revoke", resp, body, "notes")
	if links, _ := e.shares.ListShareLinks(ctx, ana, "notes"); links[0].RevokedAt == nil {
		t.Error("the share link was not revoked")
	}

	resp, body = b.get("/account/pages/notes/versions/1/publish")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "in place of version 2") {
		t.Fatalf("publish confirmation: status %d: %s", resp.StatusCode, body)
	}
	resp, body = b.form("/account/pages/notes/versions/1/publish", nil)
	done(t, "publish", resp, body, "notes")

	resp, body = b.form("/account/pages/notes/visibility", url.Values{"visibility": {"public"}})
	done(t, "visibility", resp, body, "notes")
	_, body = b.get("/account/pages/notes")
	if !strings.Contains(body, "Who can open the page is changed.") || strings.Contains(body, `action="/account/pages/notes/links"`) ||
		!strings.Contains(body, "Make it members only") {
		t.Errorf("a public page offers a share link, or no way to members only:\n%s", body)
	}

	resp, body = b.form("/account/pages/notes/stale", url.Values{"days": {"0"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Enter a whole number of days from 1 to 3650") {
		t.Errorf("stale for 0 days: status %d", resp.StatusCode)
	}
	resp, body = b.form("/account/pages/notes/stale", url.Values{"days": {"3"}})
	done(t, "stale", resp, body, "notes")
	a, _, err := e.arts.Get(ctx, ana, "notes")
	if err != nil || a.PublishedVersion != 1 || a.Visibility != artifact.Public || a.ExpiresAt == nil || !a.ExpiresAt.Equal(t0.Add(3*24*time.Hour)) {
		t.Fatalf("the page after the changes = %+v, %v", a, err)
	}
	resp, body = b.form("/account/pages/notes/keep", nil)
	done(t, "keep", resp, body, "notes")
	resp, body = b.form("/account/pages/notes/bin", nil)
	done(t, "bin", resp, body, "notes")
	_, body = b.get("/account/pages/notes")
	if !strings.Contains(body, "In the bin") || !strings.Contains(body, "Restore the page") {
		t.Errorf("a page in the bin offers no restore:\n%s", body)
	}
	resp, body = b.form("/account/pages/notes/restore", nil)
	done(t, "restore", resp, body, "notes")
	if a, _, _ := e.arts.Get(ctx, ana, "notes"); a.DeletedAt != nil || a.StaleAt != nil {
		t.Errorf("the page after the restore = %+v", a)
	}

	var actions []string
	events, err := e.audit.List(ctx, audit.Query{Slug: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events.Events {
		if ev.Action == audit.ArtifactCreate || ev.Action == audit.ArtifactUpdate && ev.Details["credential"] == "" {
			continue // the setup above, through the service
		}
		if ev.Actor != "ana" || ev.Details["credential"] != auth.SourceSession {
			t.Errorf("%s by %q with %q, want ana's session", ev.Action, ev.Actor, ev.Details["credential"])
		}
		actions = append(actions, ev.Action)
	}
	if len(actions) < 9 {
		t.Errorf("the activity log holds %v", actions)
	}
}

// What a person may change is what the artifact service allows them, as
// over the API: another's page under the owner policy is for looking, and
// a sign-in with a narrow key changes nothing its key cannot.
func TestOwnPageAuthorisation(t *testing.T) {
	ctx := context.Background()
	t.Run("another's page", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		e.person("ana", "")
		e.person("ben", "")
		e.page("ana", "notes", "Notes", artifact.Members)
		e.page("ana", "draft", "Draft", artifact.Members)
		if _, err := e.arts.SetVisibility(ctx, artifact.Actor{Handle: "ana"}, "draft", artifact.Unpublished); err != nil {
			t.Fatal(err)
		}
		b := e.browser()
		b.signIn("ben", password)
		resp, body := b.get("/account/pages/notes")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "You can look, not change") || strings.Contains(body, `action="/account/pages/notes/visibility"`) {
			t.Fatalf("another's page: status %d: %s", resp.StatusCode, body)
		}
		for _, path := range []string{"visibility", "links", "stale", "bin", "versions/1/publish"} {
			resp, body := b.form("/account/pages/notes/"+path, url.Values{"visibility": {"public"}, "days": {"3"}})
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Nothing <b>was changed</b>") {
				t.Errorf("%s on another's page: status %d", path, resp.StatusCode)
			}
		}
		if resp, _ := b.get("/account/pages/draft"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("another's unpublished page: status %d, want 404", resp.StatusCode)
		}
		if a, _, _ := e.arts.Get(ctx, artifact.Actor{Handle: "ana"}, "notes"); a.Visibility != artifact.Members || a.StaleAt != nil || a.DeletedAt != nil {
			t.Errorf("another's page was changed: %+v", a)
		}
	})
	t.Run("shared policy", func(t *testing.T) {
		e := newEnv(t, envOptions{shared: true})
		e.person("ana", "")
		e.person("ben", "")
		e.page("ana", "notes", "Notes", artifact.Members)
		b := e.browser()
		b.signIn("ben", password)
		resp, body := b.form("/account/pages/notes/visibility", url.Values{"visibility": {"unlisted"}})
		done(t, "visibility under the shared policy", resp, body, "notes")
	})
	t.Run("a narrow key", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		e.person("ana", "")
		e.page("ana", "notes", "Notes", artifact.Members)
		_, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}})
		if err != nil {
			t.Fatal(err)
		}
		b := e.browser()
		b.signIn("ana", key)
		_, body := b.get("/account/pages/notes")
		if strings.Contains(body, `action="/account/pages/notes/links"`) || strings.Contains(body, "Move to the bin") {
			t.Error("a key without share and delete is offered sharing and the bin")
		}
		for _, path := range []string{"visibility", "links", "bin"} {
			if resp, _ := b.form("/account/pages/notes/"+path, url.Values{"visibility": {"public"}, "days": {"3"}}); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s with a key that may not: status %d", path, resp.StatusCode)
			}
		}
		resp, body := b.form("/account/pages/notes/stale", url.Values{"days": {"3"}})
		done(t, "stale with a key that may write", resp, body, "notes")
		_, body = b.get("/account/")
		if strings.Contains(body, `value="artifacts:share"`) || !strings.Contains(body, `value="artifacts:write"`) {
			t.Error("the key form offers what the session's key does not hold")
		}
	})
}

// The recovery form returns to the sign-in it was opened from.
func TestRecoveryReturnsToTheSignIn(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	codes, err := e.users.NewRecoveryCodes(context.Background(), "ana")
	if err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("n", 43)
	b := e.browser()
	q := url.Values{"host": {"demo"}, "path": {"/report"}, "nonce": {nonce}}
	resp, body := b.get("/account/recover?" + q.Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="nonce" value="`+nonce+`"`) {
		t.Fatalf("the recovery form: status %d: %s", resp.StatusCode, body)
	}
	const next = "a brand new long password"
	form := url.Values{"login": {"ana"}, "code": {codes[0]}, "password": {next}, "confirm": {next}}
	maps.Copy(form, q)
	resp, body = b.post("/account/recover", form)
	want := e.mcp.URL + oauth.PathSignIn + "?" + url.Values{"host": {"demo"}, "login": {"ana"}, "nonce": {nonce}, "notice": {"recovered"}, "path": {"/report"}}.Encode()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
		t.Fatalf("recovered: status %d to %q, want %q: %s", resp.StatusCode, resp.Header.Get("Location"), want, body)
	}

	resp, _ = b.post("/account/recover", url.Values{"login": {"ana"}, "code": {codes[1]}, "password": {next + "!"}, "confirm": {next + "!"}, "request_id": {"the-request"}})
	if want := e.mcp.URL + oauth.PathConsent + "?login=ana&notice=recovered"; resp.Header.Get("Location") != want {
		t.Errorf("recovered for an authorization: to %q, want %q", resp.Header.Get("Location"), want)
	}
}

func TestOwnPageShareLinkPassword(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.person("ana", "")
	e.page("ana", "notes", "Notes", artifact.Members)
	b := e.browser()
	b.signIn("ana", password)

	_, body := b.get("/account/pages/notes")
	if !strings.Contains(body, `name="password"`) {
		t.Fatalf("the share-link form has no password field:\n%s", body)
	}
	resp, body := b.form("/account/pages/notes/links", url.Values{"days": {"3"}, "password": {"short"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Use 12 to 256 characters") {
		t.Errorf("a short password: status %d, want 422 saying how long it must be", resp.StatusCode)
	}
	resp, body = b.form("/account/pages/notes/links", url.Values{"days": {"3"}, "password": {"open sesame please"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "and the password") || strings.Contains(body, "open sesame please") {
		t.Fatalf("a link with a password: status %d, want the link shown once, its password never:\n%s", resp.StatusCode, body)
	}
	links, err := e.shares.ListShareLinks(t.Context(), artifact.Actor{Handle: "ana"}, "notes")
	if err != nil || len(links) != 1 || !links[0].HasPassword {
		t.Fatalf("links after creating one with a password = %+v, %v", links, err)
	}
}

func TestOwnPageDeletedVersion(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	e.person("ana", "")
	e.page("ana", "notes", "Notes", artifact.Members)
	ana := artifact.Actor{Handle: "ana"}
	for _, body := range []string{"<p>two</p>", "<p>three</p>"} {
		if _, _, err := e.arts.Update(ctx, ana, artifact.UpdateRequest{Slug: "notes",
			Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader(body)}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.arts.DeleteVersion(ctx, ana, "notes", 2); err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("ana", password)
	if resp, _ := b.get("/account/pages/notes/versions/2/publish"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("publish confirmation for a deleted version: status %d, want 404", resp.StatusCode)
	}
	if resp, _ := b.get("/account/pages/notes/versions/1/publish"); resp.StatusCode != http.StatusOK {
		t.Errorf("publish confirmation for a kept version: status %d, want 200", resp.StatusCode)
	}
}
