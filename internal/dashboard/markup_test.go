package dashboard_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/user"
)

// hostileValues are names a person, a client or a publisher can choose,
// each of which would run or break out of an attribute if a page wrote it
// as it is.
const (
	hostileValue = `<script>alert(1)</script>`
	hostileBreak = `"><img src=x onerror=alert(1)>`
)

var (
	reScriptTag    = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	reInlineScript = regexp.MustCompile(`(?is)<script[^>]*>(.*?)</script>`)
	reMeter        = regexp.MustCompile(`<meter\b[^>]*>`)
	reForeignAsset = regexp.MustCompile(`(?i)<(?:link|script|img|source|iframe)[^>]+(?:href|src)="(?:https?:)?//`)
)

// strictMarkup fails the test unless body is a page of the dashboard that
// the strict policy lets run: the theme's stylesheet, nothing inline, every
// asset from this origin, no hostile value unescaped, and every table cell
// labelled for the narrow layout, and every meter named. Every group of
// pages is checked with it.
func strictMarkup(t *testing.T, what, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, bad := range []string{" style=", "<style", "javascript:", " onclick="} {
		if strings.Contains(lower, bad) {
			t.Errorf("%s holds %q", what, bad)
		}
	}
	for _, tag := range reScriptTag.FindAllString(body, -1) {
		if !strings.Contains(tag, ` src="/assets/`) {
			t.Errorf("%s has a script that is not the theme's own: %s", what, tag)
		}
	}
	for _, m := range reInlineScript.FindAllStringSubmatch(body, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("%s has an inline script: %q", what, m[1])
		}
	}
	if m := reForeignAsset.FindString(body); m != "" {
		t.Errorf("%s loads an asset from another origin: %s", what, m)
	}
	if !strings.Contains(body, `<link rel="stylesheet" href="/assets/theme.`) {
		t.Errorf("%s does not load the theme's stylesheet", what)
	}
	if strings.Contains(body, hostileValue) || strings.Contains(body, hostileBreak) || strings.Contains(lower, "<img src=x") {
		t.Errorf("%s shows a hostile value unescaped", what)
	}
	if strings.Contains(body, "<td>") || strings.Contains(body, `<td class="num">`) {
		t.Errorf("%s has a table cell without a data-label", what)
	}
	for _, tag := range reMeter.FindAllString(body, -1) {
		if !strings.Contains(tag, ` aria-label="`) {
			t.Errorf("%s has a meter without a name: %s", what, tag)
		}
	}
}

var (
	reAnchor = regexp.MustCompile(`<a\b[^>]*\shref="([^"]*)"`)
	reForm   = regexp.MustCompile(`<form\b[^>]*>`)
	reAttr   = regexp.MustCompile(`\s(method|action)="([^"]*)"`)
)

// target is somewhere a page sends its reader: a link, or a form by its
// method.
type target struct{ method, path string }

// targets returns every address of the dashboard itself that body links to
// or posts to.
func targets(body string) []target {
	var out []target
	add := func(method, raw string) {
		raw = html.UnescapeString(raw)
		if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
			return
		}
		if u, err := url.Parse(raw); err == nil {
			u.Fragment = ""
			out = append(out, target{method, u.String()})
		}
	}
	for _, m := range reAnchor.FindAllStringSubmatch(body, -1) {
		add(http.MethodGet, m[1])
	}
	for _, tag := range reForm.FindAllString(body, -1) {
		method, action := http.MethodGet, ""
		for _, a := range reAttr.FindAllStringSubmatch(tag, -1) {
			if a[1] == "method" {
				method = strings.ToUpper(a[2])
			} else {
				action = a[2]
			}
		}
		add(method, action)
	}
	return out
}

// Every link and every form of the dashboard leads to a route that answers
// its method. A link is followed, and so are the links and forms of the
// page it opens; a form is sent without its token, which the route's guard
// refuses with 403 and no route answers with 404.
func TestEveryLinkLeadsSomewhere(t *testing.T) {
	ready := &fakeReady{has: true, report: preflight.Report{At: t0, Findings: []preflight.Finding{
		{Check: "dns", Status: preflight.Fail, Detail: "the wildcard record is missing"},
	}}}
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) {
		d.Ready = ready
		d.Backup = fakeBackup{estimate: backup.Estimate{DatabaseBytes: 1 << 20}}
		d.Settings = func() map[string]any { return map[string]any{"base_domain": "art.test"} }
	})
	ctx := context.Background()
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	if _, _, _, err := e.users.Invite(asAdmin(), user.InviteRequest{Handle: "ben"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.users.IssueLink(asAdmin(), "ana", user.PurposeReset, false); err != nil {
		t.Fatal(err)
	}
	if err := e.users.LinkIdentity(asAdmin(), "ana", "https://idp.example", "subject-1", "ana@idp.example"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.users.IssueKey(asAdmin(), user.IssueKeyRequest{Handle: "ana", Label: "laptop"}); err != nil {
		t.Fatal(err)
	}
	e.peopleConnectApp("ana", "Agent")
	e.publish("ana", "report")
	e.addVersion("ana", "report", "<h1>two</h1>")
	ana := artifact.Actor{Handle: "ana"}
	if _, _, err := e.shares.CreateShareLink(ctx, ana, "report", "for the board", time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	e.publish("ana", "old")
	if err := e.artifacts.Delete(ctx, ana, "old", false); err != nil {
		t.Fatal(err)
	}
	e.publish("boss", "notes")

	b := e.signedIn("boss")
	start := []string{"/", "/people", "/people/ana", "/people/ben", "/pages", "/pages/report", "/pages/old", "/bin", "/activity", "/system"}
	seen := map[target]bool{}
	checked := 0
	check := func(from string, body string, follow bool) (next []string) {
		for _, tg := range targets(body) {
			if seen[tg] {
				continue
			}
			seen[tg] = true
			checked++
			switch tg.method {
			case http.MethodGet:
				r := b.navigate(tg.path)
				if r.StatusCode == http.StatusNotFound || r.StatusCode == http.StatusMethodNotAllowed {
					t.Errorf("%s links to %s, which answers %d", from, tg.path, r.StatusCode)
				}
				if follow && r.StatusCode == http.StatusOK && strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
					next = append(next, tg.path)
				}
			case http.MethodPost:
				r := b.post(tg.path, url.Values{"form_token": {"not-the-token"}})
				if r.StatusCode != http.StatusForbidden {
					t.Errorf("%s posts to %s, which answers %d without the form's token, want the 403 of a route that is there", from, tg.path, r.StatusCode)
				}
			default:
				t.Errorf("%s has a form with the method %s", from, tg.method)
			}
		}
		return next
	}
	var second []string
	for _, path := range start {
		second = append(second, check(path, b.get(path).body, true)...)
	}
	for _, path := range second {
		check(path, b.get(path).body, false)
	}
	// The pages above hold far more than this; fewer means the walk has
	// stopped reading them.
	if checked < 60 {
		t.Errorf("the walk checked %d links and forms", checked)
	}
	for _, want := range []target{
		{http.MethodGet, "/invites/new"}, {http.MethodGet, "/bin"}, {http.MethodGet, "/people/ana/handover"},
		{http.MethodPost, "/people/ana/handover"}, {http.MethodPost, "/people/ana/identities/unlink"},
		{http.MethodPost, "/system/backup"}, {http.MethodPost, "/session/signout"},
	} {
		if !seen[want] {
			t.Errorf("the walk never met %s %s", want.method, want.path)
		}
	}
}
