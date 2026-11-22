package dashboard_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/user"
)

var boss = artifact.Actor{Handle: "boss", Admin: true, CanDelete: true}

// listed reports whether the page's row or link is on the page.
func listed(r response, slug string) bool {
	return strings.Contains(r.body, `href="/pages/`+slug+`"`)
}

// addVersion publishes another version of slug as handle.
func (e *env) addVersion(handle, slug, body string) {
	e.t.Helper()
	_, _, err := e.artifacts.Update(context.Background(), artifact.Actor{Handle: handle}, artifact.UpdateRequest{
		Slug: slug, Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader(body)}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) page(slug string) *artifact.Artifact {
	e.t.Helper()
	a, _, err := e.artifacts.Get(context.Background(), boss, slug)
	if err != nil {
		e.t.Fatalf("Get(%s): %v", slug, err)
	}
	return a
}

// pagesEnv is an instance with an admin, two members and a page in every
// state: live-one, members-one (members only), stale-one, hidden-one and
// binned-one.
func pagesEnv(t *testing.T) (*env, *browser) {
	t.Helper()
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("ben", "")
	ctx := context.Background()
	e.publish("ana", "live-one")
	e.publish("ben", "members-one")
	e.publish("ana", "stale-one")
	e.publish("ana", "hidden-one")
	e.publish("ben", "binned-one")
	must := func(_ *artifact.Artifact, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.artifacts.SetVisibility(ctx, boss, "members-one", string(artifact.Members)))
	must(e.artifacts.MarkStale(ctx, boss, "stale-one", 0))
	must(e.artifacts.SetVisibility(ctx, boss, "hidden-one", artifact.Unpublished))
	if err := e.artifacts.Delete(ctx, boss, "binned-one", false); err != nil {
		t.Fatal(err)
	}
	b := e.signedIn("boss")
	// The first page gives the browser the session's form token.
	b.get("/pages")
	return e, b
}

var allPages = []string{"live-one", "members-one", "stale-one", "hidden-one", "binned-one"}

func TestPagesList(t *testing.T) {
	_, b := pagesEnv(t)
	r := b.get("/pages")
	for _, slug := range allPages {
		if !listed(r, slug) {
			t.Errorf("the list lacks %s", slug)
		}
	}
	for _, want := range []string{
		`<span class="ja" lang="ja" aria-hidden="true">ページ</span>`,
		`<span class="status tone-ok">Live</span>`, `<span class="status tone-warn">Stale</span>`,
		`<span class="status tone-neutral">Unpublished</span>`, `<span class="status tone-danger">In the bin</span>`,
		"Members only", `href="/people/ana"`, `href="/people/ben"`,
		// Stale on 21 Nov with the default grace of 14 days; binned on 21 Nov
		// with a retention of 30.
		`Moves to the bin on <time datetime="2026-12-05T12:00:00Z">5 Dec 2026</time>`,
		`Removed for good on <time datetime="2026-12-21T12:00:00Z">21 Dec 2026</time>`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(r.body), "artifact") {
		t.Error("the list says artifact; a person reads page")
	}

	tests := []struct {
		query string
		want  []string
	}{
		{"q=stale", []string{"stale-one"}},
		{"q=ONE", allPages},
		{"q=nothing-like-it", nil},
		{"status=live", []string{"live-one", "members-one"}},
		{"status=stale", []string{"stale-one"}},
		{"status=unpublished", []string{"hidden-one"}},
		{"status=bin", []string{"binned-one"}},
		{"visibility=members", []string{"members-one"}},
		{"visibility=public", []string{"live-one", "stale-one"}},
		{"owner=ben", []string{"members-one", "binned-one"}},
		{"owner=ben&status=live", []string{"members-one"}},
		{"owner=ana&q=one&status=stale", []string{"stale-one"}},
		{"status=unpublished&visibility=members", nil},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			r := b.get("/pages?" + tt.query)
			for _, slug := range allPages {
				want := false
				for _, w := range tt.want {
					want = want || w == slug
				}
				if got := listed(r, slug); got != want {
					t.Errorf("%s listed = %v, want %v", slug, got, want)
				}
			}
			if len(tt.want) == 0 && !strings.Contains(r.body, "No page matches this filter.") {
				t.Error("an empty result does not say so")
			}
		})
	}
	// The filter stays in the form, so it stays in the URL of the next search.
	r = b.get("/pages?q=stale&status=stale&owner=ana")
	for _, want := range []string{`name="q" type="search" value="stale"`, `<option value="stale" selected>`, `<option value="ana" selected>`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the filter form lacks %q", want)
		}
	}
}

