package dashboard_test

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/user"
)

// fakeReady serves findings as a preflight.Checker would, and counts the
// runs it was asked for.
type fakeReady struct {
	report preflight.Report
	has    bool
	fresh  int
	next   []preflight.Finding // what a fresh run finds
	err    error
}

func (f *fakeReady) Check(_ context.Context, fresh bool) (preflight.Report, error) {
	if f.err != nil {
		return preflight.Report{}, f.err
	}
	if fresh || !f.has {
		f.fresh++
		f.report, f.has = preflight.Report{Findings: f.next, At: t0.Add(time.Hour)}, true
	}
	return f.report, nil
}

func (f *fakeReady) Last() (preflight.Report, bool) { return f.report, f.has }

type fakeUpdates struct{ result update.Result }

func (f fakeUpdates) Result() update.Result { return f.result }

// fakeBackup refuses, or writes body and then fails.
type fakeBackup struct {
	err      error
	body     string
	estimate backup.Estimate
}

func (f fakeBackup) Write(_ context.Context, w io.Writer, _ backup.WriteOptions) (backup.Summary, error) {
	if f.body != "" {
		_, _ = io.WriteString(w, f.body)
	}
	return backup.Summary{}, f.err
}

func (f fakeBackup) Estimate(context.Context) (backup.Estimate, error) { return f.estimate, nil }

const hostileValue = `<script>alert(1)</script>`

var (
	reInlineScript = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	reForeignAsset = regexp.MustCompile(`<(?:link|script|img|source|iframe)[^>]+(?:href|src)="(?:https?:)?//`)
	reStyleAttr    = regexp.MustCompile(`\sstyle\s*=`)
	reStyleBlock   = regexp.MustCompile(`(?i)<style`)
)

// strictMarkup fails the test unless body is a page the strict policy lets run:
// nothing inline, every asset from this origin, and no hostileValue value
// unescaped.
func strictMarkup(t *testing.T, what, body string) {
	t.Helper()
	if reStyleAttr.MatchString(body) || reStyleBlock.MatchString(body) {
		t.Errorf("%s has an inline style", what)
	}
	for _, m := range reInlineScript.FindAllStringSubmatch(body, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("%s has an inline script: %q", what, m[1])
		}
	}
	if m := reForeignAsset.FindString(body); m != "" {
		t.Errorf("%s loads an asset from another origin: %s", what, m)
	}
	if strings.Contains(body, hostileValue) || strings.Contains(body, "<script>alert") {
		t.Errorf("%s shows a hostileValue value unescaped", what)
	}
	if strings.Contains(body, "<td>") || strings.Contains(body, "<td class=\"num\">") {
		t.Errorf("%s has a table cell without a data-label", what)
	}
}

func TestOverviewNewInstance(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	r := b.get("/")
	strictMarkup(t, "the overview", r.body)
	for _, want := range []string{
		`<span class="ja" lang="ja" aria-hidden="true">概要</span>`,
		"Overview <b>of art.test</b>",
		"Getting started", `href="/people/invite"`, "Invite a person",
		`value="https://mcp.art.test/"`, "claude mcp add --transport http kazari https://mcp.art.test/",
		"export KAZARI_URL=https://api.art.test\n", "Publish a first page",
		"One admin", `href="/people"`,
		`aria-current="page">Overview`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the overview of a new instance lacks %q", want)
		}
	}
	if strings.Contains(r.body, `tone-ok">Done`) {
		t.Errorf("a step is done on an instance nobody has used")
	}
	// The sign-in that opened this page is the activity so far.
	if !strings.Contains(r.body, "Recent activity") || !strings.Contains(r.body, "boss") {
		t.Errorf("the overview shows no recent activity")
	}

	// The steps tick off as they are taken.
	e.person("ana", "")
	e.publish("ana", "report")
	r = b.get("/")
	if strings.Contains(r.body, "Getting started") {
		t.Errorf("the getting-started list outlived the first page")
	}
}

func TestOverviewStepsDone(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	b := e.signedIn("boss")
	r := b.get("/")
	if !strings.Contains(r.body, "Getting started") || strings.Count(r.body, `tone-ok">Done`) != 1 || !strings.Contains(r.body, "Invite another") {
		t.Errorf("with a second person and no page, one step is done: %d", strings.Count(r.body, `tone-ok">Done`))
	}
}

func TestOverviewWithoutLocalAccounts(t *testing.T) {
	e := newEnv(t, func(_ *dashboard.Deps, o *dashboard.Options) { o.LocalAccounts = false })
	e.person("boss", user.RoleAdmin)
	r := e.signedIn("boss").get("/")
	if !strings.Contains(r.body, "Add a person") || strings.Contains(r.body, "/people/invite") {
		t.Errorf("an instance with an identity provider offers invites: %s", r.body)
	}
}

