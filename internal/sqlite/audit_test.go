package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/PeterBooker/kazari/internal/audit"
)

func auditTestStore(t *testing.T) *AuditStore {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "kazari.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAuditStore(db)
}

func TestAuditStoreRecordList(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 5, time.UTC)
	events := []audit.Event{
		{At: t0, Actor: "alice", Action: audit.ArtifactCreate, Slug: "demo", Details: map[string]string{"version": "1"}},
		{At: t0.Add(time.Minute), Actor: "bob", Action: audit.ArtifactUpdate, Slug: "demo"},
		{At: t0.Add(2 * time.Minute), Actor: "alice", Action: audit.ArtifactCreate, Slug: "other"},
		{At: t0.Add(3 * time.Minute), Actor: "scheduler", Action: audit.ArtifactStale, Slug: "demo",
			Details: map[string]string{"reason": `quoted "value"`, "b": "2"}},
	}
	for _, e := range events {
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		q    audit.Query
		want []int // indexes into events, newest first
	}{
		{"all", audit.Query{}, []int{3, 2, 1, 0}},
		{"slug", audit.Query{Slug: "demo"}, []int{3, 1, 0}},
		{"actor", audit.Query{Actor: "alice"}, []int{2, 0}},
		{"since inclusive", audit.Query{Since: t0.Add(time.Minute)}, []int{3, 2, 1}},
		{"combined", audit.Query{Since: t0.Add(time.Minute), Slug: "demo", Actor: "bob"}, []int{1}},
		{"no match", audit.Query{Actor: "nobody"}, nil},
		{"until exclusive", audit.Query{Until: t0.Add(2 * time.Minute)}, []int{1, 0}},
		{"since and until", audit.Query{Since: t0.Add(time.Minute), Until: t0.Add(3 * time.Minute)}, []int{2, 1}},
		{"action", audit.Query{Actions: []string{audit.ArtifactCreate}}, []int{2, 0}},
		{"actions", audit.Query{Actions: []string{audit.ArtifactUpdate, audit.ArtifactStale}}, []int{3, 1}},
		{"action prefix", audit.Query{Actions: []string{"artifact."}}, []int{3, 2, 1, 0}},
		{"action prefix reads no wildcard", audit.Query{Actions: []string{"artifact_"}, Slug: "demo"}, nil},
		{"a name is not a prefix", audit.Query{Actions: []string{"artifact"}}, nil},
		{"action and actor", audit.Query{Actions: []string{"artifact."}, Actor: "alice", Slug: "other"}, []int{2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := s.List(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var want []audit.Event
			for _, i := range tt.want {
				want = append(want, events[i])
			}
			if diff := cmp.Diff(want, page.Events, cmpopts.IgnoreFields(audit.Event{}, "ID")); diff != "" {
				t.Errorf("List (-want +got):\n%s", diff)
			}
			if page.NextCursor != "" {
				t.Errorf("NextCursor = %q on a single page", page.NextCursor)
			}
			for _, e := range page.Events {
				if e.ID == 0 {
					t.Errorf("event %s has no ID", e.Action)
				}
			}
		})
	}
}

func TestAuditStoreListPages(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	// i/3 gives groups of three the same time, to exercise the tie-break.
	for i := range 11 {
		action := audit.ArtifactUpdate
		if i%2 == 0 {
			action = audit.KeyIssue
		}
		e := audit.Event{At: t0.Add(time.Duration(i/3) * time.Minute), Actor: "alice", Action: action, Slug: "demo"}
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []audit.Query{{}, {Actions: []string{"key."}}, {Since: t0.Add(time.Minute), Until: t0.Add(3 * time.Minute)}} {
		q.Limit = 1000
		full, err := s.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if full.NextCursor != "" {
			t.Fatalf("full listing has cursor %q", full.NextCursor)
		}
		for _, limit := range []int{1, 2, 3, len(full.Events)} {
			var got []audit.Event
			q.Limit, q.Cursor = limit, ""
			for pages := 0; ; pages++ {
				if pages > 20 {
					t.Fatalf("limit %d: paging does not end", limit)
				}
				page, err := s.List(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Events) > limit {
					t.Fatalf("limit %d: page of %d", limit, len(page.Events))
				}
				got = append(got, page.Events...)
				if page.NextCursor == "" {
					break
				}
				q.Cursor = page.NextCursor
			}
			if diff := cmp.Diff(full.Events, got); diff != "" {
				t.Errorf("limit %d: pages differ from the full listing (-full +paged):\n%s", limit, diff)
			}
		}
	}
}

func TestAuditStoreListInvalid(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	for _, q := range []audit.Query{
		{Cursor: "not a cursor!"},
		{Cursor: "e30"},
		{Actions: []string{""}},
		{Actions: []string{"."}},
		{Actions: make([]string, audit.MaxActions+1)},
	} {
		if _, err := s.List(ctx, q); !errors.Is(err, audit.ErrInvalidQuery) {
			t.Errorf("List(%+v) = %v, want ErrInvalidQuery", q, err)
		}
	}
}

func TestAuditStoreRecordStampsTime(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	before := time.Now()
	if err := s.Record(ctx, audit.Event{Actor: "admin", Action: audit.UserCreate}); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(ctx, audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Events; len(got) != 1 || got[0].At.Before(before) || got[0].Details != nil {
		t.Fatalf("List = %+v, want one event stamped after %v with nil details", got, before)
	}
}

func TestAuditStoreListLimits(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Direct inserts: 1001 Record calls are slow under -race.
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for i := range 1001 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO audit_event (at, actor, action) VALUES (?, 'a', 'x')`,
				toNanos(t0.Add(time.Duration(i)*time.Second))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		limit int
		want  int
		more  bool
	}{
		{0, 100, true},
		{-1, 100, true},
		{5, 5, true},
		{1000, 1000, true},
		{5000, 1000, true},
	}
	for _, tt := range tests {
		page, err := s.List(ctx, audit.Query{Limit: tt.limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != tt.want || (page.NextCursor != "") != tt.more {
			t.Errorf("Limit %d: got %d events and cursor %q, want %d and more=%v",
				tt.limit, len(page.Events), page.NextCursor, tt.want, tt.more)
		}
	}
}

func TestAuditStoreListSubject(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	events := []audit.Event{
		{At: t0, Actor: "bob", Action: audit.KeyIssue, Subject: "bob"},
		{At: t0.Add(time.Minute), Actor: "alice", Action: audit.InviteCreate, Subject: "bob"},
		{At: t0.Add(2 * time.Minute), Actor: "bob", Action: audit.KeyIssue, Subject: "carol"},
		{At: t0.Add(3 * time.Minute), Actor: "bob", Action: audit.ArtifactCreate, Slug: "demo"},
	}
	for _, e := range events {
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(ctx, audit.Query{Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	want := []audit.Event{events[1], events[0]}
	if diff := cmp.Diff(want, page.Events, cmpopts.IgnoreFields(audit.Event{}, "ID")); diff != "" {
		t.Errorf("List by subject (-want +got):\n%s", diff)
	}
}