func TestPagesListEmpty(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	r := e.signedIn("boss").get("/pages")
	if !strings.Contains(r.body, "No page has been published yet.") || !strings.Contains(r.body, "kazari publish") {
		t.Errorf("the empty list does not say how a first page gets published: %s", r.body)
	}
}

func TestPagesListCursor(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.publish("ana", "another")
	for i := range 55 {
		e.clock.Advance(time.Second)
		e.publish("ana", fmt.Sprintf("bulk-%02d", i))
	}
	b := e.signedIn("boss")
	rows := func(r response) int { return strings.Count(r.body, `href="/pages/bulk-`) }

	first := b.get("/pages?q=bulk")
	next := regexp.MustCompile(`href="(/pages\?[^"]*cursor=[^"]*)" rel="next"`).FindStringSubmatch(first.body)
	if rows(first) != 50 || next == nil || !listed(first, "bulk-54") || listed(first, "bulk-04") {
		t.Fatalf("first page: %d rows, next %v", rows(first), next)
	}
	if strings.Contains(first.body, "First page") {
		t.Error("the first page links to itself")
	}
	addr := strings.ReplaceAll(next[1], "&amp;", "&")
	if u, _ := url.Parse(addr); u.Query().Get("q") != "bulk" {
		t.Errorf("the next link %q dropped the filter", addr)
	}
	second := b.get(addr)
	if rows(second) != 5 || !listed(second, "bulk-04") || listed(second, "another") || strings.Contains(second.body, `rel="next"`) {
		t.Errorf("second page: %d rows: %s", rows(second), second.body)
	}
	if !strings.Contains(second.body, `href="/pages?q=bulk">First page</a>`) {
		t.Error("the second page has no way back to the first")
	}
	if r := b.navigate("/pages?cursor=not-a-cursor"); r.StatusCode == http.StatusInternalServerError {
		t.Errorf("a made-up cursor: status %d", r.StatusCode)
	}
}

func TestBinAndRestore(t *testing.T) {
	e, b := pagesEnv(t)
	r := b.get("/bin")
	if !listed(r, "binned-one") || listed(r, "live-one") {
		t.Fatalf("the bin: %s", r.body)
	}
	for _, want := range []string{`<time datetime="2026-12-21T12:00:00Z">21 Dec 2026</time>`, `action="/pages/binned-one/restore"`, "30 days"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the bin lacks %q", want)
		}
	}
	// A page whose slug is "bin" keeps its own page.
	e.publish("ana", "bin")
	if r := b.get("/pages/bin"); !strings.Contains(r.body, "Address and access") {
		t.Errorf("/pages/bin is not the page with that slug: %s", r.body)
	}

	r = b.submit("/pages/binned-one/restore", nil)
	if !strings.Contains(r.body, "The page is restored from the bin.") || e.page("binned-one").DeletedAt != nil {
		t.Errorf("restore: %s", r.body)
	}
	if ev := e.events(audit.ArtifactRestore); len(ev) != 1 || ev[0].Actor != "boss" || ev[0].Details["credential"] != "session" {
		t.Errorf("restore audit = %+v", ev)
	}
	// Restoring what is not in the bin changes nothing and records nothing.
	before := len(e.events(audit.ArtifactUpdate))
	b.submit("/pages/binned-one/restore", nil)
	if after := len(e.events(audit.ArtifactUpdate)); after != before {
		t.Errorf("a second restore recorded %d more events", after-before)
	}
	if r := b.get("/bin"); !strings.Contains(r.body, "The bin is empty.") {
		t.Errorf("the empty bin: %s", r.body)
	}
}

