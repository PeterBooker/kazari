package dashboard_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/user"
)

// record writes events to the log as the services do.
func (e *env) record(events ...audit.Event) {
	e.t.Helper()
	for _, ev := range events {
		if ev.At.IsZero() {
			ev.At = e.clock.Now()
		}
		if err := e.audit.Record(context.Background(), ev); err != nil {
			e.t.Fatal(err)
		}
	}
}

// activityEnv is an instance whose log holds one event of each kind beside
// what creating the admin recorded, each about its own page so a test can
// tell which rows a filter kept.
func activityEnv(t *testing.T) (*env, *browser) {
	t.Helper()
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.clock.Advance(time.Minute)
	e.record(
		audit.Event{Actor: "ana", Action: audit.ArtifactUpdate, Slug: "kind-pages", Details: map[string]string{"version": "4"}},
		audit.Event{Actor: "ana", Action: audit.ShareLinkCreate, Slug: "kind-links", Details: map[string]string{"prefix": "abcdefgh"}},
		audit.Event{Actor: "ben", Action: audit.UserUpdate, Slug: "kind-people", Details: map[string]string{"handle": "ana", "disabled": "true"}},
		audit.Event{Actor: "ben", Action: audit.SessionRevoke, Slug: "kind-people-2", Details: map[string]string{"user": "ana", "sessions": "2"}},
		audit.Event{Actor: "ben", Action: audit.KeyIssue, Slug: "kind-keys", Details: map[string]string{"handle": "ben", "prefix": "abcdefgh"}},
		audit.Event{Actor: "ana", Action: "oauth.grant", Slug: "kind-apps", Details: map[string]string{"client_id": "dcr_abc"}},
		audit.Event{Actor: "scheduler", Action: audit.BackupCreate, Slug: "kind-operations"},
	)
	b := e.signedIn("boss")
	b.get("/activity")
	return e, b
}

// The actions activityEnv recorded, one of each kind and two about people.
var activityKindActions = []string{audit.ArtifactUpdate, audit.ShareLinkCreate, audit.UserUpdate, audit.SessionRevoke, audit.KeyIssue, "oauth.grant", audit.BackupCreate}