func TestOverviewCounts(t *testing.T) {
	failing := scheduler.New(scheduler.Options{},
		scheduler.Job{Name: "gc", Run: func(context.Context) error { return errors.New("disk said no " + hostileValue) }},
		scheduler.Job{Name: "sweep", Every: time.Hour, Run: func(context.Context) error { return nil }})
	_ = failing.Trigger(context.Background(), "gc")
	ready := &fakeReady{has: true, report: preflight.Report{At: t0, Findings: []preflight.Finding{
		{Check: "dns", Status: preflight.Fail, Detail: "the wildcard record is missing " + hostileValue + "; add it at your DNS provider"},
		{Check: "config", Status: preflight.OK, Detail: "valid"},
	}}}
	e := newEnv(t, func(d *dashboard.Deps, o *dashboard.Options) {
		d.Jobs, d.Ready = failing, ready
		d.Updates = fakeUpdates{update.Result{Latest: "9.9.9", Newer: true, CheckedAt: t0}}
		o.TLSMode = "acme"
		o.CertNotAfter = func() time.Time { return t0.Add(5 * 24 * time.Hour) }
		o.DiskUsage = func(string) (uint64, uint64, error) { return 5 << 30, 100 << 30, nil }
		o.Build.Version = "0.8.0"
	})
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("gone", "")
	if _, _, _, err := e.users.Invite(asAdmin(), user.InviteRequest{Handle: "invitee"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "gone", user.UpdateUserRequest{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	e.publish("ana", "report")
	e.publish("ana", "notes")
	if err := e.artifacts.Delete(context.Background(), artifact.Actor{Handle: "ana", CanDelete: true}, "notes", false); err != nil {
		t.Fatal(err)
	}
	// One byte of quota, long since passed.
	if _, err := e.users.UpdateUser(asAdmin(), "ana", user.UpdateUserRequest{QuotaBytes: ptr(int64(1))}); err != nil {
		t.Fatal(err)
	}

	r := e.signedIn("boss").get("/")
	strictMarkup(t, "the overview", r.body)
	for _, want := range []string{
		"2 active, 1 invited, 1 disabled", "1 admin<",
		"1 live, 0 stale, 0 unpublished, 1 in the bin", "1 public, 0 unlisted, 0 for members",
		"5 GiB free of 100 GiB", `value="95"`,
		"5 days", "mode <span class=\"mono\">acme</span>",
		`tone-danger">Job failed`, `href="/system#jobs"`,
		`tone-warn">Certificate`, "expires in 5 days",
		`tone-warn">Disk space`,
		`tone-warn">Quota`, `href="/people/ana"`,
		`tone-note">One admin`,
		`tone-danger">Health check failed`, "add it at your DNS provider.",
		`tone-note">New version`, "Kazari 9.9.9 is out. This instance runs 0.8.0.", `href="/system#about"`,
		`tone-danger">Failed`, `href="/activity"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the overview lacks %q", want)
		}
	}
	if strings.Contains(r.body, "Getting started") || strings.Contains(r.body, "Nothing needs attention") {
		t.Errorf("an instance in use shows the new instance's page")
	}
}

func TestOverviewCalm(t *testing.T) {
	e := newEnv(t, func(d *dashboard.Deps, o *dashboard.Options) {
		d.Jobs, d.Audit = nil, nil
		o.DiskUsage = func(string) (uint64, uint64, error) { return 0, 0, errors.New("unsupported") }
	})
	e.person("boss", user.RoleAdmin)
	e.person("second", user.RoleAdmin)
	e.publish("boss", "report")
	r := e.signedIn("boss").get("/")
	strictMarkup(t, "the overview", r.body)
	if !strings.Contains(r.body, "Nothing needs attention right now.") {
		t.Errorf("a calm instance lists something: %s", r.body)
	}
	for _, absent := range []string{`id="jobs"`, "free of", "<meter"} {
		if strings.Contains(r.body, absent) {
			t.Errorf("the overview shows %q without the data for it", absent)
		}
	}
	if !strings.Contains(r.body, "No certificate is served yet") {
		t.Errorf("the overview does not say that no certificate is served")
	}
}

func TestOverviewBehindProxy(t *testing.T) {
	e := newEnv(t, func(_ *dashboard.Deps, o *dashboard.Options) { o.TLSMode = "proxy" })
	e.person("boss", user.RoleAdmin)
	r := e.signedIn("boss").get("/")
	if !strings.Contains(r.body, "A proxy in front of Kazari holds the certificate.") {
		t.Errorf("the overview does not say a proxy holds the certificate")
	}
}