func TestPageOwnPage(t *testing.T) {
	e, b := pagesEnv(t)
	e.clock.Advance(time.Hour)
	e.addVersion("ben", "live-one", "<h1>two</h1>")
	r := b.get("/pages/live-one")
	for _, want := range []string{
		`href="https://live-one.art.test/"`, "live-one.art.test", `href="https://live-one--v1.art.test/"`, `href="https://live-one--v2.art.test/"`,
		`<span class="status tone-version">Version 2</span> <span class="status tone-ok">Published</span>`,
		`href="/pages/live-one/versions/1/publish"`, `href="/people/ana"`, `href="/people/ben"`,
		`href="/activity?slug=live-one"`, "published version 2 of", `<span class="mono soft">artifact.update</span>`,
		`action="/pages/live-one/hide"`, `action="/pages/live-one/bin"`, `href="/pages/live-one/takedown"`, `href="/pages/live-one/remove"`,
		`<option value="ben">ben</option>`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the page's page lacks %q", want)
		}
	}
	if strings.Contains(r.body, `href="/pages/live-one/versions/2/publish"`) {
		t.Error("the published version offers to be published")
	}
	if strings.Contains(r.body, `<option value="ana">`) {
		t.Error("the page can be handed to its own owner")
	}
	if r := b.navigate("/pages/no-such-page"); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "There is no page at that address.") {
		t.Errorf("an unknown slug: status %d: %s", r.StatusCode, r.body)
	}

	// In the bin: restore and remove, and nothing that needs a served page.
	r = b.get("/pages/binned-one")
	for _, want := range []string{"In the bin", `action="/pages/binned-one/restore"`, "Removed for good on", `href="/pages/binned-one/remove"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("a binned page's page lacks %q", want)
		}
	}
	for _, not := range []string{`action="/pages/binned-one/hide"`, `action="/pages/binned-one/links"`, `href="/pages/binned-one/takedown"`, `action="/pages/binned-one/visibility"`} {
		if strings.Contains(r.body, not) {
			t.Errorf("a binned page's page offers %q", not)
		}
	}
}

func TestPageVisibilityAndHide(t *testing.T) {
	e, b := pagesEnv(t)
	r := b.submit("/pages/live-one/visibility", url.Values{"visibility": {"members"}})
	if e.page("live-one").Visibility != artifact.Members || !strings.Contains(r.body, "Who can open the page is changed.") {
		t.Fatalf("visibility: %s", r.body)
	}
	if r := b.post("/pages/live-one/visibility", url.Values{"visibility": {"everyone"}}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a made-up visibility: status %d, want 422", r.StatusCode)
	}

	// Hiding needs no confirmation: the page it leads to offers the way back.
	r = b.submit("/pages/live-one/hide", nil)
	if e.page("live-one").UnpublishedAt == nil || !strings.Contains(r.body, "The page is hidden.") {
		t.Fatalf("hide: %s", r.body)
	}
	for _, want := range []string{"Publish again", `<span class="status tone-neutral">Unpublished</span>`, `<option value="members" selected>`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("a hidden page's page lacks %q", want)
		}
	}
	if strings.Contains(r.body, `action="/pages/live-one/hide"`) {
		t.Error("a hidden page offers to be hidden")
	}
	b.submit("/pages/live-one/visibility", url.Values{"visibility": {"members"}})
	if a := e.page("live-one"); a.UnpublishedAt != nil || a.Visibility != artifact.Members {
		t.Errorf("publishing again left the page %+v", a)
	}
}

func TestPageKeep(t *testing.T) {
	e, b := pagesEnv(t)
	r := b.get("/pages/stale-one")
	for _, want := range []string{"Stale since", "Moves to the bin on", `<time datetime="2026-12-05T12:00:00Z">5 Dec 2026</time>`, "Keep the page"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("a stale page's page lacks %q", want)
		}
	}
	r = b.submit("/pages/stale-one/keep", nil)
	if a := e.page("stale-one"); a.StaleAt != nil || a.ExpiresAt != nil || !strings.Contains(r.body, "The stale mark is cleared.") {
		t.Errorf("keep left %+v: %s", a, r.body)
	}
}

func TestPagePublishVersion(t *testing.T) {
	e, b := pagesEnv(t)
	e.addVersion("ana", "live-one", "<h1>two</h1>")
	r := b.get("/pages/live-one/versions/1/publish")
	for _, want := range []string{
		"The address live-one.art.test shows version 1 from now on, in place of version 2.",
		"No version is deleted", "makes a new version and goes live as usual", `action="/pages/live-one/versions/1/publish"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the confirmation lacks %q", want)
		}
	}
	if got := e.page("live-one").PublishedVersion; got != 2 {
		t.Fatalf("opening the confirmation published version %d", got)
	}
	r = b.submit("/pages/live-one/versions/1/publish", nil)
	if got := e.page("live-one").PublishedVersion; got != 1 || !strings.Contains(r.body, "That version is now the published one.") {
		t.Fatalf("published version = %d: %s", got, r.body)
	}
	if ev := e.events(audit.ArtifactPublish); len(ev) != 1 || ev[0].Actor != "boss" || ev[0].Details["version"] != "1" {
		t.Errorf("publish audit = %+v", ev)
	}
	for path, want := range map[string]int{
		"/pages/live-one/versions/1/publish":     http.StatusUnprocessableEntity, // already published
		"/pages/live-one/versions/9/publish":     http.StatusNotFound,
		"/pages/live-one/versions/zero/publish":  http.StatusNotFound,
		"/pages/no-such-page/versions/1/publish": http.StatusNotFound,
	} {
		if r := b.navigate(path); r.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", path, r.StatusCode, want)
		}
	}
	if r := b.post("/pages/live-one/versions/9/publish", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("publishing a version that does not exist: status %d", r.StatusCode)
	}
}

