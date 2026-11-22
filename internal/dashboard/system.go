package dashboard

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/update"
)

var systemFuncs = template.FuncMap{
	"findingTone": findingTone,
	"findingWord": findingWord,
}

// findingTone and findingWord say a check's result twice, in a colour and
// in a word.
func findingTone(s preflight.Status) string {
	switch s {
	case preflight.OK:
		return "ok"
	case preflight.Warn:
		return "warn"
	case preflight.Fail:
		return "danger"
	}
	return "neutral"
}

func findingWord(s preflight.Status) string {
	switch s {
	case preflight.OK:
		return "Passed"
	case preflight.Warn:
		return "Warning"
	case preflight.Fail:
		return "Failed"
	case preflight.Skip:
		return "Skipped"
	}
	return string(s)
}

var systemFlashes = map[string]string{
	"checks-ran": "The checks ran. The results are below.",
	"job-ran":    "The job ran.",
}

const (
	docsURL          = "https://kazari.io/docs/"
	docsConfigURL    = "https://kazari.io/docs/configuration/"
	docsUpgradeURL   = "https://kazari.io/docs/ops/#upgrading"
	docsBackupURL    = "https://kazari.io/docs/ops/#backing-up-and-restoring"
	redactedSetting  = "***"
	generalSection   = "general"
	backupTimeLayout = "20060102-150405"
)

func (s *server) systemRoutes(mux *http.ServeMux) {
	s.view(mux, "GET /system", s.system)
	s.action(mux, "POST /system/health/run", s.runHealth)
	s.action(mux, "POST /system/jobs/{name}/run", s.runJob)
	s.action(mux, "POST /system/backup", s.downloadBackup)
}

// jobView is a job with what it does in plain words.
type jobView struct {
	scheduler.JobStatus
	About string
}

var jobAbout = map[string]string{
	"sweep":          "Moves stale pages to the bin when their grace ends, and empties the bin.",
	"gc":             "Deletes the files no page uses any more, and leftover temporary files.",
	"uploads-expire": "Drops uploads that were started and never finished.",
	"oauth-cleanup":  "Forgets expired sign-ins, tokens and applications that never connected.",
	"update-check":   "Asks GitHub for the newest Kazari release, once a day.",
	"views-flush":    "Saves the page views counted in the last minute.",
	"views-prune":    "Deletes daily view counts older than analytics.retention.",
}

// settingsGroup is one section of the configuration, flattened to dotted
// keys.
type settingsGroup struct {
	Name string
	Rows []settingRow
}

type settingRow struct {
	Key, Value string
	Secret     bool // set, and shown as ***
	Unset      bool // empty, null or an empty list
}

// sectionOrder is the order deploy/kazari.reference.yaml lists the sections
// in; a section it does not name follows, by name.
var sectionOrder = []string{"listen", "tls", "limits", "lifecycle", "serve", "fetch", "auth", "admin", "log", "telemetry"}

// settingsGroups turns the redacted configuration into groups of rows. The
// scalar keys at the top of the file form the first group.
func settingsGroups(cfg map[string]any) []settingsGroup {
	general := settingsGroup{Name: generalSection}
	sections := map[string]settingsGroup{}
	for key, v := range cfg {
		if m, ok := v.(map[string]any); ok {
			g := settingsGroup{Name: key}
			flattenSettings(&g.Rows, key, m)
			sortRows(g.Rows)
			sections[key] = g
			continue
		}
		general.Rows = append(general.Rows, settingOf(key, v))
	}
	sortRows(general.Rows)
	var out []settingsGroup
	if len(general.Rows) > 0 {
		out = append(out, general)
	}
	for _, name := range sectionOrder {
		if g, ok := sections[name]; ok {
			out = append(out, g)
			delete(sections, name)
		}
	}
	rest := make([]string, 0, len(sections))
	for name := range sections {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, sections[name])
	}
	return out
}

func sortRows(rows []settingRow) {
	slices.SortFunc(rows, func(a, b settingRow) int { return strings.Compare(a.Key, b.Key) })
}

func flattenSettings(rows *[]settingRow, prefix string, m map[string]any) {
	for key, v := range m {
		full := prefix + "." + key
		if sub, ok := v.(map[string]any); ok {
			if len(sub) == 0 {
				*rows = append(*rows, settingRow{Key: full, Unset: true})
				continue
			}
			flattenSettings(rows, full, sub)
			continue
		}
		*rows = append(*rows, settingOf(full, v))
	}
}

