package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/views"
)

var viewDay0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return viewDay0.AddDate(0, 0, n) }

type viewFixture struct {
	db    *sqlite.DB
	store *sqlite.ViewStore
	ids   map[string]int64
}

func newViewFixture(t *testing.T, slugs ...string) *viewFixture {
	t.Helper()
	db := openArtifactTestDB(t)
	ctx := t.Context()
	for _, h := range []string{"alice", "bob"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO user (handle, display_name, role, created_at) VALUES (?, ?, 'member', 0)`,
			h, "Name of "+h); err != nil {
			t.Fatalf("create user %s: %v", h, err)
		}
	}
	fx := &viewFixture{db: db, store: sqlite.NewViewStore(db), ids: map[string]int64{}}
	for _, slug := range slugs {
		res, err := db.ExecContext(ctx,
			`INSERT INTO artifact (slug, owner_id, visibility, created_at, updated_at)
			 VALUES (?, (SELECT id FROM user WHERE handle = 'alice'), 'public', 0, 0)`, slug)
		if err != nil {
			t.Fatalf("create artifact %s: %v", slug, err)
		}
		if fx.ids[slug], err = res.LastInsertId(); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func (fx *viewFixture) add(t *testing.T, b views.Batch) {
	t.Helper()
	if err := fx.store.AddViews(t.Context(), b); err != nil {
		t.Fatalf("AddViews: %v", err)
	}
}

func totals(n, visitors, member, link int64) views.Totals {
	return views.Totals{Views: n, Visitors: visitors, MemberViews: member, LinkViews: link}
}

func TestViewStoreAddsUp(t *testing.T) {
	fx := newViewFixture(t, "demo", "other")
	ctx := t.Context()
	demo, other := fx.ids["demo"], fx.ids["other"]
	fx.add(t, views.Batch{
		Days: []views.DayCount{
			{ArtifactID: demo, Day: day(0), Views: 3, Visitors: 2, MemberViews: 1, LinkViews: 1},
			{ArtifactID: demo, Day: day(1), Views: 1, Visitors: 1},
			{ArtifactID: other, Day: day(1), Views: 5, Visitors: 5},
		},
		Paths: []views.PathCount{
			{ArtifactID: demo, Day: day(0), Path: "index.html", Views: 2},
			{ArtifactID: demo, Day: day(0), Path: "about.html", Views: 1},
			{ArtifactID: demo, Day: day(1), Path: "index.html", Views: 1},
		},
		Readers: []views.ReaderCount{
			{ArtifactID: demo, Handle: "bob", FirstAt: day(0).Add(time.Hour), LastAt: day(0).Add(2 * time.Hour), Views: 1},
			{ArtifactID: demo, Handle: "nobody", FirstAt: day(0), LastAt: day(0), Views: 1},
		},
	})
	fx.add(t, views.Batch{
		Days:  []views.DayCount{{ArtifactID: demo, Day: day(0), Views: 1, Visitors: 1, MemberViews: 1}},
		Paths: []views.PathCount{{ArtifactID: demo, Day: day(0), Path: "about.html", Views: 2}},
		Readers: []views.ReaderCount{
			{ArtifactID: demo, Handle: "bob", FirstAt: day(1), LastAt: day(1).Add(time.Hour), Views: 2},
		},
	})

	gotDays, err := fx.store.ArtifactDays(ctx, "demo", day(0), day(2))
	if err != nil {
		t.Fatal(err)
	}
	wantDays := []views.DayTotals{{Day: day(0), Totals: totals(4, 3, 2, 1)}, {Day: day(1), Totals: totals(1, 1, 0, 0)}}
	if diff := cmp.Diff(wantDays, gotDays); diff != "" {
		t.Errorf("ArtifactDays(demo) (-want +got):\n%s", diff)
	}

	gotInstance, err := fx.store.InstanceDays(ctx, day(1), day(2))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]views.DayTotals{{Day: day(1), Totals: totals(6, 6, 0, 0)}}, gotInstance); diff != "" {
		t.Errorf("InstanceDays(day 1) (-want +got):\n%s", diff)
	}

	gotTop, err := fx.store.TopArtifacts(ctx, day(0), day(2), 10)
	if err != nil {
		t.Fatal(err)
	}
	wantTop := []views.ArtifactTotals{{Slug: "demo", Totals: totals(5, 4, 2, 1)}, {Slug: "other", Totals: totals(5, 5, 0, 0)}}
	if diff := cmp.Diff(wantTop, gotTop); diff != "" {
		t.Errorf("TopArtifacts (-want +got):\n%s", diff)
	}

	gotSums, err := fx.store.ArtifactSums(ctx, []string{"demo", "missing"}, day(1), day(2))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]views.Totals{"demo": totals(1, 1, 0, 0)}, gotSums); diff != "" {
		t.Errorf("ArtifactSums (-want +got):\n%s", diff)
	}

	gotPaths, err := fx.store.TopPaths(ctx, "demo", day(0), day(2), 10)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []views.FileViews{{Path: "about.html", Views: 3}, {Path: "index.html", Views: 3}}
	if diff := cmp.Diff(wantPaths, gotPaths); diff != "" {
		t.Errorf("TopPaths(demo) (-want +got):\n%s", diff)
	}

	gotReaders, err := fx.store.Readers(ctx, "demo", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantReaders := []views.Reader{{Handle: "bob", Name: "Name of bob", FirstAt: day(0).Add(time.Hour), LastAt: day(1).Add(time.Hour), Views: 3}}
	if diff := cmp.Diff(wantReaders, gotReaders); diff != "" {
		t.Errorf("Readers(demo) (-want +got):\n%s", diff)
	}
}

func TestViewStoreUnknownSlug(t *testing.T) {
	fx := newViewFixture(t)
	ctx := t.Context()
	_, err := fx.store.ArtifactDays(ctx, "missing", day(0), day(1))
	wantNotFound(t, "ArtifactDays", err)
	_, err = fx.store.TopPaths(ctx, "missing", day(0), day(1), 10)
	wantNotFound(t, "TopPaths", err)
	_, err = fx.store.Readers(ctx, "missing", 10)
	wantNotFound(t, "Readers", err)
}

func wantNotFound(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("%s(missing) = %v, want artifact.ErrNotFound", name, err)
	}
}

func TestViewStorePrune(t *testing.T) {
	fx := newViewFixture(t, "demo")
	ctx := t.Context()
	id := fx.ids["demo"]
	fx.add(t, views.Batch{
		Days:    []views.DayCount{{ArtifactID: id, Day: day(0), Views: 1}, {ArtifactID: id, Day: day(1), Views: 2}},
		Paths:   []views.PathCount{{ArtifactID: id, Day: day(0), Path: "index.html", Views: 1}},
		Readers: []views.ReaderCount{{ArtifactID: id, Handle: "bob", FirstAt: day(0), LastAt: day(0), Views: 1}},
	})
	n, err := fx.store.PruneViews(ctx, day(1))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("PruneViews(day 1) = %d rows, want 2", n)
	}
	got, err := fx.store.ArtifactDays(ctx, "demo", day(0), day(2))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]views.DayTotals{{Day: day(1), Totals: totals(2, 0, 0, 0)}}, got); diff != "" {
		t.Errorf("ArtifactDays after prune (-want +got):\n%s", diff)
	}
	readers, err := fx.store.Readers(ctx, "demo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(readers) != 1 {
		t.Errorf("Readers after prune = %+v, want bob kept", readers)
	}
}

// A purged artifact keeps no counts, and a flush that was on its way when
// it was purged adds none.
func TestViewStorePurge(t *testing.T) {
	fx := newViewFixture(t, "demo")
	ctx := t.Context()
	id := fx.ids["demo"]
	b := views.Batch{
		Days:    []views.DayCount{{ArtifactID: id, Day: day(0), Views: 1}},
		Paths:   []views.PathCount{{ArtifactID: id, Day: day(0), Path: "index.html", Views: 1}},
		Readers: []views.ReaderCount{{ArtifactID: id, Handle: "bob", FirstAt: day(0), LastAt: day(0), Views: 1}},
	}
	fx.add(t, b)
	if err := sqlite.NewArtifactStore(fx.db).Purge(ctx, "demo", day(1), nil); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	fx.add(t, b)
	for _, table := range []string{"view_day", "view_path_day", "view_reader"} {
		var n int
		if err := fx.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after purge, want 0", table, n)
		}
	}
}
