package views

import (
	"context"
	"fmt"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// MaxDays is the longest range a report covers; Options.Retention lowers it.
const MaxDays = 366

// Limits of the lists in a report.
const (
	TopArtifacts = 10
	TopFiles     = 10
	MaxReaders   = 100
)

// Options configures a Service.
type Options struct {
	// Retention is how long daily counts are kept; Prune deletes older
	// ones. Zero keeps them for MaxDays.
	Retention time.Duration
	Now       func() time.Time // default time.Now
}

// Service reads the counts and prunes old ones.
type Service struct {
	store Store
	opts  Options
}

// New returns a Service over s.
func New(s Store, o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Retention <= 0 {
		o.Retention = MaxDays * 24 * time.Hour
	}
	return &Service{store: s, opts: o}
}

// Summary is the instance's views over a range of days.
type Summary struct {
	From, To time.Time // To is the day after the last one
	// Days has one entry per day of the range, oldest first, zero on days
	// without views.
	Days   []DayTotals
	Totals Totals
	Top    []ArtifactTotals
}

// Report is one artifact's views over a range of days.
type Report struct {
	Slug     string
	From, To time.Time
	Days     []DayTotals
	Totals   Totals
	Files    []FileViews
	Readers  []Reader
}

// MaxRange is the most days a report may cover: the retention, whole days.
func (s *Service) MaxRange() int {
	n := int(s.opts.Retention / (24 * time.Hour))
	return max(1, min(n, MaxDays))
}

// Range returns the last days days, today included.
func (s *Service) Range(days int) (from, to time.Time, err error) {
	if days < 1 || days > s.MaxRange() {
		return time.Time{}, time.Time{}, artifact.Errorf(artifact.ErrInvalidArgument, "days must be between 1 and %d", s.MaxRange())
	}
	to = Day(s.opts.Now()).AddDate(0, 0, 1)
	return to.AddDate(0, 0, -days), to, nil
}

// Summary returns the instance's views over the last days days.
func (s *Service) Summary(ctx context.Context, days int) (*Summary, error) {
	from, to, err := s.Range(days)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.InstanceDays(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("views: summary: %w", err)
	}
	top, err := s.store.TopArtifacts(ctx, from, to, TopArtifacts)
	if err != nil {
		return nil, fmt.Errorf("views: summary: %w", err)
	}
	sum := &Summary{From: from, To: to, Days: fill(rows, from, to), Top: top}
	sum.Totals = total(sum.Days)
	return sum, nil
}

// Report returns slug's views over the last days days.
func (s *Service) Report(ctx context.Context, slug string, days int) (*Report, error) {
	from, to, err := s.Range(days)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ArtifactDays(ctx, slug, from, to)
	if err != nil {
		return nil, fmt.Errorf("views: report %s: %w", slug, err)
	}
	files, err := s.store.TopPaths(ctx, slug, from, to, TopFiles)
	if err != nil {
		return nil, fmt.Errorf("views: report %s: %w", slug, err)
	}
	readers, err := s.store.Readers(ctx, slug, MaxReaders)
	if err != nil {
		return nil, fmt.Errorf("views: report %s: %w", slug, err)
	}
	r := &Report{Slug: slug, From: from, To: to, Days: fill(rows, from, to), Files: files, Readers: readers}
	r.Totals = total(r.Days)
	return r, nil
}

// Sums returns the totals of each of slugs over the last days days; a slug
// without views has none.
func (s *Service) Sums(ctx context.Context, slugs []string, days int) (map[string]Totals, error) {
	from, to, err := s.Range(days)
	if err != nil {
		return nil, err
	}
	if len(slugs) == 0 {
		return map[string]Totals{}, nil
	}
	m, err := s.store.ArtifactSums(ctx, slugs, from, to)
	if err != nil {
		return nil, fmt.Errorf("views: sums: %w", err)
	}
	return m, nil
}

// Prune deletes the daily counts older than the retention.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	before := Day(s.opts.Now()).Add(-s.opts.Retention)
	n, err := s.store.PruneViews(ctx, before)
	if err != nil {
		return n, fmt.Errorf("views: prune: %w", err)
	}
	return n, nil
}

// fill returns one entry per day in [from, to), taking rows' counts.
func fill(rows []DayTotals, from, to time.Time) []DayTotals {
	byDay := make(map[int64]Totals, len(rows))
	for _, r := range rows {
		byDay[Day(r.Day).UnixNano()] = r.Totals
	}
	var out []DayTotals
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		out = append(out, DayTotals{Day: d, Totals: byDay[d.UnixNano()]})
	}
	return out
}

func total(days []DayTotals) Totals {
	var t Totals
	for _, d := range days {
		t.Views += d.Views
		t.Visitors += d.Visitors
		t.MemberViews += d.MemberViews
		t.LinkViews += d.LinkViews
	}
	return t
}
