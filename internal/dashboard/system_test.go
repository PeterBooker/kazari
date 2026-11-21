package dashboard_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestSystemPage(t *testing.T) {
	ready := &fakeReady{has: true, report: preflight.Report{At: t0, Findings: []preflight.Finding{
		{Check: "config", Status: preflight.OK, Detail: "the configuration is valid"},
		{Check: "dns", Status: preflight.Warn, Detail: "art.test resolves to another host " + hostileValue},
		{Check: "certificate", Status: preflight.Fail, Detail: "expired; renew it"},
		{Check: "dns token", Status: preflight.Skip, Detail: "not needed in proxy mode"},
	}}}
	e := newEnv(t, func(d *dashboard.Deps, o *dashboard.Options) {
		d.Ready = ready
		d.Updates = fakeUpdates{update.Result{Latest: "0.8.0", CheckedAt: t0}}
		d.Backup = fakeBackup{estimate: backup.Estimate{DatabaseBytes: 3 << 20, BlobBytes: 5 << 30, FreeBytes: 40 << 30}}
		d.Settings = func() map[string]any {
			return map[string]any{"base_domain": "art.test", "serve": map[string]any{"csp": hostileValue}}
		}
		o.Build.Version, o.Build.Commit, o.Build.Go = "0.8.0", "abc1234", "go1.27.4"
		o.TLSMode = "acme"
	})
	e.person("boss", user.RoleAdmin)
	r := e.signedIn("boss").get("/system")
	strictMarkup(t, "the system page", r.body)
	for _, want := range []string{
		`<span class="ja" lang="ja" aria-hidden="true">運用</span>`, `aria-current="page">System`,
		// Health: a word beside every colour.
		`tone-ok">Passed`, `tone-warn">Warning`, `tone-danger">Failed`, `tone-neutral">Skipped`, "expired; renew it",
		`action="/system/health/run"`, "can take a minute",
		// Jobs.
		`action="/system/jobs/sweep/run"`, "Moves stale pages to the bin", "15 minutes",
		// Settings.
		"edit the configuration file on the host and restart", `href="https://kazari.io/docs/configuration/"`, "base_domain",
		// About.
		"abc1234", `tone-ok">Up to date`, `href="https://github.com/PeterBooker/kazari/releases"`, `href="https://kazari.io/docs/"`,
		"https://mcp.art.test/", "An upgrade is done on the host",
		// Backup.
		`action="/system/backup"`, "Keep a backup as a secret", "hash of every password and key", "5 GiB of files",
		"leaves out the certificate", "kazari admin backup", "with the server stopped", "None taken through Kazari yet",
		// Diagnostics.
		`id="diagnostics-text"`, "kazari 0.8.0 (abc1234)", `data-copy-target="diagnostics-text"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the system page lacks %q", want)
		}
	}
	if n := strings.Count(r.body, "btn-primary"); n != 1 {
		t.Errorf("the system page has %d primary buttons, want one", n)
	}
}

// Every optional dependency missing leaves its section out and nothing
// else.
func TestSystemWithoutOptionalDependencies(t *testing.T) {
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) {
		d.Jobs, d.Ready, d.Backup, d.Updates, d.Settings, d.Audit = nil, nil, nil, nil, nil, nil
	})
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	r := b.get("/system")
	strictMarkup(t, "the system page", r.body)
	for _, absent := range []string{`id="health"`, `id="jobs"`, `id="backup"`, `id="settings"`} {
		if strings.Contains(r.body, absent) {
			t.Errorf("the system page shows %s without its dependency", absent)
		}
	}
	if !strings.Contains(r.body, "The update check is off") || !strings.Contains(r.body, "checks: not run yet") {
		t.Errorf("the system page does not say what is off: %s", r.body)
	}
	for _, path := range []string{"/system/health/run", "/system/jobs/gc/run", "/system/backup"} {
		if r := b.post(path, nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s without its dependency: status %d, want 404", path, r.StatusCode)
		}
	}
	strictMarkup(t, "the overview", b.get("/").body)
}

func TestHealthRunNow(t *testing.T) {
	ready := &fakeReady{next: []preflight.Finding{{Check: "disk", Status: preflight.OK, Detail: "61% free"}}}
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Ready = ready })
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	r := b.get("/system")
	if !strings.Contains(r.body, "The checks have not run since the server started") || ready.fresh != 0 {
		t.Fatalf("before a run (%d runs): %s", ready.fresh, r.body)
	}
	// Loading a page never runs the checks, and neither does a GET of the
	// action.
	if r := b.navigate("/system/health/run"); r.StatusCode == http.StatusOK || r.StatusCode == http.StatusSeeOther || ready.fresh != 0 {
		t.Fatalf("GET of the run: status %d, %d runs", r.StatusCode, ready.fresh)
	}
	p := b.post("/system/health/run", nil)
	if p.StatusCode != http.StatusSeeOther || p.Header.Get("Location") != "/system#health" {
		t.Fatalf("run: status %d to %q", p.StatusCode, p.Header.Get("Location"))
	}
	r = b.get("/system")
	if ready.fresh != 1 || !strings.Contains(r.body, "61% free") || !strings.Contains(r.body, "The checks ran. The results are below.") {
		t.Errorf("after a run (%d runs): %s", ready.fresh, r.body)
	}
	if r = b.get("/system"); strings.Contains(r.body, "The checks ran.") {
		t.Errorf("the confirmation line is shown twice")
	}

	ready.err = errors.New("resolver exploded")
	if p := b.post("/system/health/run", nil); p.StatusCode != http.StatusInternalServerError || strings.Contains(p.body, "resolver exploded") {
		t.Errorf("a run that cannot start: status %d: %s", p.StatusCode, p.body)
	}
}

func TestJobRunNow(t *testing.T) {
	var runs atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	jobs := scheduler.New(scheduler.Options{Now: func() time.Time { return t0 }},
		scheduler.Job{Name: "sweep", Every: 15 * time.Minute, Run: func(context.Context) error { runs.Add(1); return nil }},
		scheduler.Job{Name: "gc", Every: time.Hour, Run: func(context.Context) error { return errors.New("blob store said " + hostileValue) }},
		scheduler.Job{Name: "slow", Run: func(context.Context) error { close(entered); <-release; return nil }})
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Jobs = jobs })
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	b.get("/system")

	// No change on a GET.
	if r := b.navigate("/system/jobs/sweep/run"); r.StatusCode == http.StatusOK || r.StatusCode == http.StatusSeeOther || runs.Load() != 0 {
		t.Fatalf("GET of a run: status %d, %d runs", r.StatusCode, runs.Load())
	}
	// Nor from another origin, nor without the form token.
	if r := b.postRaw("/system/jobs/sweep/run", url.Values{"form_token": {b.token}}, "https://demo.art.test"); r.StatusCode != http.StatusForbidden || runs.Load() != 0 {
		t.Fatalf("a run posted from an artifact: status %d, %d runs", r.StatusCode, runs.Load())
	}
	if r := b.postRaw("/system/jobs/sweep/run", url.Values{}, e.admin.URL); r.StatusCode != http.StatusForbidden || runs.Load() != 0 {
		t.Fatalf("a run without the form token: status %d, %d runs", r.StatusCode, runs.Load())
	}

	r := b.submit("/system/jobs/sweep/run", nil)
	if runs.Load() != 1 || !strings.Contains(r.body, "The job ran.") {
		t.Errorf("run now: %d runs: %s", runs.Load(), r.body)
	}

	r = b.post("/system/jobs/gc/run", nil)
	if r.StatusCode != http.StatusInternalServerError || !strings.Contains(r.body, "The job failed") || !strings.Contains(r.body, "blob store said") {
		t.Errorf("a failing job: status %d: %s", r.StatusCode, r.body)
	}
	strictMarkup(t, "the failed job's page", r.body)
	r = b.get("/system")
	strictMarkup(t, "the system page", r.body)
	if !strings.Contains(r.body, `tone-danger">Failed`) || !strings.Contains(r.body, "blob store said") {
		t.Errorf("the jobs table does not show the last error: %s", r.body)
	}

	if r := b.post("/system/jobs/nope/run", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown job: status %d, want 404", r.StatusCode)
	}

	done := make(chan error, 1)
	go func() { done <- jobs.Trigger(context.Background(), "slow") }()
	<-entered
	if r := b.post("/system/jobs/slow/run", nil); r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "already running") {
		t.Errorf("a job that is running: status %d: %s", r.StatusCode, r.body)
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("the run in progress: %v", err)
	}
}

