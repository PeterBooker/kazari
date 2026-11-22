package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/disk"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/user"
)

var overviewFuncs = template.FuncMap{
	"howmany": howMany,
}

// quotaHighPct is the share of a quota past which a person is listed. The
// certificate's and the disk's thresholds are the health checks' own, so
// the two never disagree.
const quotaHighPct = 90

func (s *server) overviewRoutes(mux *http.ServeMux) {
	s.view(mux, "GET /{$}", s.overview)
}

// attention is one thing the admin should look at: a word for what it is,
// a sentence that says it, and where the next step is.
type attention struct {
	Tone  string // danger, warn or note
	Label string
	Text  string
	Href  string
	Link  string
}

// quotaUse is what a person has used of their quota.
type quotaUse struct {
	Handle, Name string
	Used, Quota  int64
}

// attentionInput is everything the list is made from, as values, so the
// list is a function of what the instance looks like and of nothing else.
type attentionInput struct {
	Now          time.Time
	Jobs         []scheduler.JobStatus
	CertNotAfter time.Time // zero when Kazari serves no certificate
	DiskFree     uint64
	DiskTotal    uint64 // 0 when unknown
	People       []quotaUse
	ActiveAdmins int
	Findings     []preflight.Finding
	Update       update.Result
	Version      string
}

// needsAttention lists what is wrong or soon will be, the worst first within
// the order the plan gives: jobs, the certificate, the disk, quotas, the
// lone admin, the health checks, a newer version.
func needsAttention(in attentionInput) []attention {
	var out []attention
	for _, j := range in.Jobs {
		if j.LastError == "" {
			continue
		}
		out = append(out, attention{
			Tone: "danger", Label: "Job failed",
			Text: "The job " + j.Name + " failed on its last run. " + jobConsequence(j.Name),
			Href: "/system#jobs", Link: "See the error and run it again",
		})
	}
	if t := in.CertNotAfter; !t.IsZero() {
		switch left := t.Sub(in.Now); {
		case left <= 0:
			out = append(out, attention{
				Tone: "danger", Label: "Certificate expired",
				Text: "The certificate expired on " + t.UTC().Format("2 Jan 2006") + ". Browsers refuse every page until it is renewed.",
				Href: "/system#health", Link: "See the health checks",
			})
		case left < preflight.CertWarnWindow:
			out = append(out, attention{
				Tone: "warn", Label: "Certificate",
				Text: "The certificate expires in " + daysLeft(left) + ", on " + t.UTC().Format("2 Jan 2006") + ". It should have been renewed by now.",
				Href: "/system#health", Link: "See the health checks",
			})
		}
	}
	if in.DiskTotal > 0 {
		share := float64(in.DiskFree) / float64(in.DiskTotal)
		a := attention{
			Tone: "warn", Label: "Disk space",
			Text: fmt.Sprintf("The data disk has %s free of %s, under %d%%. Remove pages for good, or give the disk more room.",
				humanBytes(in.DiskFree), humanBytes(in.DiskTotal), int(preflight.DiskWarnFree*100)),
			Href: "/bin", Link: "Open the bin",
		}
		switch {
		case share < preflight.DiskFailFree:
			a.Tone, a.Label = "danger", "Disk almost full"
			out = append(out, a)
		case share < preflight.DiskWarnFree:
			out = append(out, a)
		}
	}
	for _, p := range in.People {
		if p.Quota <= 0 || p.Used*100 <= p.Quota*quotaHighPct {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Handle
		}
		out = append(out, attention{
			Tone: "warn", Label: "Quota",
			Text: fmt.Sprintf("%s has used %d%% of a %s quota. A large publish from them will be refused.",
				name, min(999, p.Used*100/p.Quota), humanBytes(p.Quota)),
			Href: "/people/" + url.PathEscape(p.Handle), Link: "Raise the quota or remove pages",
		})
	}
	if in.ActiveAdmins == 1 {
		out = append(out, attention{
			Tone: "note", Label: "One admin",
			Text: "This instance has a single admin. If they lose their sign-in, nobody else can let them back in without a terminal on the server.",
			Href: "/people", Link: "Make a second person an admin",
		})
	}
	for _, f := range in.Findings {
		// The certificate and the disk are measured above, at this moment.
		if f.Check == "certificate" || f.Check == "disk" {
			continue
		}
		switch f.Status {
		case preflight.Fail:
			out = append(out, attention{Tone: "danger", Label: "Health check failed",
				Text: "The check \"" + f.Check + "\" failed. " + asSentence(f.Detail), Href: "/system#health", Link: "See the health checks"})
		case preflight.Warn:
			out = append(out, attention{Tone: "warn", Label: "Health check",
				Text: "The check \"" + f.Check + "\" has a warning. " + asSentence(f.Detail), Href: "/system#health", Link: "See the health checks"})
		}
	}
	if in.Update.Newer {
		text := "Kazari " + in.Update.Latest + " is out."
		if in.Version != "" {
			text += " This instance runs " + in.Version + "."
		}
		out = append(out, attention{Tone: "note", Label: "New version", Text: text, Href: "/system#about", Link: "How to upgrade"})
	}
	return out
}