// rowsOf lists which of actions have a row on the page, by the raw action
// each row shows.
func rowsOf(r response, actions []string) string {
	var out []string
	for _, a := range actions {
		if strings.Contains(r.body, `<tr data-action="`+a+`">`) {
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}

// rowsAbout lists which of slugs a row on the page names.
func rowsAbout(r response, slugs []string) string {
	var out []string
	for _, s := range slugs {
		// The filter form echoes a slug too; a row links it.
		if strings.Contains(r.body, ">"+s+"</a>") {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func TestActivityFilters(t *testing.T) {
	_, b := activityEnv(t)
	r := b.get("/activity")
	for _, want := range []string{
		`<span class="ja" lang="ja" aria-hidden="true">履歴</span>`,
		"Nothing prunes this log", "a backup includes them",
		`<a href="/people/ana" class="mono">ana</a> published version 4 of <a href="/pages/kind-pages" class="mono">kind-pages</a>`,
		`<tr data-action="artifact.update">`,
		`<a href="/people/ben" class="mono">ben</a> disabled <a href="/people/ana" class="mono">ana</a>`,
		"the scheduler took a backup", "signed in to the dashboard",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	tests := []struct{ query, want string }{
		{"", strings.Join(activityKindActions, " ")},
		{"kind=pages", "artifact.update"},
		{"kind=links", "share_link.create"},
		{"kind=people", "user.update session.revoke"},
		{"kind=keys", "key.issue"},
		{"kind=apps", "oauth.grant"},
		{"kind=operations", "backup.create"},
		{"kind=no-such-kind", strings.Join(activityKindActions, " ")},
		{"person=ana", "artifact.update share_link.create oauth.grant"},
		{"person=ben&kind=keys", "key.issue"},
		{"person=scheduler", "backup.create"},
		{"slug=kind-links", "share_link.create"},
		{"slug=kind-links&person=ben", ""},
		{"person=nobody", ""},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			r := b.get("/activity?" + tt.query)
			if got := rowsOf(r, activityKindActions); got != tt.want {
				t.Errorf("rows of %q, want %q", got, tt.want)
			}
			if tt.want == "" && !strings.Contains(r.body, "No event matches this filter.") {
				t.Error("an empty result does not say so")
			}
		})
	}
	// The filter stays in the form and travels with the export.
	r = b.get("/activity?person=ana&kind=apps&from=2026-11-01&to=2026-11-30&slug=kind-apps")
	for _, want := range []string{
		`<option value="ana" selected>ana</option>`, `list="f-slug-pages"`, `value="kind-apps"`, `<option value="apps" selected>`, `name="from" type="date" value="2026-11-01"`,
		`name="to" type="date" value="2026-11-30"`, `<input type="hidden" name="person" value="ana">`, `<input type="hidden" name="kind" value="apps">`,
		`<input type="hidden" name="to" value="2026-11-30">`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the filter form lacks %q", want)
		}
	}
}

// The date range is whole days in UTC, the last one included.
func TestActivityDateRange(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	days := []string{"day-before", "day-start", "day-end", "day-after"}
	e.record(
		audit.Event{At: at("2026-11-19T23:59:59Z"), Actor: "ana", Action: audit.ArtifactCreate, Slug: "day-before"},
		audit.Event{At: at("2026-11-20T00:00:00Z"), Actor: "ana", Action: audit.ArtifactCreate, Slug: "day-start"},
		audit.Event{At: at("2026-11-20T23:59:59Z"), Actor: "ana", Action: audit.ArtifactCreate, Slug: "day-end"},
		audit.Event{At: at("2026-11-21T00:00:00Z"), Actor: "ana", Action: audit.ArtifactCreate, Slug: "day-after"},
	)
	b := e.signedIn("boss")
	for query, want := range map[string]string{
		"from=2026-11-20&to=2026-11-20": "day-start day-end",
		"from=2026-11-20":               "day-start day-end day-after",
		"to=2026-11-20":                 "day-before day-start day-end",
		"to=2026-11-19":                 "day-before",
		"from=2026-11-21&to=2026-11-19": "",
	} {
		if got := rowsAbout(b.get("/activity?person=ana&"+query), days); got != want {
			t.Errorf("%s: rows about %q, want %q", query, got, want)
		}
	}
	// A date that is none is said so beside its field, and left out.
	r := b.get("/activity?person=ana&from=yesterday&to=2026-11-20")
	if !strings.Contains(r.body, "Use a date such as 2026-11-21.") || !strings.Contains(r.body, `aria-describedby="f-from-err"`) ||
		rowsAbout(r, days) != "day-before day-start day-end" {
		t.Errorf("a bad date: rows about %q: %s", rowsAbout(r, days), r.body)
	}
}

var olderLink = regexp.MustCompile(`href="(/activity\?[^"]*cursor=[^"]*)" rel="next"`)

func TestActivityCursor(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	for i := range 120 {
		e.clock.Advance(time.Second)
		e.record(audit.Event{Actor: "ana", Action: audit.ArtifactCreate, Slug: fmt.Sprintf("bulk-%03d", i)})
	}
	e.record(audit.Event{Actor: "ben", Action: audit.ArtifactCreate, Slug: "bens-page"})
	b := e.signedIn("boss")
	rows := func(r response) int { return strings.Count(r.body, `href="/pages/bulk-`) }

	r := b.get("/activity?person=ana")
	if rows(r) != 50 || !strings.Contains(r.body, "bulk-119") || strings.Contains(r.body, "First page") {
		t.Fatalf("first page: %d rows", rows(r))
	}
	seen := rows(r)
	for page := 2; ; page++ {
		m := olderLink.FindStringSubmatch(r.body)
		if m == nil {
			break
		}
		addr := strings.ReplaceAll(m[1], "&amp;", "&")
		if u, _ := url.Parse(addr); u.Query().Get("person") != "ana" {
			t.Fatalf("the link to page %d dropped the filter: %s", page, addr)
		}
		r = b.get(addr)
		if !strings.Contains(r.body, `href="/activity?person=ana">First page</a>`) {
			t.Errorf("page %d has no way back to the first", page)
		}
		if strings.Contains(r.body, "bens-page") {
			t.Errorf("page %d shows another person's event", page)
		}
		seen += rows(r)
		if page > 5 {
			t.Fatal("the pages never end")
		}
	}
	if seen != 120 || !strings.Contains(r.body, "bulk-000") {
		t.Errorf("the pages hold %d rows in all, want 120", seen)
	}
	if r := b.navigate("/activity?cursor=not-a-cursor"); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a made-up cursor: status %d, want 422", r.StatusCode)
	}
}

func TestActivityEmpty(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	if r := b.get("/activity?person=nobody-at-all"); !strings.Contains(r.body, "No event matches this filter.") || !strings.Contains(r.body, "Show all activity") {
		t.Errorf("an empty filter result: %s", r.body)
	}
}

func TestActivityExport(t *testing.T) {
	e, b := activityEnv(t)
	e.record(
		audit.Event{Actor: "=HYPERLINK(\"http://evil.example\")", Action: "+cmd", Slug: "@slug", Details: map[string]string{"note": "-1+1"}},
		audit.Event{Actor: "ana", Action: audit.ArtifactImport, Slug: "kind-pages", Details: map[string]string{"source": "https://example.com/a,b", "version": "1"}},
	)
	r := b.post("/activity/export", nil)
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "text/csv; charset=utf-8" ||
		r.Header.Get("Content-Disposition") != `attachment; filename="kazari-activity-2026-11-21.csv"` || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("export: status %d, headers %v", r.StatusCode, r.Header)
	}
	rows, err := csv.NewReader(strings.NewReader(r.body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rows[0], "|"); got != "time|actor|action|slug|what happened|details" {
		t.Errorf("header = %s", got)
	}
	byslug := map[string][]string{}
	for _, row := range rows[1:] {
		if len(row) != 6 {
			t.Fatalf("a row of %d cells: %v", len(row), row)
		}
		if _, err := time.Parse(time.RFC3339, row[0]); err != nil {
			t.Errorf("time %q: %v", row[0], err)
		}
		for _, cell := range row {
			if cell != "" && strings.ContainsAny(cell[:1], "=+-@\t\r") {
				t.Errorf("a cell a spreadsheet would run: %q", cell)
			}
		}
		byslug[row[3]+" "+row[2]] = row
	}
	if row := byslug["kind-pages artifact.update"]; row == nil || row[1] != "ana" || row[4] != "ana published version 4 of kind-pages" || row[5] != `version="4"` {
		t.Errorf("the row of a publish = %q", row)
	}
	if row := byslug["kind-pages artifact.import"]; row == nil || row[4] != "ana imported kind-pages from https://example.com/a,b" {
		t.Errorf("the row of an import = %q", row)
	}
	if row := byslug["'@slug '+cmd"]; row == nil || row[1] != `'=HYPERLINK("http://evil.example")` || row[5] != `note="-1+1"` {
		t.Errorf("the row of the formulas = %q", row)
	}
	// Every event is there: what the page lists and what creating the admin
	// and signing in recorded.
	all := len(rows) - 1
	if all < 9 {
		t.Errorf("the export holds %d rows", all)
	}

	// The export is itself recorded, with what was asked for.
	ev := e.events(audit.AuditExport)
	if len(ev) != 1 || ev[0].Actor != "boss" || ev[0].Details["rows"] != fmt.Sprint(all) {
		t.Fatalf("export audit = %+v, want one by boss of %d rows", ev, all)
	}

	// With the page's filter.
	r = b.post("/activity/export", url.Values{"person": {"ben"}, "kind": {"people"}, "from": {"2026-11-21"}, "to": {"2026-11-21"}})
	rows, err = csv.NewReader(strings.NewReader(r.body)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("filtered export: %d rows, %v: %s", len(rows), err, r.body)
	}
	ev = e.events(audit.AuditExport)
	if len(ev) != 2 || ev[0].Details["rows"] != "2" || ev[0].Details["person"] != "ben" || ev[0].Details["kind"] != "people" ||
		ev[0].Details["from"] != "2026-11-21" || ev[0].Details["to"] != "2026-11-21" {
		t.Errorf("filtered export audit = %+v", ev[0])
	}
	if r := b.get("/activity?kind=operations"); !strings.Contains(r.body, "exported the activity log, 2 rows") {
		t.Errorf("the export is not on the page: %s", r.body)
	}

	// Asking is a change: it needs the form token and the dashboard's origin.
	if r := b.navigate("/activity/export"); r.StatusCode == http.StatusOK {
		t.Errorf("GET /activity/export: status %d", r.StatusCode)
	}
	if r := b.postRaw("/activity/export", url.Values{}, e.admin.URL); r.StatusCode != http.StatusForbidden {
		t.Errorf("an export without the form token: status %d, want 403", r.StatusCode)
	}
	if r := b.post("/activity/export", url.Values{"from": {"yesterday"}}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an export with a bad date: status %d, want 422", r.StatusCode)
	}
	if got := len(e.events(audit.AuditExport)); got != 2 {
		t.Errorf("refused exports were recorded: %d events", got)
	}
}

// An export walks every page of the log, not the first.
func TestActivityExportEveryPage(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	for i := range 1500 {
		e.record(audit.Event{At: t0.Add(time.Duration(i) * time.Millisecond), Actor: "ana", Action: audit.ArtifactCreate, Slug: fmt.Sprintf("bulk-%04d", i)})
	}
	b := e.signedIn("boss")
	b.get("/activity")
	r := b.post("/activity/export", url.Values{"person": {"ana"}})
	rows, err := csv.NewReader(strings.NewReader(r.body)).ReadAll()
	if err != nil || len(rows) != 1501 {
		t.Fatalf("export: %d rows, %v", len(rows), err)
	}
	if rows[1][3] != "bulk-1499" || rows[1500][3] != "bulk-0000" {
		t.Errorf("first row %v, last row %v", rows[1], rows[1500])
	}
}

func TestActivityMarkup(t *testing.T) {
	_, b := activityEnv(t)
	for _, path := range []string{"/activity", "/activity?kind=pages&person=ana", "/activity?person=nobody", "/activity?from=bad"} {
		strictMarkup(t, path, b.get(path).body)
	}
}

// The filter offers the people and pages there are, rather than asking for
// an exact handle or address, and the log shows no raw action names.
func TestActivityFilterChoices(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.publish("ana", "notes")
	e.record(audit.Event{Actor: "ana", Action: audit.ShareLinkCreate, Slug: "notes", Details: map[string]string{"prefix": "abcdefgh", "label": "the client"}})
	r := e.signedIn("boss").get("/activity")
	strictMarkup(t, "the activity", r.body)
	for _, want := range []string{`<option value="ana">ana</option>`, `<option value="notes"></option>`, "created share link", ", for the client"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the activity lacks %q", want)
		}
	}
	if strings.Contains(r.body, `<span class="mono soft">artifact.create</span>`) || strings.Contains(r.body, "<th>Action</th>") {
		t.Error("the activity shows raw action names")
	}
}
