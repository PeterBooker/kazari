package dashboard_test

import (
	"context"
	"errors"
	"io"
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
	// estimates, when set, counts the calls of Estimate, which measures
	// the blob directory file by file.
	estimates *int
}

func (f fakeBackup) Write(_ context.Context, w io.Writer, _ backup.WriteOptions) (backup.Summary, error) {
	if f.body != "" {
		_, _ = io.WriteString(w, f.body)
	}
	return backup.Summary{}, f.err
}

func (f fakeBackup) Estimate(context.Context) (backup.Estimate, error) {
	if f.estimates != nil {
		*f.estimates++
	}
	return f.estimate, nil
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
		"Getting started", `href="/invites/new"`, "Invite a person",
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

	// The steps tick off as they are taken, and the list stays until an
	// agent has connected too.
	e.person("ana", "")
	e.publish("ana", "report")
	r = b.get("/")
	if !strings.Contains(r.body, "Getting started") || strings.Count(r.body, `tone-ok">Done`) != 2 {
		t.Errorf("with a person and a page but no agent, two steps are done: %d", strings.Count(r.body, `tone-ok">Done`))
	}
	if _, key, err := e.users.IssueKey(context.Background(), user.IssueKeyRequest{Handle: "ana"}); err != nil {
		t.Fatal(err)
	} else if _, err := e.users.Authenticate(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	r = b.get("/")
	if strings.Contains(r.body, "Getting started") || !strings.Contains(r.body, `value="https://mcp.art.test/"`) {
		t.Errorf("once every step is done the list should go and the connector address stay")
	}
}

// An admin puts the list away for good, and it stays away in the next
// session; the connector address stays.
func TestOverviewStartDismissed(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("other", user.RoleAdmin)
	b := e.signedIn("boss")
	if r := b.get("/"); !strings.Contains(r.body, `action="/start/dismiss"`) {
		t.Fatalf("the list offers no way to put it away: %s", r.body)
	}
	r := b.submit("/start/dismiss", nil)
	strictMarkup(t, "the overview", r.body)
	if strings.Contains(r.body, "Getting started") || !strings.Contains(r.body, "The getting-started list is put away.") ||
		!strings.Contains(r.body, `value="https://mcp.art.test/"`) {
		t.Errorf("after putting the list away: %s", r.body)
	}
	if r := e.signedIn("boss").get("/"); strings.Contains(r.body, "Getting started") {
		t.Error("the list is back in the next session")
	}
	if r := e.signedIn("other").get("/"); !strings.Contains(r.body, "Getting started") {
		t.Error("one admin put the list away for another")
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
	if !strings.Contains(r.body, "Add a person") || strings.Contains(r.body, "/invites/new") {
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
	if _, err := e.users.UpdateUser(asAdmin(), "gone", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	e.publish("ana", "report")
	e.publish("ana", "notes")
	if err := e.artifacts.Delete(context.Background(), artifact.Actor{Handle: "ana", CanDelete: true}, "notes", false); err != nil {
		t.Fatal(err)
	}
	// One byte of quota, long since passed.
	if _, err := e.users.UpdateUser(asAdmin(), "ana", user.UpdateUserRequest{QuotaBytes: new(int64(1))}); err != nil {
		t.Fatal(err)
	}

	r := e.signedIn("boss").get("/")
	strictMarkup(t, "the overview", r.body)
	for _, want := range []string{
		"2 active, 1 invited, 1 disabled", "1 admin<",
		"1 live, 0 stale, 0 unpublished, 1 in the bin", "1 public, 0 unlisted, 0 members only",
		"5 GiB free of 100 GiB", `value="95"`,
		"5 days", "mode <span class=\"mono\">acme</span>",
		`tone-danger">Job failed`, `href="/system#jobs"`,
		`tone-warn">Certificate`, "expires in 5 days",
		`tone-warn">Disk space`,
		`tone-warn">Quota`, `href="/people/ana"`,
		`tone-note">One admin`,
		`tone-danger">Health check failed`, "add it at your DNS provider.",
		`tone-note">New version`, "Kazari 9.9.9 is out. This instance runs 0.8.0.", `href="/system#about"`,
		`href="/activity"`, "counted against quotas: the content, and 4 KiB for each file",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the overview lacks %q", want)
		}
	}
	if strings.Contains(r.body, "Nothing needs attention") {
		t.Errorf("an instance with trouble says nothing needs attention")
	}
	if strings.Contains(r.body, `id="jobs"`) {
		t.Errorf("the overview lists the jobs; a failed one is under Needs attention")
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

// An admin who was invited and has not set a password cannot let anyone
// back in, so the instance still has a single admin.
func TestOverviewSingleAdminCountsWhoCanSignIn(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	_, _, token, err := e.users.Invite(asAdmin(), user.InviteRequest{Handle: "second", Role: user.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	b := e.signedIn("boss")
	if r := b.get("/"); !strings.Contains(r.body, "This instance has a single admin") {
		t.Errorf("with a second admin who is only invited, the overview does not warn: %s", r.body)
	}
	if _, err := e.users.Redeem(context.Background(), token, user.RedeemRequest{Password: password}); err != nil {
		t.Fatal(err)
	}
	if r := b.get("/"); strings.Contains(r.body, "This instance has a single admin") {
		t.Errorf("with two admins who can sign in, the overview warns of one: %s", r.body)
	}
}