// jobConsequence says what stays undone while a job keeps failing.
func jobConsequence(name string) string {
	switch name {
	case "sweep":
		return "Stale pages are not moved to the bin and the bin is not emptied until it runs."
	case "gc":
		return "Removed pages keep their disk space until it runs."
	case "uploads-expire":
		return "Abandoned uploads keep their disk space until it runs."
	case "oauth-cleanup":
		return "Expired sign-ins and tokens stay in the database until it runs."
	case "update-check":
		return "The dashboard does not know whether a newer version exists."
	case "views-flush":
		return "Page views are held in memory, up to a limit, until it runs."
	case "views-prune":
		return "Old view counts stay in the database until it runs."
	}
	return "It runs again on its schedule."
}

func daysLeft(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	if n < 1 {
		return "less than a day"
	}
	return howMany(n, "day")
}

// howMany is "1 page" or "3 pages".
func howMany(n any, word string) string {
	var v int64
	switch x := n.(type) {
	case int:
		v = int64(x)
	case int64:
		v = x
	}
	if v == 1 {
		return "1 " + word
	}
	return strconv.FormatInt(v, 10) + " " + word + "s"
}

type peopleCounts struct {
	Total, Active, Invited, Disabled, Admins int
}

type pageCounts struct {
	Total, Live, Stale, Unpublished, Bin int64
	Public, Unlisted, Members            int64
}

type storageView struct {
	Counted  int64 // what quotas count: every version in full
	Stored   int64 // on disk, each file once
	Versions int64
	HasDisk  bool
	DiskFree uint64
	DiskSize uint64
	DiskUsed int // percent
}

type certView struct {
	Mode     string
	Proxy    bool
	NotAfter time.Time // zero when none is served
	Left     string
}

// startStep is one line of the getting-started list.
type startStep struct {
	Done bool
}

type overviewData struct {
	Attention []attention
	// New is an instance with no page yet, or nobody but the admin on it:
	// the getting-started list stands where the activity would.
	New                         bool
	Invited, Connected, Shipped startStep
	LocalAccounts               bool
	MCPURL, CLIURL              string
	People                      peopleCounts
	Pages                       pageCounts
	Storage                     storageView
	Cert                        certView
	Jobs                        []scheduler.JobStatus
	Views                       viewsPanel
	Events                      []audit.Event
	BaseDomain                  string
}