func settingOf(key string, v any) settingRow {
	row := settingRow{Key: key}
	switch x := v.(type) {
	case nil:
		row.Unset = true
	case string:
		row.Value, row.Unset, row.Secret = x, x == "", x == redactedSetting
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, fmt.Sprint(item))
		}
		row.Value, row.Unset = strings.Join(parts, ", "), len(x) == 0
	default:
		row.Value = fmt.Sprint(x)
	}
	return row
}

// updateView is what the update check knows, in the words of the page.
type updateView struct {
	Off       bool // no checker is wired: admin.update_check is off
	Unknown   bool // no check has answered, or this build has no version to compare
	Newer     bool
	Latest    string
	CheckedAt time.Time
}

func (s *server) updateView() updateView {
	if s.d.Updates == nil {
		return updateView{Off: true}
	}
	return updateOf(s.d.Updates.Result())
}

func updateOf(r update.Result) updateView {
	if r.CheckedAt.IsZero() {
		return updateView{Unknown: true}
	}
	return updateView{Newer: r.Newer, Latest: r.Latest, CheckedAt: r.CheckedAt}
}

type backupView struct {
	// FileBytes is what the pages' files take on disk, the bulk of the
	// archive. It is the database's own total: measuring the blob
	// directory walks every file, which a page view must not do.
	FileBytes  int64
	Last       *audit.Event
	BaseDomain string
}

type systemData struct {
	HasReady   bool
	Report     *preflight.Report // nil until a run has finished
	Failed     bool
	Jobs       []jobView
	HasJobs    bool
	Settings   []settingsGroup
	Build      buildinfo.Info
	Started    time.Time
	DataDir    string
	MCPURL     string
	TLSMode    string
	Update     updateView
	Backup     *backupView // nil without a backup service
	Diagnostic string
	// The addresses the page links to.
	DocsURL, ConfigURL, UpgradeURL, BackupDocsURL, ReleasesURL string
}

func (s *server) system(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	d := systemData{
		Build: s.o.Build, Started: s.o.Started, DataDir: s.o.DataDir, MCPURL: s.o.MCPURL, TLSMode: s.o.TLSMode,
		Update:  s.updateView(),
		DocsURL: docsURL, ConfigURL: docsConfigURL, UpgradeURL: docsUpgradeURL, BackupDocsURL: docsBackupURL, ReleasesURL: releasesURL,
	}
	if s.d.Ready != nil {
		d.HasReady = true
		if rep, ok := s.d.Ready.Last(); ok {
			d.Report, d.Failed = &rep, rep.Failed()
		}
	}
	if s.d.Jobs != nil {
		d.HasJobs = true
		for _, j := range s.d.Jobs.Status() {
			d.Jobs = append(d.Jobs, jobView{JobStatus: j, About: jobAbout[j.Name]})
		}
	}
	var cfg map[string]any
	if s.d.Settings != nil {
		cfg = s.d.Settings()
		d.Settings = settingsGroups(cfg)
	}
	if s.d.Backup != nil {
		_, blobs, err := s.d.Artifacts.Storage(ctx)
		if err != nil {
			return err
		}
		b := &backupView{BaseDomain: s.o.BaseDomain, FileBytes: blobs.Bytes}
		if b.Last, err = s.lastBackup(ctx); err != nil {
			return err
		}
		d.Backup = b
	}
	d.Diagnostic = diagnostics(s.o.Build, s.o.TLSMode, s.o.Started, d.Report, d.Settings)
	return s.render(w, r, c, http.StatusOK, "system", page{Title: "System", Nav: "system"}, d)
}

// lastBackup returns the newest recorded backup, nil when there is none.
func (s *server) lastBackup(ctx context.Context) (*audit.Event, error) {
	p, err := s.d.Audit.List(ctx, audit.Query{Actions: []string{audit.BackupCreate}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(p.Events) == 0 {
		return nil, nil
	}
	return &p.Events[0], nil
}

// diagnostics is the text an admin copies for whoever is asked for help. It
// is built from the redacted settings and holds nothing else of the
// configuration, so it carries no secret.
func diagnostics(b buildinfo.Info, tlsMode string, started time.Time, rep *preflight.Report, settings []settingsGroup) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "kazari %s", b.Version)
	if b.Commit != "" {
		fmt.Fprintf(&sb, " (%s)", b.Commit)
	}
	fmt.Fprintf(&sb, "\ngo: %s\ntls: %s\n", b.Go, tlsMode)
	if !started.IsZero() {
		fmt.Fprintf(&sb, "started: %s\n", started.UTC().Format(time.RFC3339))
	}
	sb.WriteString("\nchecks")
	if rep == nil {
		sb.WriteString(": not run yet\n")
	} else {
		fmt.Fprintf(&sb, " (%s):\n", rep.At.UTC().Format(time.RFC3339))
		for _, f := range rep.Findings {
			fmt.Fprintf(&sb, "  %-12s %-5s %s\n", f.Check, f.Status, f.Detail)
		}
	}
	if len(settings) > 0 {
		sb.WriteString("\nsettings:\n")
		for _, g := range settings {
			for _, row := range g.Rows {
				fmt.Fprintf(&sb, "  %s: %s\n", row.Key, row.Value)
			}
		}
	}
	return sb.String()
}

