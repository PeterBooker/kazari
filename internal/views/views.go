// Package views counts page views of artifacts: how many, by how many
// visitors a day, which files, and which members opened a members page. A
// Recorder counts in memory and a scheduled Flush adds the counts to a Store
// in daily buckets; a Service reads them back for the admin.
package views

import (
	"context"
	"time"
)

// Via is how a viewer was let in: the gate's result for a served page.
type Via string

const (
	ViaPublic    Via = "public"
	ViaMember    Via = "member"
	ViaShareLink Via = "share_link"
)

// Access is who the gate let in. Handle is the member's, set only for
// ViaMember.
type Access struct {
	Via    Via
	Handle string
}

// Hit is one served page.
type Hit struct {
	ArtifactID int64
	Path       string // the stored file served
	Access     Access
	// IP and UA tell visitors apart for the day; they are hashed in memory
	// and never stored.
	IP, UA string
}

// Day is the UTC day t falls on, as the start of that day.
func Day(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// DayCount is an artifact's counts for one day.
type DayCount struct {
	ArtifactID  int64
	Day         time.Time
	Views       int64
	Visitors    int64
	MemberViews int64
	LinkViews   int64
}

// PathCount is the views of one file of an artifact on one day.
type PathCount struct {
	ArtifactID int64
	Day        time.Time
	Path       string
	Views      int64
}

// ReaderCount is a member's views of an artifact.
type ReaderCount struct {
	ArtifactID      int64
	Handle          string
	FirstAt, LastAt time.Time
	Views           int64
}

// Batch is what a Flush adds: every count is added to what is stored.
type Batch struct {
	Days    []DayCount
	Paths   []PathCount
	Readers []ReaderCount
}

// Empty reports whether b adds nothing.
func (b Batch) Empty() bool { return len(b.Days) == 0 && len(b.Paths) == 0 && len(b.Readers) == 0 }

// Totals are counts summed over a range of days. Visitors is the sum of
// each day's visitors.
type Totals struct {
	Views, Visitors, MemberViews, LinkViews int64
}

// DayTotals are the counts of one day.
type DayTotals struct {
	Day time.Time
	Totals
}

// ArtifactTotals are one artifact's counts over a range.
type ArtifactTotals struct {
	Slug string
	Totals
}

// FileViews are the views of one file over a range.
type FileViews struct {
	Path  string
	Views int64
}

// Reader is a member who opened a members page.
type Reader struct {
	Handle, Name    string
	FirstAt, LastAt time.Time
	Views           int64
}

// Store keeps the counts. sqlite.ViewStore implements it. Ranges are days
// in [from, to). The methods for one slug return artifact.ErrNotFound for a
// slug that names no artifact.
type Store interface {
	AddViews(ctx context.Context, b Batch) error
	// PruneViews deletes the daily counts of days before before.
	PruneViews(ctx context.Context, before time.Time) (int64, error)

	InstanceDays(ctx context.Context, from, to time.Time) ([]DayTotals, error)
	TopArtifacts(ctx context.Context, from, to time.Time, limit int) ([]ArtifactTotals, error)
	ArtifactSums(ctx context.Context, slugs []string, from, to time.Time) (map[string]Totals, error)
	ArtifactDays(ctx context.Context, slug string, from, to time.Time) ([]DayTotals, error)
	TopPaths(ctx context.Context, slug string, from, to time.Time, limit int) ([]FileViews, error)
	Readers(ctx context.Context, slug string, limit int) ([]Reader, error)
}
