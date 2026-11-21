package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/update"
)

func TestOverviewNeedsAttention(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	const gib = 1 << 30
	// An instance with nothing to report: two admins, room everywhere.
	calm := func() attentionInput {
		return attentionInput{
			Now:          now,
			Jobs:         []scheduler.JobStatus{{Name: "sweep", LastRun: &now}, {Name: "gc"}},
			CertNotAfter: now.Add(60 * 24 * time.Hour),
			DiskFree:     50 * gib, DiskTotal: 100 * gib,
			People:       []quotaUse{{Handle: "ana", Name: "Ana Sousa", Used: gib, Quota: 2 * gib}, {Handle: "ben", Used: 5 * gib}},
			ActiveAdmins: 2,
			Findings:     []preflight.Finding{{Check: "dns", Status: preflight.OK, Detail: "resolves"}, {Check: "dns token", Status: preflight.Skip, Detail: "proxy mode"}},
			Update:       update.Result{Latest: "0.8.0", CheckedAt: now},
			Version:      "0.8.0",
		}
	}
	tests := []struct {
		name   string
		mutate func(*attentionInput)
		// What the one item says; empty means the list is empty.
		tone, label, href string
		text              []string
	}{
		{name: "nothing", mutate: func(*attentionInput) {}},
		{name: "a failed job", mutate: func(in *attentionInput) { in.Jobs[1].LastError = "disk I/O error" },
			tone: "danger", label: "Job failed", href: "/system#jobs", text: []string{"gc", "failed", "disk space"}},
		{name: "a failed job nobody named", mutate: func(in *attentionInput) {
			in.Jobs = []scheduler.JobStatus{{Name: "custom", LastError: "boom"}}
		}, tone: "danger", label: "Job failed", href: "/system#jobs", text: []string{"custom", "on its schedule"}},
		{name: "certificate in 14 days", mutate: func(in *attentionInput) { in.CertNotAfter = now.Add(14 * 24 * time.Hour) },
			tone: "warn", label: "Certificate", href: "/system#health", text: []string{"expires in 14 days", "5 Dec 2026"}},
		{name: "certificate tomorrow", mutate: func(in *attentionInput) { in.CertNotAfter = now.Add(30 * time.Hour) },
			tone: "warn", label: "Certificate", href: "/system#health", text: []string{"expires in 1 day,"}},
		{name: "certificate within hours", mutate: func(in *attentionInput) { in.CertNotAfter = now.Add(3 * time.Hour) },
			tone: "warn", label: "Certificate", href: "/system#health", text: []string{"less than a day"}},
		{name: "certificate in 15 days", mutate: func(in *attentionInput) { in.CertNotAfter = now.Add(15 * 24 * time.Hour) }},
		{name: "certificate expired", mutate: func(in *attentionInput) { in.CertNotAfter = now.Add(-time.Hour) },
			tone: "danger", label: "Certificate expired", href: "/system#health", text: []string{"expired on 21 Nov 2026"}},
		{name: "no certificate served", mutate: func(in *attentionInput) { in.CertNotAfter = time.Time{} }},
		{name: "disk under 10% free", mutate: func(in *attentionInput) { in.DiskFree = 9 * gib },
			tone: "warn", label: "Disk space", href: "/pages/bin", text: []string{"9 GiB free of 100 GiB"}},
		{name: "disk at 10% free", mutate: func(in *attentionInput) { in.DiskFree = 10 * gib }},
		{name: "disk nearly full", mutate: func(in *attentionInput) { in.DiskFree = gib },
			tone: "danger", label: "Disk almost full", href: "/pages/bin", text: []string{"1 GiB free"}},
		{name: "disk size unknown", mutate: func(in *attentionInput) { in.DiskFree, in.DiskTotal = 0, 0 }},
		{name: "quota above 90%", mutate: func(in *attentionInput) { in.People[0].Used = 2*gib - gib/20 },
			tone: "warn", label: "Quota", href: "/people/ana", text: []string{"Ana Sousa has used 97% of a 2 GiB quota"}},
		{name: "quota at 90%", mutate: func(in *attentionInput) { in.People[0].Used, in.People[0].Quota = 90, 100 }},
		{name: "over quota, no name", mutate: func(in *attentionInput) { in.People[1].Quota = gib },
			tone: "warn", label: "Quota", href: "/people/ben", text: []string{"ben has used 500%"}},
		{name: "a single admin", mutate: func(in *attentionInput) { in.ActiveAdmins = 1 },
			tone: "note", label: "One admin", href: "/people", text: []string{"single admin", "nobody else can let them back in"}},
		{name: "a failed health check", mutate: func(in *attentionInput) {
			in.Findings[0] = preflight.Finding{Check: "dns", Status: preflight.Fail, Detail: "*.art.test does not resolve; add the wildcard record"}
		}, tone: "danger", label: "Health check failed", href: "/system#health", text: []string{`"dns" failed`, "*.art.test does not resolve; add the wildcard record."}},
		{name: "a health warning", mutate: func(in *attentionInput) {
			in.Findings[0] = preflight.Finding{Check: "dashboard", Status: preflight.Warn, Detail: "set admin.public_url"}
		}, tone: "warn", label: "Health check", href: "/system#health", text: []string{`"dashboard" has a warning`, "Set admin.public_url."}},
		// The certificate and the disk are measured live; a stale finding
		// about them is not a second item.
		{name: "stale certificate and disk findings", mutate: func(in *attentionInput) {
			in.Findings = []preflight.Finding{{Check: "certificate", Status: preflight.Fail, Detail: "expired"}, {Check: "disk", Status: preflight.Warn, Detail: "8% free"}}
		}},
		{name: "a newer version", mutate: func(in *attentionInput) { in.Update = update.Result{Latest: "0.8.1", Newer: true, CheckedAt: now} },
			tone: "note", label: "New version", href: "/system#about", text: []string{"Kazari 0.8.1 is out.", "This instance runs 0.8.0."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := calm()
			tt.mutate(&in)
			got := needsAttention(in)
			if tt.label == "" {
				if len(got) != 0 {
					t.Fatalf("needsAttention = %+v, want nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("needsAttention = %+v, want one item", got)
			}
			a := got[0]
			if a.Tone != tt.tone || a.Label != tt.label || a.Href != tt.href || a.Link == "" {
				t.Errorf("item = %+v, want tone %q, label %q, link to %q", a, tt.tone, tt.label, tt.href)
			}
			for _, want := range tt.text {
				if !strings.Contains(a.Text, want) {
					t.Errorf("text %q lacks %q", a.Text, want)
				}
			}
			if !strings.HasSuffix(a.Text, ".") || strings.Contains(a.Text, "!") {
				t.Errorf("text %q is not a plain sentence", a.Text)
			}
		})
	}

	// Everything at once comes in the order the page promises.
	in := calm()
	in.Jobs[0].LastError = "boom"
	in.CertNotAfter = now.Add(time.Hour)
	in.DiskFree = gib
	in.People[0].Used = 2 * gib
	in.ActiveAdmins = 1
	in.Findings[0].Status = preflight.Fail
	in.Update.Newer = true
	var labels []string
	for _, a := range needsAttention(in) {
		labels = append(labels, a.Label)
	}
	want := "Job failed|Certificate|Disk almost full|Quota|One admin|Health check failed|New version"
	if got := strings.Join(labels, "|"); got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
}

func TestOverviewCountPages(t *testing.T) {
	got := countPages([]artifact.StatusCount{
		{Status: artifact.StatusPublished, Visibility: artifact.Public, Count: 5},
		{Status: artifact.StatusPublished, Visibility: artifact.Members, Count: 2},
		{Status: artifact.StatusStale, Visibility: artifact.Unlisted, Count: 1},
		{Status: artifact.StatusUnpublished, Visibility: artifact.Public, Count: 3},
		{Status: artifact.StatusDeleted, Visibility: artifact.Public, Count: 4},
		{Status: artifact.StatusPurged, Visibility: artifact.Public, Count: 9},
	})
	want := pageCounts{Total: 15, Live: 7, Stale: 1, Unpublished: 3, Bin: 4, Public: 5, Unlisted: 1, Members: 2}
	if got != want {
		t.Errorf("countPages = %+v, want %+v", got, want)
	}
}