// The page shows the configuration as the server redacts it: every secret
// as ***, on the page and in the diagnostics alike.
func TestSettingsRedacted(t *testing.T) {
	const adminToken, clientSecret, dnsToken = "admin-token-0123456789abcdef0123456789", "oidc-client-secret-value", "dns-api-token-value"
	cfg := config.Default()
	cfg.BaseDomain, cfg.DataDir = "art.test", "/var/lib/kazari"
	cfg.Auth.AdminToken = adminToken
	cfg.TLS.DNS.APIToken = dnsToken
	cfg.Auth.OIDC = &config.OIDC{Issuer: "https://idp.example", ClientID: "kazari", ClientSecret: clientSecret}
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Settings = cfg.Redacted })
	e.person("boss", user.RoleAdmin)
	r := e.signedIn("boss").get("/system")
	strictMarkup(t, "the system page", r.body)
	for _, secret := range []string{adminToken, clientSecret, dnsToken} {
		if strings.Contains(r.body, secret) {
			t.Errorf("the system page shows the secret %q", secret)
		}
	}
	for _, want := range []string{
		"auth.admin_token", "auth.oidc.client_secret", "tls.dns.api_token", "set, hidden",
		"  auth.admin_token: ***\n", "  tls.dns.api_token: ***\n",
		"auth.oidc.issuer", "https://idp.example", "limits.max_version_bytes", "50MiB", "admin.update_check",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the system page lacks %q", want)
		}
	}
	// The sections come in the order of the reference file.
	order := []string{`<span class="mono">general</span>`, `<span class="mono">listen</span>`, `<span class="mono">tls</span>`,
		`<span class="mono">limits</span>`, `<span class="mono">auth</span>`, `<span class="mono">admin</span>`, `<span class="mono">telemetry</span>`}
	last := -1
	for _, name := range order {
		i := strings.Index(r.body, name)
		if i < 0 || i < last {
			t.Errorf("section %s is missing or out of order", name)
		}
		last = i
	}
	if strings.Contains(r.body, "<form") && strings.Contains(r.body, `name="base_domain"`) {
		t.Error("the settings can be edited")
	}
}