var shareToken = regexp.MustCompile(`kzs_[a-z2-7]{8}_[a-z2-7]{52}`)

func TestPageShareLink(t *testing.T) {
	e, b := pagesEnv(t)
	ctx := context.Background()
	b.get("/pages/members-one")
	r := b.post("/pages/members-one/links", url.Values{"label": {"for the client"}, "days": {"3"}})
	token := shareToken.FindString(r.body)
	if r.StatusCode != http.StatusOK || token == "" {
		t.Fatalf("create: status %d: %s", r.StatusCode, r.body)
	}
	for _, want := range []string{
		`value="https://members-one.art.test/.kazari/share?token=` + token + `"`, "Shown once", "for the client",
		"It works until 24 Nov 2026", `data-copy-target="once-value"`, `href="/pages/members-one">Back to the page`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the link's page lacks %q", want)
		}
	}
	// The link is in that one response and nowhere it could be kept.
	if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Location") != "" {
		t.Errorf("Cache-Control %q, Location %q", r.Header.Get("Cache-Control"), r.Header.Get("Location"))
	}
	for _, ck := range r.Header.Values("Set-Cookie") {
		if strings.Contains(ck, "kzs_") {
			t.Errorf("the link is in a cookie: %s", ck)
		}
	}
	l, err := e.artifacts.CheckShareLink(ctx, "members-one", token)
	if err != nil || l.Label != "for the client" || !l.ExpiresAt.Equal(t0.Add(3*24*time.Hour)) || l.CreatedBy != "boss" {
		t.Fatalf("the link shown opens nothing: %+v, %v", l, err)
	}
	r = b.get("/pages/members-one")
	if strings.Contains(r.body, token) || shareToken.MatchString(r.body) {
		t.Error("the page's page shows the link again")
	}
	for _, want := range []string{l.Prefix, "for the client", `<span class="status tone-ok">Active</span>`, `href="/pages/members-one/links/` + l.Prefix + `/revoke"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the links table lacks %q", want)
		}
	}

	for _, days := range []string{"0", "91", "-3", "soon", ""} {
		r := b.post("/pages/members-one/links", url.Values{"label": {"kept label"}, "days": {days}})
		if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "Give a number of days from 1 to 90.") ||
			!strings.Contains(r.body, `value="kept label"`) || !strings.Contains(r.body, `aria-invalid="true"`) {
			t.Errorf("days %q: status %d", days, r.StatusCode)
		}
	}
	if links, _ := e.artifacts.ListShareLinks(ctx, boss, "members-one"); len(links) != 1 {
		t.Errorf("refused forms created links: %d", len(links))
	}

	// Revoking goes through a page that says what ends.
	r = b.get("/pages/members-one/links/" + l.Prefix + "/revoke")
	if !strings.Contains(r.body, "The link stops opening the page at once.") || !strings.Contains(r.body, "This cannot be undone.") {
		t.Errorf("the revoke confirmation: %s", r.body)
	}
	if _, err := e.artifacts.CheckShareLink(ctx, "members-one", token); err != nil {
		t.Fatalf("opening the confirmation revoked the link: %v", err)
	}
	r = b.submit("/pages/members-one/links/"+l.Prefix+"/revoke", nil)
	if _, err := e.artifacts.CheckShareLink(ctx, "members-one", token); !errors.Is(err, artifact.ErrLinkNotFound) {
		t.Errorf("the link after its revoke: %v", err)
	}
	if !strings.Contains(r.body, "The share link is revoked.") || !strings.Contains(r.body, `<span class="status tone-danger">Revoked</span>`) {
		t.Errorf("after the revoke: %s", r.body)
	}
	for _, path := range []string{"/pages/members-one/links/aaaaaaaa/revoke", "/pages/live-one/links/" + l.Prefix + "/revoke"} {
		if r := b.navigate(path); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, r.StatusCode)
		}
		if r := b.post(path, nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", path, r.StatusCode)
		}
	}
}

func TestPageHandOver(t *testing.T) {
	e, b := pagesEnv(t)
	r := b.submit("/pages/live-one/owner", url.Values{"owner": {"ben"}})
	if e.page("live-one").OwnerHandle != "ben" || !strings.Contains(r.body, "The page has a new owner.") {
		t.Fatalf("hand over: %s", r.body)
	}
	if ev := e.events(audit.ArtifactTransfer); len(ev) != 1 || ev[0].Details["from"] != "ana" || ev[0].Details["to"] != "ben" {
		t.Errorf("transfer audit = %+v", ev)
	}
	for _, owner := range []string{"", "nobody"} {
		if r := b.post("/pages/live-one/owner", url.Values{"owner": {owner}}); r.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("handing over to %q: status %d, want 422", owner, r.StatusCode)
		}
	}
}

func TestPageToBin(t *testing.T) {
	e, b := pagesEnv(t)
	r := b.submit("/pages/live-one/bin", nil)
	if e.page("live-one").DeletedAt == nil || !strings.Contains(r.body, "The page is in the bin.") || !strings.Contains(r.body, "Restore the page") {
		t.Fatalf("move to the bin: %s", r.body)
	}
	if !listed(b.get("/bin"), "live-one") {
		t.Error("the bin lacks the page")
	}
}

func TestPageTakeDown(t *testing.T) {
	e, b := pagesEnv(t)
	ctx := context.Background()
	enabled := func(handle string) bool {
		u, err := e.users.GetUser(ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		return u.DisabledAt == nil
	}

	r := b.get("/pages/live-one/takedown")
	for _, want := range []string{
		"live-one.art.test and the address of every version answer 404 at once", "Nothing is deleted.",
		`name="disable_owner" value="1" checked`, "Also disable its owner, ana", "Hiding is not a lock", "for good",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the confirmation lacks %q", want)
		}
	}
	if e.page("live-one").UnpublishedAt != nil {
		t.Fatal("opening the confirmation hid the page")
	}

	// Without the tick: hidden, the owner untouched.
	r = b.submit("/pages/live-one/takedown", nil)
	if e.page("live-one").UnpublishedAt == nil || !enabled("ana") || !strings.Contains(r.body, "The page is hidden. Its owner can still sign in.") {
		t.Fatalf("take down without the owner: %s", r.body)
	}

	// With it: the owner is disabled, and what they held ends.
	_, key, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ben"})
	if err != nil {
		t.Fatal(err)
	}
	r = b.submit("/pages/members-one/takedown", url.Values{"disable_owner": {"yes"}})
	if e.page("members-one").UnpublishedAt == nil || enabled("ben") || !strings.Contains(r.body, "The page is hidden and its owner is disabled.") {
		t.Fatalf("take down with the owner: %s", r.body)
	}
	if _, err := e.users.Authenticate(ctx, key); err == nil {
		t.Error("the disabled owner's key still works")
	}
	if r := b.get("/pages/hidden-one/takedown"); !strings.Contains(r.body, "Also disable its owner, ana") {
		t.Errorf("a hidden page's owner can still be disabled: %s", r.body)
	}
	if r := b.get("/pages/stale-one/takedown"); !strings.Contains(r.body, "Also disable") {
		t.Error("ana is enabled, and the confirmation does not offer to disable them")
	}

	// A guard of the user service refuses: the page is hidden all the same,
	// and the next page says what did not happen.
	e.publish("boss", "boss-page")
	if r := b.get("/pages/boss-page/takedown"); strings.Contains(r.body, `name="disable_owner"`) || !strings.Contains(r.body, "nobody disables their own account") {
		t.Errorf("the confirmation for one's own page: %s", r.body)
	}
	r = b.submit("/pages/boss-page/takedown", url.Values{"disable_owner": {"yes"}})
	if e.page("boss-page").UnpublishedAt == nil || !enabled("boss") ||
		!strings.Contains(r.body, "The page is hidden. You own it, and nobody disables their own account.") {
		t.Fatalf("take down of one's own page: %s", r.body)
	}
	if r := b.get("/pages/members-one/takedown"); strings.Contains(r.body, `name="disable_owner"`) || !strings.Contains(r.body, "ben, is already disabled") {
		t.Errorf("the confirmation for a disabled owner's page: %s", r.body)
	}
}

func TestPageRemoveForGood(t *testing.T) {
	e, b := pagesEnv(t)
	ctx := context.Background()
	r := b.get("/pages/live-one/remove")
	for _, want := range []string{
		"This cannot be undone.", "for up to 90 days", "until the next clean-up", "live-one.art.test",
		"no new page can take for those 90 days", "restoring it for 30 days", `name="slug"`, `class="btn btn-danger"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the confirmation lacks %q", want)
		}
	}
	for _, typed := range []string{"", "live", "LIVE-ONE", "stale-one"} {
		r := b.post("/pages/live-one/remove", url.Values{"slug": {typed}})
		if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "That is not the address of this page. Nothing was removed.") {
			t.Errorf("typed %q: status %d", typed, r.StatusCode)
		}
		if _, _, err := e.artifacts.Get(ctx, boss, "live-one"); err != nil {
			t.Fatalf("typed %q removed the page: %v", typed, err)
		}
	}
	r = b.submit("/pages/live-one/remove", url.Values{"slug": {" live-one "}})
	if !strings.Contains(r.body, "The page is removed for good.") || listed(r, "live-one") {
		t.Fatalf("after the removal: %s", r.body)
	}
	if _, _, err := e.artifacts.Get(ctx, boss, "live-one"); !errors.Is(err, artifact.ErrGone) {
		t.Errorf("the page after its removal: %v", err)
	}
	if r := b.navigate("/pages/live-one"); r.StatusCode != http.StatusGone || !strings.Contains(r.body, "removed for good") {
		t.Errorf("the removed page's address: status %d: %s", r.StatusCode, r.body)
	}
	// From the bin too.
	b.submit("/pages/binned-one/remove", url.Values{"slug": {"binned-one"}})
	if _, _, err := e.artifacts.Get(ctx, boss, "binned-one"); !errors.Is(err, artifact.ErrGone) {
		t.Errorf("a binned page after its removal: %v", err)
	}
}