func (s *server) overview(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	now := s.now()
	d := overviewData{LocalAccounts: s.o.LocalAccounts, MCPURL: s.o.MCPURL, BaseDomain: s.o.BaseDomain,
		CLIURL: strings.TrimSuffix(strings.TrimSuffix(s.o.APIURL, "/"), "/v1")}
	in := attentionInput{Now: now, Version: s.o.Build.Version}

	users, err := s.d.Users.ListUsers(ctx)
	if err != nil {
		return err
	}
	accounts, err := s.d.Users.Accounts(ctx)
	if err != nil {
		return err
	}
	invited := make(map[string]bool, len(accounts))
	keyUsed := false
	// An admin who was invited and has set no password cannot let anyone
	// back in yet.
	canSignIn := 0
	for _, a := range accounts {
		invited[a.Handle] = a.Invited
		keyUsed = keyUsed || a.KeyUsedAt != nil
	}
	owners, err := s.d.Artifacts.OwnerUsage(ctx)
	if err != nil {
		return err
	}
	used := make(map[string]int64, len(owners))
	for _, o := range owners {
		used[o.Handle] = o.Bytes
	}
	for i := range users {
		u := &users[i]
		d.People.Total++
		switch {
		case u.DisabledAt != nil:
			d.People.Disabled++
		case invited[u.Handle]:
			d.People.Invited++
		default:
			d.People.Active++
		}
		if u.DisabledAt == nil {
			if u.Role == user.RoleAdmin {
				d.People.Admins++
				if !invited[u.Handle] {
					canSignIn++
				}
			}
			in.People = append(in.People, quotaUse{Handle: u.Handle, Name: u.DisplayName, Used: used[u.Handle], Quota: u.QuotaBytes})
		}
	}
	in.ActiveAdmins = canSignIn

	census, err := s.d.Artifacts.Census(ctx)
	if err != nil {
		return err
	}
	d.Pages = countPages(census)
	if d.Views, err = s.viewsPanel(ctx); err != nil {
		return err
	}
	versions, blobs, err := s.d.Artifacts.Storage(ctx)
	if err != nil {
		return err
	}
	d.Storage = storageView{Counted: versions.Bytes, Stored: blobs.Bytes, Versions: versions.Count}
	if free, total, err := s.diskUsage(); err == nil && total > 0 {
		in.DiskFree, in.DiskTotal = free, total
		d.Storage.HasDisk, d.Storage.DiskFree, d.Storage.DiskSize = true, free, total
		d.Storage.DiskUsed = int((total - min(free, total)) * 100 / total)
	}

	d.Cert = certView{Mode: s.o.TLSMode, Proxy: s.o.TLSMode == "proxy"}
	if s.o.CertNotAfter != nil {
		if t := s.o.CertNotAfter(); !t.IsZero() {
			in.CertNotAfter, d.Cert.NotAfter = t, t
			d.Cert.Left = "expired"
			if left := t.Sub(now); left > 0 {
				d.Cert.Left = daysLeft(left)
			}
		}
	}
	if s.d.Jobs != nil {
		d.Jobs = s.d.Jobs.Status()
		in.Jobs = d.Jobs
	}
	if s.d.Ready != nil {
		if rep, ok := s.d.Ready.Last(); ok {
			in.Findings = rep.Findings
		}
	}
	if s.d.Updates != nil {
		in.Update = s.d.Updates.Result()
	}
	d.Attention = needsAttention(in)

	recent, err := s.d.Audit.List(ctx, audit.Query{Limit: 10})
	if err != nil {
		return err
	}
	d.Events = recent.Events

	d.New = d.Pages.Total == 0 || d.People.Total <= 1
	if d.New {
		d.Invited.Done = d.People.Total > 1
		d.Shipped.Done = d.Pages.Total > 0
		d.Connected.Done = keyUsed || d.Shipped.Done || s.granted(ctx)
	}
	return s.render(w, r, c, http.StatusOK, "overview", page{Title: "Overview", Nav: "overview"}, d)
}

// granted reports whether anybody ever connected an application.
func (s *server) granted(ctx context.Context) bool {
	p, err := s.d.Audit.List(ctx, audit.Query{Actions: []string{"oauth.grant"}, Limit: 1})
	return err == nil && len(p.Events) > 0
}

func (s *server) diskUsage() (free, total uint64, err error) {
	if s.o.DataDir == "" {
		return 0, 0, fmt.Errorf("dashboard: no data directory")
	}
	if s.o.DiskUsage != nil {
		return s.o.DiskUsage(s.o.DataDir)
	}
	return disk.Usage(s.o.DataDir)
}

// countPages folds the census into what the page shows. A tombstone is no
// page any more and is left out.
func countPages(census []artifact.StatusCount) pageCounts {
	var p pageCounts
	for _, c := range census {
		switch c.Status {
		case artifact.StatusPurged:
			continue
		case artifact.StatusDeleted:
			p.Bin += c.Count
		case artifact.StatusUnpublished:
			p.Unpublished += c.Count
		case artifact.StatusStale:
			p.Stale += c.Count
		default:
			p.Live += c.Count
		}
		p.Total += c.Count
		// Who can open a page matters only while it is served.
		if c.Status == artifact.StatusStale || c.Status == artifact.StatusPublished {
			switch c.Visibility {
			case artifact.Public:
				p.Public += c.Count
			case artifact.Unlisted:
				p.Unlisted += c.Count
			case artifact.Members:
				p.Members += c.Count
			}
		}
	}
	return p
}