func TestSystemUpdateNotice(t *testing.T) {
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) {
		d.Updates = fakeUpdates{update.Result{Latest: "0.9.0", Newer: true, CheckedAt: t0}}
	})
	e.person("boss", user.RoleAdmin)
	r := e.signedIn("boss").get("/system")
	if !strings.Contains(r.body, `tone-note">New version`) || !strings.Contains(r.body, `<span class="mono">0.9.0</span> is out`) {
		t.Errorf("the system page does not announce the newer version: %s", r.body)
	}
	e = newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Updates = fakeUpdates{} })
	e.person("boss", user.RoleAdmin)
	if r := e.signedIn("boss").get("/system"); !strings.Contains(r.body, "No check has answered yet") {
		t.Errorf("the system page does not say the newest release is unknown")
	}
}

// The download is a real archive of the instance, without the
// certificate's private key, and is recorded with the admin's name.
func TestBackupDownload(t *testing.T) {
	var dataDir string
	e := newEnv(t, func(d *dashboard.Deps, o *dashboard.Options) {
		dataDir = o.DataDir
		// A second handle on the instance's database, as the copy needs.
		db, err := sqlite.Open(context.Background(), filepath.Join(dataDir, "kazari.db"), sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		tmp, certs := filepath.Join(dataDir, "tmp"), filepath.Join(dataDir, "certmagic")
		for _, dir := range []string{tmp, certs} {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(certs, "private.key"), []byte("the private key"), 0o600); err != nil {
			t.Fatal(err)
		}
		d.Backup = backup.New(db, backup.Options{
			DBPath: filepath.Join(dataDir, "kazari.db"), BlobDir: filepath.Join(dataDir, "blobs"), CertDir: certs, TempDir: tmp,
			Now: func() time.Time { return t0 }, Audit: d.Recorder,
		})
	})
	e.person("boss", user.RoleAdmin)
	e.publish("boss", "report")
	b := e.signedIn("boss")
	b.get("/system")

	if r := b.navigate("/system/backup"); r.StatusCode == http.StatusOK || len(e.events(audit.BackupCreate)) != 0 {
		t.Fatalf("GET of the download: status %d", r.StatusCode)
	}
	if r := b.postRaw("/system/backup", url.Values{"form_token": {b.token}}, "https://demo.art.test"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("a download posted from an artifact: status %d", r.StatusCode)
	}

	r := b.post("/system/backup", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d: %s", r.StatusCode, r.body)
	}
	for name, want := range map[string]string{
		"Content-Type":        "application/x-tar",
		"Content-Disposition": `attachment; filename="kazari-art.test-20261121-120000.tar"`,
		"Cache-Control":       "no-store",
	} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	names := map[string]bool{}
	tr := tar.NewReader(strings.NewReader(r.body))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
		if strings.HasPrefix(h.Name, "certmagic") {
			t.Errorf("the download holds %s", h.Name)
		}
	}
	if !names["kazari.db"] || len(names) < 2 {
		t.Errorf("archive entries = %v, want the database and the blobs", names)
	}
	if strings.Contains(r.body, "the private key") {
		t.Error("the download holds the certificate's private key")
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := backup.Extract(strings.NewReader(r.body), restored); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	db, err := sqlite.Open(context.Background(), filepath.Join(restored, "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("the restored database does not open: %v", err)
	}
	a, err := sqlite.NewArtifactStore(db).Get(context.Background(), "report")
	db.Close()
	if err != nil || a.OwnerHandle != "boss" {
		t.Errorf("the restored database lacks the page: %+v, %v", a, err)
	}

	ev := e.events(audit.BackupCreate)
	if len(ev) != 1 || ev[0].Actor != "boss" || ev[0].Details["certificates"] != "false" {
		t.Errorf("backup audit = %+v", ev)
	}
	page := b.get("/system")
	if !strings.Contains(page.body, `by <span class="mono">boss</span>`) || strings.Contains(page.body, "None taken through Kazari yet") {
		t.Errorf("the system page does not show the last backup: %s", page.body)
	}
}

func TestBackupRefused(t *testing.T) {
	bk := &fakeBackup{estimate: backup.Estimate{DatabaseBytes: 3 << 30, FreeBytes: 1 << 30}}
	e := newEnv(t, func(d *dashboard.Deps, _ *dashboard.Options) { d.Backup = bk })
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	b.get("/system")

	bk.err = backup.ErrRunning
	r := b.post("/system/backup", nil)
	if r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "A backup is already running") || r.Header.Get("Content-Disposition") != "" {
		t.Errorf("a second backup: status %d, disposition %q: %s", r.StatusCode, r.Header.Get("Content-Disposition"), r.body)
	}
	strictMarkup(t, "the refusal", r.body)

	bk.err = errors.Join(backup.ErrNoSpace, errors.New("/var/lib/kazari/tmp has 1 bytes free"))
	r = b.post("/system/backup", nil)
	if r.StatusCode != http.StatusInsufficientStorage || !strings.Contains(r.body, "needs up to 3 GiB") || !strings.Contains(r.body, "has 1 GiB free") ||
		strings.Contains(r.body, "/var/lib/kazari") {
		t.Errorf("too little space: status %d: %s", r.StatusCode, r.body)
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Errorf("the refusal is served as %q", r.Header.Get("Content-Type"))
	}

	// A failure once bytes have gone out cannot become a page: the transfer
	// breaks, so nobody keeps half an archive as a backup.
	bk.err, bk.body = errors.New("disk read failed"), strings.Repeat("x", 4096)
	req, _ := http.NewRequest(http.MethodPost, e.admin.URL+"/system/backup", strings.NewReader(url.Values{"form_token": {b.token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.admin.URL)
	resp, err := b.client.Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Error("a backup that failed mid-transfer ended as a complete download")
	}
}