// A change after the session lost its rights is refused, and nothing on a
// GET changes a page.
func TestPageActionsNeedAnAdmin(t *testing.T) {
	e, b := pagesEnv(t)
	e.person("second", user.RoleAdmin)
	b.get("/pages/live-one")
	for _, path := range []string{"/pages/live-one/hide", "/pages/live-one/bin", "/pages/live-one/remove", "/pages/live-one/keep",
		"/pages/live-one/restore", "/pages/live-one/owner", "/pages/live-one/visibility", "/pages/live-one/links"} {
		if r := b.navigate(path); r.StatusCode == http.StatusSeeOther || r.StatusCode == http.StatusFound {
			t.Errorf("GET %s: status %d", path, r.StatusCode)
		}
	}
	if a := e.page("live-one"); a.UnpublishedAt != nil || a.DeletedAt != nil {
		t.Fatalf("a GET changed the page: %+v", a)
	}
	if r := b.postRaw("/pages/live-one/hide", url.Values{}, e.admin.URL); r.StatusCode != http.StatusForbidden {
		t.Errorf("a change without the form token: status %d, want 403", r.StatusCode)
	}
	if r := b.postRaw("/pages/live-one/hide", url.Values{"form_token": {b.token}}, "https://live-one.art.test"); r.StatusCode != http.StatusForbidden {
		t.Errorf("a change from an artifact's origin: status %d, want 403", r.StatusCode)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "boss", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Fatal(err)
	}
	if r := b.post("/pages/live-one/hide", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("a change after a demotion: status %d, want 403", r.StatusCode)
	}
	if e.page("live-one").UnpublishedAt != nil {
		t.Error("a refused change hid the page")
	}
}