// runHealth runs the checks now. A run takes as long as the slowest check,
// which asks the DNS provider.
func (s *server) runHealth(w http.ResponseWriter, r *http.Request, _ *request) error {
	if s.d.Ready == nil {
		return notFound("This instance runs no health checks.")
	}
	if _, err := s.d.Ready.Check(r.Context(), true); err != nil {
		return err
	}
	return s.done(w, r, "/system#health", "checks-ran")
}

func (s *server) runJob(w http.ResponseWriter, r *http.Request, _ *request) error {
	if s.d.Jobs == nil {
		return notFound("This instance runs no jobs.")
	}
	name := r.PathValue("name")
	err := s.d.Jobs.Trigger(r.Context(), name)
	switch {
	case errors.Is(err, scheduler.ErrUnknownJob):
		return notFound("There is no job with that name.")
	case errors.Is(err, scheduler.ErrRunning):
		return &problem{status: http.StatusConflict, title: "The job is already running",
			message: "Wait for the run in progress to finish, then look at its result on the System page."}
	case err != nil:
		// The scheduler has logged it and keeps it as the job's last error.
		return &problem{status: http.StatusInternalServerError, title: "The job failed",
			message: "The run ended with an error: " + err.Error() + ". It is kept as the job's last error on the System page, and the job runs again on its schedule."}
	}
	return s.done(w, r, "/system#jobs", "job-ran")
}

// downloadBackup streams a backup as a download. It is the one POST that
// answers with a body: the archive cannot wait behind a redirect.
func (s *server) downloadBackup(w http.ResponseWriter, r *http.Request, _ *request) error {
	if s.d.Backup == nil {
		return notFound("This instance has no built-in backup.")
	}
	ctx := r.Context()
	out := &backupDownload{w: w, out: middleware.Streaming(w, backupStall), name: "kazari-" + fileSafe(s.o.BaseDomain) + "-" + s.now().UTC().Format(backupTimeLayout) + ".tar"}
	// Never with the certificates: the private key stays on the server.
	_, err := s.d.Backup.Write(ctx, out, backup.WriteOptions{})
	if err == nil {
		out.start()
		return nil
	}
	if out.started {
		// The status is sent; only a broken transfer tells the browser the
		// file is not whole.
		s.log.ErrorContext(ctx, "dashboard: backup failed mid-transfer", "err", err)
		panic(http.ErrAbortHandler)
	}
	switch {
	case errors.Is(err, backup.ErrRunning):
		return &problem{status: http.StatusConflict, code: api.CodeBackupRunning, title: "A backup is already running",
			message: "One backup is written at a time. Wait for the download in progress to finish, then try again."}
	case errors.Is(err, backup.ErrNoSpace):
		msg := "The server has too little free space for the temporary copy of the database. Free some space on the data disk, then try again."
		if est, eerr := s.d.Backup.Estimate(ctx); eerr == nil {
			msg = fmt.Sprintf("The copy of the database needs up to %s and the data disk has %s free. Free some space, then try again.",
				humanBytes(est.DatabaseBytes), humanBytes(est.FreeBytes))
		}
		return &problem{status: http.StatusInsufficientStorage, code: api.CodeNoSpace, title: "Not enough free space", message: msg}
	}
	return err
}

// backupDownload sends its headers with the first byte, so a backup that is
// refused before it writes anything can still be answered with a page.
type backupDownload struct {
	w       http.ResponseWriter
	out     io.Writer
	name    string
	started bool
}

// backupStall is how long the browser may stop reading before the download
// ends. A backup holds the one backup slot and keeps the collector waiting
// until it is over.
const backupStall = time.Minute

func (d *backupDownload) start() {
	if d.started {
		return
	}
	d.started = true
	h := d.w.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("Content-Disposition", `attachment; filename="`+d.name+`"`)
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	d.w.WriteHeader(http.StatusOK)
}

func (d *backupDownload) Write(p []byte) (int, error) {
	d.start()
	return d.out.Write(p)
}

// fileSafe keeps what a host name is made of, so the name needs no quoting.
func fileSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return -1
	}, strings.ToLower(s))
}
