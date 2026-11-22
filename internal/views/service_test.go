package views_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/views"
)

func TestServiceRange(t *testing.T) {
	fx := newFixture(t)
	svc := views.New(fx.store, views.Options{Retention: 30 * 24 * time.Hour, Now: fx.clock.Now})
	from, to, err := svc.Range(7)
	if err != nil {
		t.Fatalf("Range(7): %v", err)
	}
	today := views.Day(start)
	if !from.Equal(today.AddDate(0, 0, -6)) || !to.Equal(today.AddDate(0, 0, 1)) {
		t.Errorf("Range(7) = [%v, %v), want the six days before today and today", from, to)
	}
	for _, days := range []int{0, -1, 31} {
		if _, _, err := svc.Range(days); !errors.Is(err, artifact.ErrInvalidArgument) {
			t.Errorf("Range(%d) = %v, want ErrInvalidArgument", days, err)
		}
	}
	if got := svc.MaxRange(); got != 30 {
		t.Errorf("MaxRange() = %d, want 30", got)
	}
}

func TestServiceSummary(t *testing.T) {
	fx := newFixture(t, "demo", "other")
	rec := fx.recorder(views.RecorderOptions{})
	public := views.Access{Via: views.ViaPublic}
	rec.Record(pageRequest(), fx.hit("other", "index.html", public, "192.0.2.1"))
	fx.clock.Add(24 * time.Hour)
	rec.Record(pageRequest(), fx.hit("demo", "index.html", public, "192.0.2.1"))
	rec.Record(pageRequest(), fx.hit("demo", "index.html", public, "192.0.2.2"))
	flush(t, rec)

	got, err := fx.svc.Summary(t.Context(), 3)
	if err != nil {
		t.Fatalf("Summary(3): %v", err)
	}
	day := views.Day(start)
	want := &views.Summary{
		From: day.AddDate(0, 0, -1), To: day.AddDate(0, 0, 2),
		Days: []views.DayTotals{
			{Day: day.AddDate(0, 0, -1)},
			{Day: day, Views: 1, Visitors: 1},
			{Day: day.AddDate(0, 0, 1), Views: 2, Visitors: 2},
		},
		Totals: views.Totals{Views: 3, Visitors: 3},
		Top: []views.ArtifactTotals{
			{Slug: "demo", Views: 2, Visitors: 2},
			{Slug: "other", Views: 1, Visitors: 1},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Summary(3) (-want +got):\n%s", diff)
	}
}

func TestServiceReportUnknownSlug(t *testing.T) {
	fx := newFixture(t)
	if _, err := fx.svc.Report(t.Context(), "missing", 7); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("Report(missing) = %v, want ErrNotFound", err)
	}
}

func TestServicePrune(t *testing.T) {
	fx := newFixture(t, "demo")
	rec := fx.recorder(views.RecorderOptions{})
	rec.Record(pageRequest(), fx.hit("demo", "index.html", views.Access{Via: views.ViaPublic}, "192.0.2.1"))
	flush(t, rec)
	svc := views.New(fx.store, views.Options{Retention: 2 * 24 * time.Hour, Now: fx.clock.Now})

	fx.clock.Add(2 * 24 * time.Hour)
	if n, err := svc.Prune(t.Context()); err != nil || n != 0 {
		t.Errorf("Prune on the last day of retention = %d, %v; want 0 rows", n, err)
	}
	fx.clock.Add(24 * time.Hour)
	if n, err := svc.Prune(t.Context()); err != nil || n != 2 {
		t.Errorf("Prune past retention = %d, %v; want 2 rows", n, err)
	}
}