const (
	hostileScript = `<script>alert(1)</script>`
	hostileAttr   = `"><img src=x onerror=alert(1)>`
)

// hostilePages is an instance whose names are markup: a page's name and
// description, a share link's label, a person's display name and details
// in the log.
func hostilePages(t *testing.T) (*env, *browser) {
	t.Helper()
	e, b := pagesEnv(t)
	ctx := context.Background()
	name, desc := hostileScript, hostileAttr
	if _, _, err := e.artifacts.Update(ctx, boss, artifact.UpdateRequest{Slug: "members-one", Name: &name, Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.artifacts.CreateShareLink(ctx, boss, "members-one", hostileAttr, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "ana", user.UpdateUserRequest{DisplayName: new(hostileScript)}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []audit.Event{
		{Actor: hostileScript, Action: audit.ArtifactImport, Slug: "members-one", Details: map[string]string{"source": hostileAttr, "version": hostileScript}},
		{Actor: "ben", Action: audit.ArtifactTransfer, Slug: "members-one", Details: map[string]string{"from": hostileScript, "to": hostileAttr}},
		{Actor: "ben", Action: hostileScript, Slug: hostileAttr},
		{Actor: "ben", Action: audit.KeyIssue, Details: map[string]string{"prefix": hostileAttr, "handle": hostileScript}},
	} {
		ev.At = e.clock.Now()
		if err := e.audit.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	return e, b
}

var pagesViews = []string{
	"/pages", "/pages?q=one&status=live", "/bin", "/pages/members-one", "/pages/binned-one", "/pages/stale-one",
	"/pages/members-one/takedown", "/pages/members-one/remove", "/pages/stale-one/remove", "/activity", "/activity?slug=members-one",
}

func TestPagesHostileNames(t *testing.T) {
	_, b := hostilePages(t)
	views := map[string]response{}
	for _, path := range pagesViews {
		views[path] = b.get(path)
	}
	views["a refused share link"] = b.post("/pages/members-one/links", url.Values{"label": {hostileAttr}, "days": {"0"}})
	views["a new share link"] = b.post("/pages/members-one/links", url.Values{"label": {hostileScript}, "days": {"1"}})
	views["a refused removal"] = b.post("/pages/members-one/remove", url.Values{"slug": {hostileAttr}})
	views["a filter"] = b.get("/pages?" + url.Values{"q": {hostileAttr}, "owner": {hostileScript}, "status": {hostileAttr}}.Encode())
	views["an activity filter"] = b.get("/activity?" + url.Values{"person": {hostileScript}, "slug": {hostileAttr}, "from": {hostileAttr}, "kind": {hostileScript}}.Encode())
	for name, r := range views {
		for _, raw := range []string{hostileScript, hostileAttr, "<img src=x", "onerror=alert(1)>"} {
			if strings.Contains(r.body, raw) {
				t.Errorf("%s: %q is on the page unescaped", name, raw)
			}
		}
	}
	// The names are there, as text.
	if r := views["/pages/members-one"]; !strings.Contains(r.body, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(r.body, "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;") {
		t.Errorf("the hostile names are missing as text: %s", r.body)
	}
	// A name that cannot be a handle or a slug is never made a link.
	if r := views["/activity"]; strings.Contains(r.body, `href="/people/%3Cscript`) || strings.Contains(r.body, `href="/pages/%22`) {
		t.Errorf("a hostile name became a link: %s", r.body)
	}
}

func TestPagesMarkup(t *testing.T) {
	e, b := hostilePages(t)
	e.addVersion("ana", "stale-one", "<h1>two</h1>")
	for _, path := range append([]string{"/pages/stale-one/versions/1/publish"}, pagesViews...) {
		strictMarkup(t, path, b.get(path).body)
	}
	strictMarkup(t, "a new share link", b.post("/pages/members-one/links", url.Values{"days": {"1"}}).body)
	strictMarkup(t, "an error page", b.navigate("/pages/no-such-page").body)
	l, _, err := e.artifacts.CreateShareLink(context.Background(), boss, "members-one", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	strictMarkup(t, "a revoke confirmation", b.get("/pages/members-one/links/"+l.Prefix+"/revoke").body)
}

// The live filter is the database's, so a page of results is a page of live
// pages however many hidden ones were changed more recently.
func TestPagesListLiveBehindHiddenOnes(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	ctx := context.Background()
	e.publish("ana", "live-old")
	for i := range 51 {
		e.clock.Advance(time.Second)
		slug := fmt.Sprintf("hidden-%02d", i)
		e.publish("ana", slug)
		if _, err := e.artifacts.SetVisibility(ctx, boss, slug, artifact.Unpublished); err != nil {
			t.Fatal(err)
		}
	}
	b := e.signedIn("boss")
	r := b.get("/pages?status=live")
	if !listed(r, "live-old") || strings.Contains(r.body, `href="/pages/hidden-`) || strings.Contains(r.body, `rel="next"`) {
		t.Errorf("the live pages, behind 51 hidden ones: %s", r.body)
	}
	if r = b.get("/pages?status=live&visibility=unpublished"); listed(r, "live-old") || strings.Contains(r.body, `href="/pages/hidden-`) {
		t.Errorf("live and unpublished at once lists something: %s", r.body)
	}
	if r = b.get("/pages?status=unpublished"); strings.Count(r.body, `href="/pages/hidden-`) != 50 || !strings.Contains(r.body, `rel="next"`) {
		t.Errorf("the hidden pages: %d rows", strings.Count(r.body, `href="/pages/hidden-`))
	}
}

// Choosing the published version of a hidden page does not show the page,
// and no sentence on the way says it does.
func TestPagePublishVersionOfHiddenPage(t *testing.T) {
	e, b := pagesEnv(t)
	e.addVersion("ana", "hidden-one", "<h1>two</h1>")
	if a := e.page("hidden-one"); a.UnpublishedAt == nil || a.PublishedVersion != 2 {
		t.Fatalf("hidden-one: published version %d, hidden %v", a.PublishedVersion, a.UnpublishedAt != nil)
	}
	r := b.get("/pages/hidden-one/versions/1/publish")
	for _, want := range []string{"Version 1 becomes the published version.", "The page stays hidden: hidden-one.art.test answers 404 until you publish the page again"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the confirmation lacks %q", want)
		}
	}
	if strings.Contains(r.body, "shows version 1 from now on") {
		t.Errorf("the confirmation says a hidden page shows a version: %s", r.body)
	}
	r = b.submit("/pages/hidden-one/versions/1/publish", nil)
	if a := e.page("hidden-one"); a.UnpublishedAt == nil || a.PublishedVersion != 1 {
		t.Fatalf("after the publish: published version %d, hidden %v", a.PublishedVersion, a.UnpublishedAt != nil)
	}
	if !strings.Contains(r.body, "The page is still hidden") || strings.Contains(r.body, "That version is now the published one.</p>") {
		t.Errorf("the confirmation line of a hidden page: %s", r.body)
	}
	// The published version of a hidden page is published already, like
	// any other.
	if r := b.navigate("/pages/hidden-one/versions/1/publish"); r.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(r.body, "the page is hidden") {
		t.Errorf("publishing the published version of a hidden page: status %d: %s", r.StatusCode, r.body)
	}
}

// What is wrong with a share link's form stands beside the field it is
// about, and the field points at it.
func TestPageShareLinkFieldErrors(t *testing.T) {
	_, b := pagesEnv(t)
	b.get("/pages/members-one")
	r := b.post("/pages/members-one/links", url.Values{"label": {"board"}, "days": {"0"}})
	if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, `<p class="error" id="link-days-err">`) ||
		!regexp.MustCompile(`<input[^>]+id="link-days"[^>]+aria-invalid="true" aria-describedby="link-days-err"`).MatchString(r.body) ||
		strings.Contains(r.body, `id="link-label-err"`) {
		t.Errorf("days out of range: status %d: %s", r.StatusCode, r.body)
	}
	r = b.post("/pages/members-one/links", url.Values{"label": {"two\nlines"}, "days": {"3"}})
	if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, `<p class="error" id="link-label-err">`) ||
		!regexp.MustCompile(`<input[^>]+id="link-label"[^>]+aria-invalid="true" aria-describedby="link-label-err"`).MatchString(r.body) ||
		strings.Contains(r.body, `id="link-days-err"`) || strings.Contains(r.body, "artifact:") {
		t.Errorf("a note of two lines: status %d: %s", r.StatusCode, r.body)
	}
	strictMarkup(t, "a refused share link", r.body)
}

// A share link is offered only where it opens something the address alone
// does not: a members page that is published. Elsewhere the page says why,
// with the change that would make one useful.
func TestPageShareLinkOnlyForMembers(t *testing.T) {
	e, b := pagesEnv(t)
	for _, tt := range []struct {
		slug, want string
		form       bool
	}{
		{"members-one", "Create a share link", true},
		{"live-one", "Make it members only", false},
		{"hidden-one", "Publish it, members only", false},
	} {
		r := b.get("/pages/" + tt.slug)
		strictMarkup(t, tt.slug, r.body)
		if got := strings.Contains(r.body, `action="/pages/`+tt.slug+`/links"`); got != tt.form || !strings.Contains(r.body, tt.want) {
			t.Errorf("%s: the share-link form shown = %t, want %t, with %q", tt.slug, got, tt.form, tt.want)
		}
	}
	r := b.submit("/pages/live-one/visibility", url.Values{"visibility": {"members"}})
	if !strings.Contains(r.body, `action="/pages/live-one/links"`) || e.page("live-one").Visibility != artifact.Members {
		t.Error("making a page members only does not offer the share link")
	}
}

// Marking a page stale from the dashboard starts its grace period.
func TestPageMarkStale(t *testing.T) {
	e, b := pagesEnv(t)
	r := b.get("/pages/live-one")
	for _, want := range []string{`action="/pages/live-one/stale"`, "Four ways to take a page offline", "restored for 30 days"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("a live page's page lacks %q", want)
		}
	}
	r = b.post("/pages/live-one/stale", url.Values{"days": {"soon"}})
	if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "Enter a whole number of days from 1 to 3650.") || !strings.Contains(r.body, `value="soon"`) {
		t.Errorf("stale for soon: status %d", r.StatusCode)
	}
	r = b.submit("/pages/live-one/stale", url.Values{"days": {"3"}})
	a := e.page("live-one")
	if a.ExpiresAt == nil || !a.ExpiresAt.Equal(t0.Add(3*24*time.Hour)) || !strings.Contains(r.body, "The page is marked stale.") {
		t.Errorf("stale for 3 days left %+v", a)
	}
	b.submit("/pages/live-one/keep", nil)
	b.submit("/pages/live-one/stale", url.Values{"days": {""}})
	if a := e.page("live-one"); a.ExpiresAt == nil || !a.ExpiresAt.Equal(t0.Add(14*24*time.Hour)) {
		t.Errorf("stale with no days left %+v, want the default grace", a)
	}
}
