package views_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/views"
)

var start = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

// clock is a fixed clock the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	db    *sqlite.DB
	store *sqlite.ViewStore
	clock *clock
	svc   *views.Service
	ids   map[string]int64
}

// newFixture opens a database with the people alice and bob and a public
// artifact for each slug, owned by alice.
func newFixture(t *testing.T, slugs ...string) *fixture {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, h := range []string{"alice", "bob"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO user (handle, display_name, role, created_at) VALUES (?, ?, 'member', 0)`,
			h, "Name of "+h); err != nil {
			t.Fatalf("create user %s: %v", h, err)
		}
	}
	c := &clock{t: start}
	fx := &fixture{db: db, store: sqlite.NewViewStore(db), clock: c, ids: map[string]int64{}}
	fx.svc = views.New(fx.store, views.Options{Now: c.Now})
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

func (fx *fixture) recorder(o views.RecorderOptions) *views.Recorder {
	o.Now = fx.clock.Now
	return views.NewRecorder(fx.store, o)
}

func (fx *fixture) hit(slug, path string, a views.Access, ip string) views.Hit {
	return views.Hit{ArtifactID: fx.ids[slug], Path: path, Access: a, IP: ip, UA: firefox}
}

func (fx *fixture) report(t *testing.T, slug string, days int) *views.Report {
	t.Helper()
	r, err := fx.svc.Report(t.Context(), slug, days)
	if err != nil {
		t.Fatalf("Report(%s, %d): %v", slug, days, err)
	}
	return r
}

func flush(t *testing.T, rec *views.Recorder) {
	t.Helper()
	if err := rec.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func TestRecorderCounts(t *testing.T) {
	fx := newFixture(t, "demo")
	rec := fx.recorder(views.RecorderOptions{Readers: true})
	public := views.Access{Via: views.ViaPublic}
	bob := views.Access{Via: views.ViaMember, Handle: "bob"}
	link := views.Access{Via: views.ViaShareLink}

	rec.Record(pageRequest(), fx.hit("demo", "index.html", public, "192.0.2.1"))
	rec.Record(pageRequest(), fx.hit("demo", "index.html", public, "192.0.2.1"))
	rec.Record(pageRequest(), fx.hit("demo", "about.html", bob, "192.0.2.2"))
	fx.clock.Add(time.Hour)
	rec.Record(pageRequest(), fx.hit("demo", "index.html", bob, "192.0.2.2"))
	rec.Record(pageRequest(), fx.hit("demo", "index.html", link, "192.0.2.3"))
	bot := pageRequest()
	bot.Header.Set("User-Agent", "Googlebot/2.1")
	rec.Record(bot, fx.hit("demo", "index.html", public, "192.0.2.4"))
	flush(t, rec)

	got := fx.report(t, "demo", 1)
	if want := (views.Totals{Views: 5, Visitors: 3, MemberViews: 2, LinkViews: 1}); got.Totals != want {
		t.Errorf("Report(demo).Totals = %+v, want %+v", got.Totals, want)
	}
	wantFiles := []views.FileViews{{Path: "index.html", Views: 4}, {Path: "about.html", Views: 1}}
	if diff := cmp.Diff(wantFiles, got.Files); diff != "" {
		t.Errorf("Report(demo).Files (-want +got):\n%s", diff)
	}
	wantReaders := []views.Reader{{Handle: "bob", Name: "Name of bob", FirstAt: start, LastAt: start.Add(time.Hour), Views: 2}}
	if diff := cmp.Diff(wantReaders, got.Readers); diff != "" {
		t.Errorf("Report(demo).Readers (-want +got):\n%s", diff)
	}
}

// A visitor is told apart for one day only: the next day the same visitor
// counts again, and nothing that identified them is stored.
func TestRecorderVisitorsPerDay(t *testing.T) {
	fx := newFixture(t, "demo")
	rec := fx.recorder(views.RecorderOptions{})
	h := fx.hit("demo", "index.html", views.Access{Via: views.ViaPublic}, "192.0.2.1")
	rec.Record(pageRequest(), h)
	rec.Record(pageRequest(), h)
	fx.clock.Add(24 * time.Hour)
	rec.Record(pageRequest(), h)
	flush(t, rec)

	got := fx.report(t, "demo", 2)
	want := []views.DayTotals{
		{Day: views.Day(start), Views: 2, Visitors: 1},
		{Day: views.Day(start).AddDate(0, 0, 1), Views: 1, Visitors: 1},
	}
	if diff := cmp.Diff(want, got.Days); diff != "" {
		t.Errorf("Report(demo).Days (-want +got):\n%s", diff)
	}
}

func TestRecorderReadersOff(t *testing.T) {
	fx := newFixture(t, "demo")
	rec := fx.recorder(views.RecorderOptions{Readers: false})
	rec.Record(pageRequest(), fx.hit("demo", "index.html", views.Access{Via: views.ViaMember, Handle: "bob"}, "192.0.2.1"))
	flush(t, rec)
	got := fx.report(t, "demo", 1)
	if got.Totals.MemberViews != 1 || len(got.Readers) != 0 {
		t.Errorf("Report(demo) with readers off = %+v member views and readers %+v, want 1 and none", got.Totals.MemberViews, got.Readers)
	}
}

func TestRecorderMaxPending(t *testing.T) {
	fx := newFixture(t, "demo", "other")
	// One artifact's day and file take two keys; the other's do not fit.
	rec := fx.recorder(views.RecorderOptions{MaxPending: 2})
	public := views.Access{Via: views.ViaPublic}
	rec.Record(pageRequest(), fx.hit("demo", "index.html", public, "192.0.2.1"))
	rec.Record(pageRequest(), fx.hit("other", "index.html", public, "192.0.2.1"))
	rec.Record(pageRequest(), fx.hit("demo", "index.html", public, "192.0.2.2"))
	flush(t, rec)
	if got := fx.report(t, "demo", 1).Totals.Views; got != 2 {
		t.Errorf("Report(demo).Totals.Views = %d, want 2", got)
	}
	if got := fx.report(t, "other", 1).Totals.Views; got != 0 {
		t.Errorf("Report(other).Totals.Views = %d, want 0: past MaxPending a new key is dropped", got)
	}
}

// failing is a Store whose AddViews fails while fail is set.
type failing struct {
	views.Store
	mu   sync.Mutex
	fail bool
}

func (f *failing) AddViews(ctx context.Context, b views.Batch) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("disk full")
	}
	return f.Store.AddViews(ctx, b)
}

func TestRecorderKeepsCountsWhenFlushFails(t *testing.T) {
	fx := newFixture(t, "demo")
	store := &failing{Store: fx.store, fail: true}
	rec := views.NewRecorder(store, views.RecorderOptions{Readers: true, Now: fx.clock.Now})
	bob := views.Access{Via: views.ViaMember, Handle: "bob"}
	rec.Record(pageRequest(), fx.hit("demo", "index.html", bob, "192.0.2.1"))
	if err := rec.Flush(t.Context()); err == nil {
		t.Fatal("Flush with a failing store succeeded")
	}
	fx.clock.Add(time.Minute)
	rec.Record(pageRequest(), fx.hit("demo", "index.html", bob, "192.0.2.1"))
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	flush(t, rec)

	got := fx.report(t, "demo", 1)
	if want := (views.Totals{Views: 2, Visitors: 1, MemberViews: 2}); got.Totals != want {
		t.Errorf("Report(demo).Totals = %+v, want %+v", got.Totals, want)
	}
	wantReaders := []views.Reader{{Handle: "bob", Name: "Name of bob", FirstAt: start, LastAt: start.Add(time.Minute), Views: 2}}
	if diff := cmp.Diff(wantReaders, got.Readers); diff != "" {
		t.Errorf("Report(demo).Readers (-want +got):\n%s", diff)
	}
}

func TestRecorderConcurrent(t *testing.T) {
	fx := newFixture(t, "demo")
	rec := fx.recorder(views.RecorderOptions{})
	h := fx.hit("demo", "index.html", views.Access{Via: views.ViaPublic}, "192.0.2.1")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				rec.Record(pageRequest(), h)
			}
		})
	}
	wg.Go(func() {
		for range 5 {
			if err := rec.Flush(context.WithoutCancel(t.Context())); err != nil {
				t.Errorf("Flush: %v", err)
			}
		}
	})
	wg.Wait()
	flush(t, rec)
	if got := fx.report(t, "demo", 1).Totals; got.Views != 400 || got.Visitors != 1 {
		t.Errorf("Report(demo).Totals = %+v, want 400 views by 1 visitor", got)
	}
}

// Counting a view that adds no key allocates nothing: it runs on every page
// a visitor opens.
func TestRecordAllocations(t *testing.T) {
	fx := newFixture(t, "demo")
	rec := fx.recorder(views.RecorderOptions{Readers: true})
	r := pageRequest()
	h := fx.hit("demo", "index.html", views.Access{Via: views.ViaMember, Handle: "bob"}, "192.0.2.1")
	rec.Record(r, h)
	if n := testing.AllocsPerRun(100, func() { rec.Record(r, h) }); n != 0 {
		t.Errorf("Record allocates %v times per view, want 0", n)
	}
	bot := pageRequest()
	bot.Header.Set("User-Agent", "Googlebot/2.1")
	if n := testing.AllocsPerRun(100, func() { rec.Record(bot, h) }); n != 0 {
		t.Errorf("Record allocates %v times per bot request, want 0", n)
	}
}

func BenchmarkRecord(b *testing.B) {
	rec := views.NewRecorder(nil, views.RecorderOptions{Readers: true})
	r := pageRequest()
	h := views.Hit{ArtifactID: 1, Path: "index.html", Access: views.Access{Via: views.ViaPublic}, IP: "192.0.2.1", UA: firefox}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec.Record(r, h)
		}
	})
}
